package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	v1 "github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/classification"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type deploymentObservation struct {
	Metadata     meta.ObjectMeta `json:"metadata"`
	TemplateHash string          `json:"templateHash"`
	SpecHash     string          `json:"specHash"`
	Attempt      string          `json:"attempt"`
	Hostname     string          `json:"hostname"`
	Fence        *StopFence      `json:"fence,omitempty"`
}
type observer struct {
	selectedPre       *observerDeploymentRead
	preCandidates     []observerDeploymentRead
	firstDeployment   *observerDeploymentRead
	desiredReads      []observerDeploymentRead
	deployments       []observerDeploymentRead
	stops             []observerStopRead
	c                 ObserverConfig
	r                 *observerRecorder
	api, service      *http.Client
	apiGuard          *observerGuard
	evidence          RuntimeEvidence
	incident          v1.RecoveryIncident
	pods              PodList
	initialPolicy     *v1.RecoveryPolicy
	initialDeployment *deploymentObservation
	lastPolicy        *v1.RecoveryPolicy
	lastDeployment    *deploymentObservation
	receipts          ObserverReceipts
	probes            []ProbeRecord
	readiness         *readinessObserverState
}

func runObserve(args []string, w io.Writer, clock observerClock) error {
	f := flag.NewFlagSet("observe", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	path := f.String("config", "", "explicit observer JSON config")
	if e := f.Parse(args); e != nil {
		return errors.New("observe: invalid flags")
	}
	if *path == "" || f.NArg() != 0 {
		return errors.New("usage: demo-driver observe -config <file>")
	}
	b, e := observerLocalFile(*path, 1<<20)
	if e != nil {
		return e
	}
	var c ObserverConfig
	if strictDecode(b, &c, true) != nil {
		return errors.New("observe: invalid configuration")
	}
	if e = c.validate(); e != nil {
		return e
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	return collectObserver(ctx, c, w, clock)
}
func collectObserver(parent context.Context, c ObserverConfig, w io.Writer, clock observerClock) (err error) {
	if err = c.validate(); err != nil {
		return err
	}
	token := ""
	if c.BearerFile != "" {
		b, e := observerLocalFile(c.BearerFile, 16<<10)
		if e != nil {
			return e
		}
		token = strings.TrimSpace(string(b))
		if !observerBearer.MatchString(token) {
			return errors.New("observer: invalid bearer file")
		}
	}
	if token != "" {
		configBytes, _ := json.Marshal(c)
		if bytes.Contains(configBytes, []byte(token)) {
			return errors.New("observer: credential in configuration blocked")
		}
	}
	api, g, e := observerClient(c.APIOrigin, c.APICAFile, token, time.Duration(c.TimeoutMS)*time.Millisecond)
	if e != nil {
		return e
	}
	su, _ := url.Parse(c.ServiceURL)
	svc, sg, e := observerClient(su.Scheme+"://"+su.Host, c.ServiceCAFile, "", time.Duration(c.TimeoutMS)*time.Millisecond)
	if e != nil {
		return e
	}
	sg.allowed[su.RequestURI()] = true
	r, e := newObserverRecorder(c, clock)
	if e != nil {
		return e
	}
	r.forbidden = []byte(token)
	o := &observer{c: c, r: r, api: api, service: svc, apiGuard: g, evidence: RuntimeEvidence{Version: evidenceVersion, EvidenceKind: c.EvidenceKind, RunID: c.RunID}}
	if c.Readiness != nil {
		o.readiness = &readinessObserverState{stream: w}
	}
	defer api.CloseIdleConnections()
	defer svc.CloseIdleConnections()
	defer func() {
		if !r.finalized {
			r.issue("collection-interrupted-or-write-failed")
			_ = r.finish(false)
		}
	}()
	// The real context bounds even a stalled injected clock or transport. Virtual
	// clock deadlines are also checked in the loop for deterministic local tests.
	ctx, cancel := context.WithTimeout(parent, time.Duration(c.DeadlineMS)*time.Millisecond)
	defer cancel()
	start := clock.Now()
	for poll := 0; poll < 1000; poll++ {
		if ctx.Err() != nil {
			return errors.New("observer: collection interrupted/deadline during request")
		}
		if e = o.poll(ctx); e != nil {
			return e
		}
		if c.Readiness != nil {
			if e = o.pollReadiness(ctx); e != nil {
				return e
			}
		}
		if clock.Now().Sub(start)+time.Duration(c.IntervalMS)*time.Millisecond >= time.Duration(c.DeadlineMS)*time.Millisecond {
			break
		}
		if e = clock.Sleep(ctx, time.Duration(c.IntervalMS)*time.Millisecond); e != nil {
			return errors.New("observer: collection interrupted")
		}
	}
	if c.Readiness != nil {
		return o.finalizeReadiness(w)
	}
	return o.finalize(w)
}
func (o *observer) apiPath(resource, name string) string {
	prefix := "/api/v1/namespaces/" + o.c.Namespace + "/"
	switch resource {
	case "deployments", "replicasets":
		prefix = "/apis/apps/v1/namespaces/" + o.c.Namespace + "/"
	case "recoverypolicies", "recoveryincidents":
		prefix = "/apis/opsloop.io/v1alpha1/namespaces/" + o.c.Namespace + "/"
	case "nodes", "namespaces":
		prefix = "/api/v1/"
	case "endpointslices":
		prefix = "/apis/discovery.k8s.io/v1/namespaces/" + o.c.Namespace + "/"
	}
	if resource == "endpointslices" {
		return prefix + resource + "?" + url.Values{"labelSelector": {"kubernetes.io/service-name=" + o.c.Service}, "limit": {"100"}}.Encode()
	}
	return prefix + resource + "/" + name
}

// Only callers with a fixed resource and a validated target name can expand
// the exact-path allowlist. No URL or arbitrary API path is accepted from API data.
func (o *observer) get(ctx context.Context, resource, name string, v any) (Stamp, int, string, error) {
	if resource != "endpointslices" && !validObserverName(name) {
		at, err := o.r.stamp(ctx)
		if err != nil {
			return at, 0, "invalid-object-name", err
		}
		ref, err := o.r.raw(resource, resource, at, 0, "invalid-object-name", nil)
		o.r.diagnose(resource+":invalid-object-name", "gap", resource, at, ref, "apply")
		return at, 0, "invalid-object-name", err
	}
	path := o.apiPath(resource, name)
	o.apiGuard.allowed[path] = true
	b, status, problem := observerGET(ctx, o.api, o.c.APIOrigin+path, 1<<20)
	at, e := o.r.stamp(ctx)
	if e != nil {
		return at, status, problem, e
	}
	if problem == "" && strictDecode(b, v, false) != nil {
		problem = "api-json-invalid"
	}
	if problem != "" {
		ref, err := o.r.raw(resource, path, at, status, problem, nil)
		e = err
		class := "gap"
		// Only a named object's first 404 can mean it has not been created yet.
		// A failed LIST is an observation gap, even before any slice UID is pinned.
		if resource != "endpointslices" && status == 404 && o.r.manifest.Pins[path] == "" {
			class = "pending"
		}
		if resource == "pods" && status == 404 && o.c.Readiness != nil && o.c.Readiness.Mode == "runtime" && name == o.c.Readiness.SourcePod.Name && o.readinessApplied() {
			class = "pending"
		}
		o.r.diagnose(resource+":"+problem, class, path, at, ref, "apply")
	}
	return at, status, problem, e
}
func (o *observer) object(resource, name string, m meta.ObjectMeta, at Stamp) error {
	if o.c.Readiness != nil {
		if err := o.checkReadinessUID(resource, name, m); err != nil {
			return err
		}
	}
	reject := func(reason string) error {
		ref, e := o.r.raw("identity-rejected", o.apiPath(resource, name), at, 200, "target-identity-mismatch", projectionMeta(m))
		o.r.diagnose(resource+":"+reason, "fatal", o.apiPath(resource, name), at, ref, "collection")
		return e
	}
	if m.Name != name || (resource != "nodes" && resource != "namespaces" && m.Namespace != o.c.Namespace) {
		if e := reject("object-identity-mismatch"); e != nil {
			return e
		}
		return errors.New("observer: response target identity mismatch")
	}
	if e := o.r.pin(o.apiPath(resource, name), string(m.UID)); e != nil {
		if e := reject(e.Error()); e != nil {
			return e
		}
		return errors.New("observer: target UID missing or changed")
	}
	return nil
}
func projectionMeta(m meta.ObjectMeta) meta.ObjectMeta {
	return meta.ObjectMeta{Name: m.Name, Namespace: m.Namespace, UID: m.UID, ResourceVersion: m.ResourceVersion, Generation: m.Generation, OwnerReferences: m.OwnerReferences, DeletionTimestamp: m.DeletionTimestamp}
}
func projectDeployment(d apps.Deployment) deploymentObservation {
	p := deploymentObservation{Metadata: projectionMeta(d.ObjectMeta), TemplateHash: v1.TemplateHash(d.Spec.Template), SpecHash: v1.SpecHash(d.Spec), Attempt: d.Spec.Template.Annotations["opsloop.io/attempt-id"], Hostname: d.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"]}
	var f StopFence
	if json.Unmarshal([]byte(d.Annotations["opsloop.io/execution-fence"]), &f) == nil {
		p.Fence = &f
	}
	return p
}
func (o *observer) poll(ctx context.Context) error {
	if o.readiness != nil {
		o.readiness.policyFresh = false
		o.readiness.deploymentFresh = false
		o.readiness.serviceFresh = false
	}
	// Separate GETs, each carrying its own observed stamp and raw projection.
	// They are never described as an atomic snapshot or server commit.
	var policy v1.RecoveryPolicy
	pat, ps, problem, e := o.get(ctx, "recoverypolicies", o.c.Policy, &policy)
	if e != nil {
		return e
	}
	policyOK := problem == ""
	policyRef := ""
	if !policyOK {
		policyRef = o.r.contextRef
	}
	if policyOK {
		if e = o.object("recoverypolicies", o.c.Policy, policy.ObjectMeta, pat); e != nil {
			return e
		}
		if o.readiness != nil {
			o.readiness.policyFresh = true
		}
		policy.ObjectMeta = projectionMeta(policy.ObjectMeta)
		for i := range policy.Status.Conditions {
			cond := &policy.Status.Conditions[i]
			if cond.Type != "Quiesced" {
				cond.Message = ""
			} else {
				var f StopFence
				if json.Unmarshal([]byte(cond.Message), &f) != nil {
					cond.Message = ""
				} else {
					b, _ := json.Marshal(f)
					cond.Message = string(b)
				}
			}
		}
		if policyRef, e = o.r.raw("policy", o.apiPath("recoverypolicies", o.c.Policy), pat, ps, "", policy); e != nil {
			return e
		}
		if o.initialPolicy == nil {
			o.initialPolicy = policy.DeepCopy()
		}
		o.lastPolicy = &policy
	}
	var dep apps.Deployment
	dat, ds, problem, e := o.get(ctx, "deployments", o.c.Workload, &dep)
	if e != nil {
		return e
	}
	depOK := problem == ""
	var dp deploymentObservation
	depRef := ""
	if !depOK {
		depRef = o.r.contextRef
	}
	if depOK {
		if e = o.object("deployments", o.c.Workload, dep.ObjectMeta, dat); e != nil {
			return e
		}
		if o.readiness != nil {
			o.readiness.deploymentFresh = true
		}
		dp = projectDeployment(dep)
		depRef, e = o.r.raw("deployment-projection", o.apiPath("deployments", o.c.Workload), dat, ds, "", dp)
		if e != nil {
			return e
		}
		if o.initialDeployment == nil {
			d := dp
			o.initialDeployment = &d
			o.firstDeployment = &observerDeploymentRead{dp, dat, depRef}
		}
		if o.readiness != nil {
			o.readiness.deployment = dep.DeepCopy()
		}
		o.lastDeployment = &dp
		o.rememberDeployment(dp, dat, depRef)
	}
	var svc core.Service
	sat, ss, problem, e := o.get(ctx, "services", o.c.Service, &svc)
	if e != nil {
		return e
	}
	svcOK := problem == ""
	if svcOK {
		if e = o.object("services", o.c.Service, svc.ObjectMeta, sat); e != nil {
			return e
		}
		if o.readiness != nil {
			o.readiness.serviceFresh = true
		}
		if _, e = o.r.raw("service-identity", o.apiPath("services", o.c.Service), sat, ss, "", projectionMeta(svc.ObjectMeta)); e != nil {
			return e
		}
		o.evidence.Service = ServiceIdentity{Namespace: o.c.Namespace, Name: o.c.Service, UID: string(svc.UID), PathID: o.c.PathID, URL: o.c.ServiceURL}
	}
	if o.readiness != nil && (!policyOK || !depOK || !svcOK) {
		return errors.New("observer: required readiness API observation not fresh")
	}
	var inc v1.RecoveryIncident
	iat, is, problem, e := o.get(ctx, "recoveryincidents", o.c.Incident, &inc)
	if e != nil {
		return e
	}
	if problem == "" {
		if e = o.object("recoveryincidents", o.c.Incident, inc.ObjectMeta, iat); e != nil {
			return e
		}
		if o.c.Readiness != nil {
			if err := o.checkReadinessIncident(inc); err != nil {
				return err
			}
		}
		inc.ObjectMeta = projectionMeta(inc.ObjectMeta)
		containerBinding := ""
		if o.c.Readiness != nil && classification.IsPodReadinessTrigger(inc.Spec.Trigger) && inc.Spec.Trigger.PodUID == o.c.Readiness.SourcePod.UID {
			containerBinding = classification.ReadinessTriggerContainerHash(inc.Spec.Trigger)
		}
		inc.Spec.Trigger.Evidence = nil
		if validSHA256(containerBinding) {
			inc.Spec.Trigger.Evidence = []string{fmt.Sprintf("POD_NOT_READY podUID=%s previouslyReadyObserved=true containerIdentitySHA256=%s", inc.Spec.Trigger.PodUID, containerBinding)}
		}
		inc.Status.Runtime.Message = ""
		for i := range inc.Status.Runtime.Attempts {
			inc.Status.Runtime.Attempts[i].LastProbeError = ""
			for j := range inc.Status.Runtime.Attempts[i].Decision.Candidates {
				inc.Status.Runtime.Attempts[i].Decision.Candidates[j].Details = nil
			}
		}
		if inc.Status.Runtime.Decision != nil {
			for i := range inc.Status.Runtime.Decision.Candidates {
				inc.Status.Runtime.Decision.Candidates[i].Details = nil
			}
		}
		if _, e = o.r.raw("incident-projection", o.apiPath("recoveryincidents", o.c.Incident), iat, is, "", inc); e != nil {
			return e
		}
		o.incident = inc
		if o.initialPolicy != nil && o.initialDeployment != nil && (inc.Spec.PolicyRef.Name != o.c.Policy || inc.Spec.Workload.Name != o.c.Workload || o.initialPolicy.Spec.TargetRef.Name != o.c.Workload || inc.Spec.PolicyRef.UID != string(o.initialPolicy.UID) || inc.Spec.Workload.UID != string(o.initialDeployment.Metadata.UID) || inc.Spec.PolicyHash != v1.PolicyHash(o.initialPolicy.Spec) || inc.Spec.PolicyGeneration != o.initialPolicy.Generation) {
			o.r.issue("initial-policy-or-workload-link-mismatch")
		}
		if len(inc.Status.Runtime.Attempts) == 1 {
			a := inc.Status.Runtime.Attempts[0]
			prior := o.evidence.Identity
			if (prior.ActionID != "" && prior.ActionID != a.ActionID) || (prior.PodUID != "" && prior.PodUID != a.PodUID) || (prior.NodeUID != "" && prior.NodeUID != a.NodeUID) {
				o.r.issue("attempt-identity-changed")
				return errors.New("observer: attempt identity changed")
			}
			o.evidence.Identity = RunIdentity{PolicyUID: inc.Spec.PolicyRef.UID, PolicyGeneration: inc.Spec.PolicyGeneration, PolicyHash: inc.Spec.PolicyHash, IncidentUID: string(inc.UID), IncidentSequence: inc.Spec.Sequence, ActionID: a.ActionID, DeploymentUID: inc.Spec.Workload.UID, NodeUID: a.NodeUID, PodUID: a.PodUID}
			o.linkSourceTimes(a)
			if depOK && o.initialDeployment != nil && dp.TemplateHash == a.TargetTemplateHash && dp.Attempt == a.ActionID && dp.Hostname == a.Hostname && o.evidence.Apply.Ref == "" {
				pre := o.precondition(a, dat)
				if pre == nil {
					o.r.diagnose("pre-apply-observation-mismatch", "gap", o.apiPath("deployments", o.c.Workload), dat, depRef, "apply")
				} else {
					copy := *pre
					o.selectedPre = &copy
					o.evidence.Apply = ApplyProof{DeploymentUID: string(dp.Metadata.UID), PreRV: pre.Projection.Metadata.ResourceVersion, PostRV: dp.Metadata.ResourceVersion, PreTemplateHash: pre.Projection.TemplateHash, TargetTemplateHash: dp.TemplateHash, AttemptAnnotation: dp.Attempt, Hostname: dp.Hostname, Source: "fresh-get", Ref: depRef, Observed: dat}
				}
			}
			if depOK {
				o.checkDesired(a, dp, dat, depRef)
			}
			if a.PodName != "" && a.ReplicaSetName != "" {
				if e = o.attemptObjects(ctx, a, svcOK); e != nil {
					return e
				}
			}
		}
	}
	if e = o.observeStop(ctx, policy, dp, policyOK, depOK, pat, dat, policyRef, depRef); e != nil {
		return e
	}
	return o.probe(ctx)
}
func observerOwner(m meta.ObjectMeta, kind, uid string) bool {
	count := 0
	match := false
	for _, r := range m.OwnerReferences {
		if r.Controller != nil && *r.Controller {
			count++
			match = r.APIVersion == "apps/v1" && r.Kind == kind && string(r.UID) == uid
		}
	}
	return count == 1 && match
}
func (o *observer) attemptObjects(ctx context.Context, a v1.Attempt, svcOK bool) error {
	var pod core.Pod
	at, status, problem, e := o.get(ctx, "pods", a.PodName, &pod)
	if e != nil {
		return e
	}
	if problem != "" {
		return nil
	}
	if e = o.object("pods", a.PodName, pod.ObjectMeta, at); e != nil {
		return e
	}
	// Preserve the raw name/namespace/node/controller owner; discard env, volume,
	// Pod IP, arbitrary labels/annotations, logs and container state messages.
	var pl PodList
	b, _ := json.Marshal(map[string]any{"items": []core.Pod{pod}})
	if e = json.Unmarshal(b, &pl); e != nil {
		return errors.New("observer: pod projection failed")
	}
	ready := false
	var readyAt meta.Time
	for _, c := range pod.Status.Conditions {
		if c.Type == core.PodReady {
			ready = c.Status == core.ConditionTrue
			readyAt = c.LastTransitionTime
		}
	}
	if _, e = o.r.raw("pod-projection", o.apiPath("pods", a.PodName), at, status, "", struct {
		Pods    PodList   `json:"pods"`
		Ready   bool      `json:"ready"`
		ReadyAt meta.Time `json:"readyAt"`
	}{pl, ready, readyAt}); e != nil {
		return e
	}
	o.pods = pl
	linked := string(pod.UID) == a.PodUID && pod.Spec.NodeName == a.NodeName && observerOwner(pod.ObjectMeta, "ReplicaSet", a.ReplicaSetUID)
	if !linked {
		o.r.issue("pod-attempt-owner-link-incomplete")
	}
	var rs apps.ReplicaSet
	rat, rsStatus, problem, e := o.get(ctx, "replicasets", a.ReplicaSetName, &rs)
	if e != nil {
		return e
	}
	if problem != "" {
		return nil
	}
	if e = o.object("replicasets", a.ReplicaSetName, rs.ObjectMeta, rat); e != nil {
		return e
	}
	if _, e = o.r.raw("replicaset-owner", o.apiPath("replicasets", a.ReplicaSetName), rat, rsStatus, "", projectionMeta(rs.ObjectMeta)); e != nil {
		return e
	}
	if string(rs.UID) != a.ReplicaSetUID || !observerOwner(rs.ObjectMeta, "Deployment", o.evidence.Identity.DeploymentUID) {
		linked = false
		o.r.issue("replicaset-deployment-link-incomplete")
	}
	// Never follow a Node selected outside the configured expected target.
	nodeName := o.c.ExpectNode
	if o.c.Readiness != nil {
		if !o.c.Readiness.allowedNode(a.NodeName, a.NodeUID, true) {
			return errors.New("observer: candidate outside pinned readiness scope")
		}
		nodeName = a.NodeName
	}
	if a.NodeName != nodeName {
		o.r.issue("attempt-node-outside-approved-target")
		return nil
	}
	var node core.Node
	nat, ns, problem, e := o.get(ctx, "nodes", nodeName, &node)
	if e != nil {
		return e
	}
	if problem != "" {
		return nil
	}
	if e = o.object("nodes", nodeName, node.ObjectMeta, nat); e != nil {
		return e
	}
	nodeReady := false
	for _, c := range node.Status.Conditions {
		if c.Type == core.NodeReady {
			nodeReady = c.Status == core.ConditionTrue
		}
	}
	if _, e = o.r.raw("node-identity", o.apiPath("nodes", nodeName), nat, ns, "", struct {
		Metadata meta.ObjectMeta `json:"metadata"`
		Hostname string          `json:"hostname"`
		Ready    bool            `json:"ready"`
	}{projectionMeta(node.ObjectMeta), node.Labels["kubernetes.io/hostname"], nodeReady}); e != nil {
		return e
	}
	if string(node.UID) != a.NodeUID || node.Labels["kubernetes.io/hostname"] != a.Hostname || !nodeReady {
		linked = false
		o.r.issue("node-link-or-readiness-incomplete")
	}
	var eps discovery.EndpointSliceList
	eat, es, problem, e := o.get(ctx, "endpointslices", "", &eps)
	if e != nil {
		return e
	}
	if problem != "" {
		return nil
	}
	endpoint := false
	// Endpoint addresses and topology are not needed to prove targetRef membership.
	projected := []any{}
	endpointProblems := []string{}
	if eps.Continue != "" || len(eps.Items) > 100 {
		endpointProblems = append(endpointProblems, "endpointslice-list-truncated")
		linked = false
	}
	for _, slice := range eps.Items {
		if slice.Namespace != o.c.Namespace || slice.Labels[discovery.LabelServiceName] != o.c.Service {
			endpointProblems = append(endpointProblems, "endpointslice-scope-mismatch")
			linked = false
			continue
		}
		if e = o.r.pin("endpointslice/"+slice.Name, string(slice.UID)); e != nil {
			ref, err := o.r.raw("identity-rejected", o.apiPath("endpointslices", ""), eat, es, "target-uid-missing-or-replaced", projectionMeta(slice.ObjectMeta))
			o.r.diagnose("endpointslice-uid-missing-or-replaced", "fatal", o.apiPath("endpointslices", ""), eat, ref, "collection")
			if err != nil {
				return err
			}
			return errors.New("observer: EndpointSlice UID changed")
		}
		owners := slice.OwnerReferences
		owns := false
		for _, r := range owners {
			if r.Kind == "Service" && r.APIVersion == "v1" && string(r.UID) == o.evidence.Service.UID {
				owns = true
			}
		}
		for _, ep := range slice.Endpoints {
			projected = append(projected, struct {
				Slice      meta.ObjectMeta              `json:"slice"`
				Target     *core.ObjectReference        `json:"target"`
				Conditions discovery.EndpointConditions `json:"conditions"`
			}{projectionMeta(slice.ObjectMeta), ep.TargetRef, ep.Conditions})
			if owns && ep.TargetRef != nil && ep.TargetRef.Kind == "Pod" && ep.TargetRef.Namespace == o.c.Namespace && ep.TargetRef.Name == a.PodName && string(ep.TargetRef.UID) == a.PodUID && trueValue(ep.Conditions.Ready) {
				endpoint = true
			}
		}
	}
	if _, e = o.r.raw("endpointslice-targets", o.apiPath("endpointslices", ""), eat, es, strings.Join(endpointProblems, ","), projected); e != nil {
		return e
	}
	for _, reason := range endpointProblems {
		class := "contradiction"
		if reason == "endpointslice-list-truncated" {
			class = "gap"
		}
		o.r.diagnose(reason, class, o.apiPath("endpointslices", ""), eat, o.r.contextRef, "apply")
	}
	if !endpoint {
		class := "pending"
		if stampValid(o.evidence.ReadyObserved) {
			class = "contradiction"
		}
		o.r.diagnose("service-endpoint-link-incomplete", class, o.apiPath("endpointslices", ""), eat, o.r.contextRef, "apply")
	}
	if linked && endpoint && svcOK && ready && a.PodReadyAt != nil && a.PodReadyAt.Time.Equal(readyAt.Time) && o.evidence.Apply.Ref != "" && !stampValid(o.evidence.ReadyObserved) {
		o.evidence.ReadyObserved = at
	}
	return nil
}
func (o *observer) probe(ctx context.Context) error {
	start, e := o.r.stamp(ctx)
	if e != nil {
		return e
	}
	b, status, problem := observerGET(ctx, o.service, o.c.ServiceURL, maxBodyBytes)
	end, e := o.r.stamp(ctx)
	if e != nil {
		return e
	}
	p := ProbeRecord{T: start.At, LatencyMs: end.At.Sub(start.At).Milliseconds(), Status: status, PathID: o.c.PathID, URL: o.c.ServiceURL, Error: problem}
	result := "response"
	if ctx.Err() != nil {
		return errors.New("observer: probe cancelled; incomplete run")
	}
	if problem != "" {
		// Local parser/size/read failures and redirects do not manufacture outage.
		if (status >= 400 && strings.HasPrefix(problem, "http-")) || problem == "timeout-measurement-path" || problem == "transport-measurement-path" {
			result = "service-failure"
		} else {
			result = "recorder-error"
		}
	} else {
		var body struct{ Node, Pod, PodUID, Arch, Version, Namespace, Service string }
		if strictDecode(b, &body, false) != nil || (o.c.Readiness != nil && (body.Namespace != o.c.Namespace || body.Service != o.c.Service)) {
			p.Error = "response-identity-invalid"
			result = "recorder-error"
		} else {
			p.OK = true
			p.Node = body.Node
			p.Pod = body.Pod
			p.PodUID = body.PodUID
			p.Arch = body.Arch
			p.Version = body.Version
		}
	}
	timing := ProbeTiming{Start: start, End: end, Result: result}
	if _, e = o.r.raw("service-response", o.c.PathID, end, status, p.Error, struct {
		Record ProbeRecord `json:"record"`
		Timing ProbeTiming `json:"timing"`
	}{p, timing}); e != nil {
		return e
	}
	if result == "recorder-error" {
		o.r.diagnose("service-measurement:"+p.Error, "gap", o.c.PathID, end, o.r.contextRef, "apply")
	}
	o.probes = append(o.probes, p)
	o.evidence.Probe = append(o.evidence.Probe, timing)
	return nil
}
