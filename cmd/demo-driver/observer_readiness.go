package main

// The readiness profile extends the existing Observer. It performs observations
// only; all placement mutations remain in the existing Runtime Controller.
import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	v1 "github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/candidates"
	"github.com/SleepyMiny/OpsLoop/internal/classification"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ReadinessNode struct {
	Name         string `json:"name"`
	UID          string `json:"uid"`
	PodCIDR      string `json:"podCIDR"`
	Architecture string `json:"architecture"`
}
type ReadinessPlatform struct {
	Architecture   string `json:"architecture"`
	ManifestDigest string `json:"manifestDigest"`
	ConfigDigest   string `json:"configDigest"`
}
type ReadinessObserverConfig struct {
	Mode             string              `json:"mode"` // baseline|runtime; never a legacy runtime-v1 PASS override
	ClusterUID       string              `json:"clusterUID"`
	NamespaceUID     string              `json:"namespaceUID"`
	PolicyUID        string              `json:"policyUID"`
	PolicyGeneration int64               `json:"policyGeneration"`
	PolicyHash       string              `json:"policyHash"`
	DeploymentUID    string              `json:"deploymentUID"`
	ServiceUID       string              `json:"serviceUID"`
	SourcePod        FinishPod           `json:"sourcePod"`
	SourceNode       ReadinessNode       `json:"sourceNode"`
	Candidates       []ReadinessNode     `json:"candidates"`
	Platforms        []ReadinessPlatform `json:"platforms"`
	PrometheusURL    string              `json:"prometheusURL"`
	Export           bool                `json:"export"`
}

func (r ReadinessObserverConfig) validate(c ObserverConfig) error {
	if r.Mode != "baseline" && r.Mode != "runtime" {
		return errors.New("observer: explicit readiness collection mode required")
	}
	for _, uid := range []string{r.ClusterUID, r.NamespaceUID, r.PolicyUID, r.DeploymentUID, r.ServiceUID, r.SourcePod.UID, r.SourceNode.UID} {
		if uid == "" || len(uid) > 256 || strings.ContainsAny(uid, "\r\n") {
			return errors.New("observer: explicit readiness UID bindings required")
		}
	}
	if !validObserverName(r.SourcePod.Name) || !validObserverName(r.SourceNode.Name) || r.PolicyGeneration < 1 || !validSHA256(r.PolicyHash) || len(r.Candidates) != 2 || len(r.Platforms) != 2 {
		return errors.New("observer: invalid readiness target scope")
	}
	seen := map[string]bool{r.SourceNode.Name: true}
	for _, n := range append([]ReadinessNode{r.SourceNode}, r.Candidates...) {
		ip, _, err := net.ParseCIDR(n.PodCIDR)
		if err != nil || !ip.IsPrivate() || (n.Architecture != "amd64" && n.Architecture != "arm64") {
			return errors.New("observer: explicit private PodCIDR and architecture required")
		}
	}
	for _, n := range r.Candidates {
		if !validObserverName(n.Name) || n.UID == "" || seen[n.Name] {
			return errors.New("observer: duplicate or unbound readiness node")
		}
		seen[n.Name] = true
	}
	arches := map[string]bool{}
	for _, p := range r.Platforms {
		if (p.Architecture != "amd64" && p.Architecture != "arm64") || arches[p.Architecture] || !validDigest(p.ManifestDigest) || !validDigest(p.ConfigDigest) {
			return errors.New("observer: explicit amd64/arm64 image pins required")
		}
		arches[p.Architecture] = true
	}
	if r.Mode == "baseline" && c.ExpectNode != r.SourceNode.Name {
		return errors.New("observer: baseline must observe the pinned source node")
	}
	u, err := observerURL(c.ServiceURL, false)
	if err != nil || u.Scheme != "http" || u.Host != c.Service+"."+c.Namespace+".svc.cluster.local:8080" || u.Path != "/" {
		return errors.New("observer: readiness requires the exact direct ClusterIP Service authority")
	}
	if r.PrometheusURL != "http://opsloop-mon-kube-prometheu-prometheus.monitoring.svc:9090" {
		return errors.New("observer: unapproved Prometheus authority")
	}
	return nil
}
func validSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}
func validDigest(s string) bool {
	return strings.HasPrefix(s, "sha256:") && validSHA256(strings.TrimPrefix(s, "sha256:"))
}
func (r ReadinessObserverConfig) allowedNode(name, uid string, candidate bool) bool {
	if !candidate && r.SourceNode.Name == name && r.SourceNode.UID == uid {
		return true
	}
	return slices.ContainsFunc(r.Candidates, func(n ReadinessNode) bool { return n.Name == name && n.UID == uid })
}

type readinessSample struct {
	At                   Stamp        `json:"at"`
	SourcePresent        bool         `json:"sourcePresent"`
	SourceReady          bool         `json:"sourceReady"`
	EndpointReadyPodUIDs []string     `json:"endpointReadyPodUIDs"`
	Service              ProbeRecord  `json:"service"`
	Endpoint             *ProbeRecord `json:"directEndpoint,omitempty"`
}
type readinessObserverState struct {
	deployment          *apps.Deployment
	nodes               map[string]core.Node
	preview             *v1.RecoveryDecision
	samples             []readinessSample
	originalContainer   string
	initialTemplateHash string
	schedulerWitness    bool
	schedulerEvents     []readinessSchedulerEvent
	promBase            http.RoundTripper
	endpointBase        http.RoundTripper
	stream              io.Writer
	policyFresh         bool
	deploymentFresh     bool
	serviceFresh        bool
	actionPods          map[string]readinessPodBinding
}

func (o *observer) checkReadinessUID(resource, name string, m meta.ObjectMeta) error {
	r := o.c.Readiness
	expected := ""
	switch resource {
	case "namespaces":
		if name == "kube-system" {
			expected = r.ClusterUID
		} else if name == o.c.Namespace {
			expected = r.NamespaceUID
		}
	case "recoverypolicies":
		expected = r.PolicyUID
	case "deployments":
		expected = r.DeploymentUID
	case "services":
		expected = r.ServiceUID
	case "nodes":
		if !r.allowedNode(name, string(m.UID), false) {
			return errors.New("observer: Node outside pinned readiness scope")
		}
	case "pods":
		if name == r.SourcePod.Name {
			expected = r.SourcePod.UID
		}
	}
	if expected != "" && string(m.UID) != expected {
		return errors.New("observer: readiness object UID changed")
	}
	return nil
}
func podIsReady(p core.Pod) bool {
	return p.DeletionTimestamp == nil && p.Status.Phase == core.PodRunning && slices.ContainsFunc(p.Status.Conditions, func(c core.PodCondition) bool { return c.Type == core.PodReady && c.Status == core.ConditionTrue })
}
func (o *observer) readinessNode(ctx context.Context, name string) (core.Node, error) {
	var node core.Node
	at, status, problem, err := o.get(ctx, "nodes", name, &node)
	if err != nil {
		return node, err
	}
	if problem != "" {
		return node, errors.New("observer: readiness Node observation unavailable")
	}
	if err = o.object("nodes", name, node.ObjectMeta, at); err != nil {
		return node, err
	}
	for _, kind := range []core.NodeConditionType{core.NodeReady, core.NodeMemoryPressure, core.NodeDiskPressure, core.NodePIDPressure} {
		want := core.ConditionFalse
		if kind == core.NodeReady {
			want = core.ConditionTrue
		}
		if !slices.ContainsFunc(node.Status.Conditions, func(c core.NodeCondition) bool { return c.Type == kind && c.Status == want }) {
			return node, errors.New("observer: required Node condition unavailable or unsafe")
		}
	}
	bindings := append([]ReadinessNode{o.c.Readiness.SourceNode}, o.c.Readiness.Candidates...)
	for _, binding := range bindings {
		if binding.Name == name && (node.Spec.PodCIDR != binding.PodCIDR || node.Labels[core.LabelArchStable] != binding.Architecture) {
			return node, errors.New("observer: approved Node PodCIDR/architecture changed")
		}
	}
	projection := struct {
		Metadata meta.ObjectMeta `json:"metadata"`
		Spec     core.NodeSpec   `json:"spec"`
		Status   core.NodeStatus `json:"status"`
	}{projectionMeta(node.ObjectMeta), core.NodeSpec{Unschedulable: node.Spec.Unschedulable, Taints: node.Spec.Taints, PodCIDR: node.Spec.PodCIDR, PodCIDRs: node.Spec.PodCIDRs}, core.NodeStatus{Allocatable: node.Status.Allocatable, Capacity: node.Status.Capacity, Conditions: node.Status.Conditions, Addresses: node.Status.Addresses, NodeInfo: core.NodeSystemInfo{Architecture: node.Status.NodeInfo.Architecture, OperatingSystem: node.Status.NodeInfo.OperatingSystem}}}
	_, err = o.r.raw("readiness-node", o.apiPath("nodes", name), at, status, "", projection)
	return node, err
}
func (o *observer) pollReadiness(ctx context.Context) error {
	cfg := o.c.Readiness
	state := o.readiness
	if state == nil || !state.policyFresh || !state.deploymentFresh || !state.serviceFresh || o.lastPolicy == nil || state.deployment == nil || o.evidence.Service.UID == "" {
		return errors.New("observer: readiness policy/workload/service observation missing")
	}
	policy := o.lastPolicy
	if o.incident.UID != "" {
		if err := o.checkReadinessIncident(o.incident); err != nil {
			return err
		}
	}
	if state.nodes == nil {
		for _, target := range []struct{ name, uid string }{{"kube-system", cfg.ClusterUID}, {o.c.Namespace, cfg.NamespaceUID}} {
			var ns core.Namespace
			at, _, problem, err := o.get(ctx, "namespaces", target.name, &ns)
			if err != nil {
				return err
			}
			if problem != "" || string(ns.UID) != target.uid || ns.DeletionTimestamp != nil {
				return errors.New("observer: cluster/namespace readiness identity unavailable")
			}
			if err = o.object("namespaces", target.name, ns.ObjectMeta, at); err != nil {
				return err
			}
		}
		if policy.Generation != cfg.PolicyGeneration || v1.PolicyHash(policy.Spec) != cfg.PolicyHash {
			return errors.New("observer: initial readiness Policy generation/hash mismatch")
		}
		if !classification.PodReadinessEnabled(policy.Spec.Detection) || !slices.ContainsFunc(policy.Status.Conditions, func(c meta.Condition) bool {
			return c.Type == "ReadinessObserved" && c.Status == meta.ConditionTrue && c.ObservedGeneration == policy.Generation
		}) {
			return errors.New("observer: Controller has not observed the healthy source for this Policy generation")
		}
		if cfg.Mode == "baseline" && policy.Spec.Mode != v1.PolicyModeObserveOnly || cfg.Mode == "runtime" && policy.Spec.Mode != v1.PolicyModeEnforce {
			return errors.New("observer: readiness collection Policy mode mismatch")
		}
		state.nodes = map[string]core.Node{}
		for _, n := range append([]ReadinessNode{cfg.SourceNode}, cfg.Candidates...) {
			node, err := o.readinessNode(ctx, n.Name)
			if err != nil {
				return err
			}
			state.nodes[n.Name] = node
		}
		if issues := candidates.CheckWorkload(state.deployment); len(issues) != 0 {
			return errors.New("observer: workload outside production relocation constraints")
		}
		state.initialTemplateHash = v1.TemplateHash(state.deployment.Spec.Template)
		if state.deployment.Spec.Template.Spec.SchedulerName != "default-scheduler" || state.deployment.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] != cfg.SourceNode.Name {
			return errors.New("observer: initial scheduler/source selector mismatch")
		}
		containers := []struct {
			Name           string                    `json:"name"`
			Image          string                    `json:"image"`
			Resources      core.ResourceRequirements `json:"resources"`
			ReadinessProbe bool                      `json:"readinessProbe"`
		}{}
		for _, container := range state.deployment.Spec.Template.Spec.Containers {
			containers = append(containers, struct {
				Name           string                    `json:"name"`
				Image          string                    `json:"image"`
				Resources      core.ResourceRequirements `json:"resources"`
				ReadinessProbe bool                      `json:"readinessProbe"`
			}{container.Name, container.Image, container.Resources, container.ReadinessProbe != nil})
		}
		_, err := o.r.raw("readiness-workload-constraints", o.apiPath("deployments", o.c.Workload), o.firstDeployment.Observed, 200, "", struct {
			Metadata              meta.ObjectMeta         `json:"metadata"`
			TemplateHash          string                  `json:"templateHash"`
			Scheduler             string                  `json:"scheduler"`
			SourceSelector        map[string]string       `json:"sourceSelector"`
			Replicas              *int32                  `json:"replicas"`
			Strategy              apps.DeploymentStrategy `json:"strategy"`
			Containers            any                     `json:"containers"`
			Platforms             []string                `json:"requiredPlatforms"`
			PlacementNotYetTested bool                    `json:"placementNotYetTested"`
		}{projectionMeta(state.deployment.ObjectMeta), state.initialTemplateHash, state.deployment.Spec.Template.Spec.SchedulerName, state.deployment.Spec.Template.Spec.NodeSelector, state.deployment.Spec.Replicas, state.deployment.Spec.Strategy, containers, policy.Spec.RequiredPlatforms, true})
		if err != nil {
			return err
		}
		if err := o.readinessPreview(ctx); err != nil {
			return err
		}
	}
	if cfg.Mode == "baseline" && (policy.Spec.Mode != v1.PolicyModeObserveOnly || policy.Generation != cfg.PolicyGeneration || !policy.Spec.Enabled || policy.Status.ActiveIncident != nil) {
		return errors.New("observer: baseline Policy authority changed")
	}
	if cfg.Mode == "runtime" {
		liveSpec := policy.Spec.DeepCopy()
		liveSpec.Enabled = true
		if policy.Spec.Mode != v1.PolicyModeEnforce || v1.PolicyHash(*liveSpec) != cfg.PolicyHash || (policy.Generation != cfg.PolicyGeneration && !(policy.Generation == cfg.PolicyGeneration+1 && !policy.Spec.Enabled)) {
			return errors.New("observer: runtime Policy changed outside enabled-to-disabled stop scope")
		}
	}
	source := core.Pod{}
	at, status, problem, err := o.get(ctx, "pods", cfg.SourcePod.Name, &source)
	if err != nil {
		return err
	}
	sample := readinessSample{At: at, EndpointReadyPodUIDs: []string{}}
	if problem != "" {
		if status != 404 {
			return errors.New("observer: source Pod observation unknown")
		}
		if cfg.Mode == "baseline" || !o.readinessApplied() {
			return errors.New("observer: baseline source Pod missing")
		}
	} else {
		if err = o.object("pods", source.Name, source.ObjectMeta, at); err != nil {
			return err
		}
		owner := meta.GetControllerOf(&source)
		if owner == nil || source.Spec.NodeName != cfg.SourceNode.Name || !observerOwner(source.ObjectMeta, "ReplicaSet", string(owner.UID)) {
			return errors.New("observer: source node/owner changed")
		}
		var rs apps.ReplicaSet
		rat, _, rp, err := o.get(ctx, "replicasets", owner.Name, &rs)
		if err != nil {
			return err
		}
		if rp != "" || string(rs.UID) != string(owner.UID) || !observerOwner(rs.ObjectMeta, "Deployment", cfg.DeploymentUID) {
			return errors.New("observer: source Deployment owner chain unavailable")
		}
		template := rs.Spec.Template.DeepCopy()
		delete(template.Labels, apps.DefaultDeploymentUniqueLabelKey)
		if v1.TemplateHash(*template) != state.initialTemplateHash {
			return errors.New("observer: source ReplicaSet template changed")
		}
		if err = o.object("replicasets", rs.Name, rs.ObjectMeta, rat); err != nil {
			return err
		}
		sample.SourcePresent = true
		sample.SourceReady = podIsReady(source)
		incarnation := classification.ReadinessContainerHash(&source)
		if state.originalContainer == "" {
			if !sample.SourceReady || incarnation == "" {
				return errors.New("observer: fresh healthy source container required")
			}
			state.originalContainer = incarnation
		}
		if state.originalContainer != incarnation {
			return errors.New("observer: source container incarnation changed")
		}
		if cfg.Mode == "baseline" && !sample.SourceReady {
			return errors.New("observer: baseline source is not Ready")
		}
		_, err = o.r.raw("readiness-source", o.apiPath("pods", source.Name), at, status, "", struct {
			Metadata          meta.ObjectMeta `json:"metadata"`
			Node              string          `json:"node"`
			Ready             bool            `json:"ready"`
			ContainerIdentity string          `json:"containerIdentitySHA256"`
		}{projectionMeta(source.ObjectMeta), source.Spec.NodeName, sample.SourceReady, incarnation})
		if err != nil {
			return err
		}
	}
	var eps discovery.EndpointSliceList
	eat, es, ep, err := o.get(ctx, "endpointslices", "", &eps)
	if err != nil {
		return err
	}
	if ep != "" || eps.Continue != "" || len(eps.Items) > 100 {
		return errors.New("observer: EndpointSlice observation incomplete")
	}
	projected := []any{}
	for _, slice := range eps.Items {
		if slice.Namespace != o.c.Namespace || slice.Labels[discovery.LabelServiceName] != o.c.Service || !slices.ContainsFunc(slice.OwnerReferences, func(ref meta.OwnerReference) bool {
			return ref.APIVersion == "v1" && ref.Kind == "Service" && string(ref.UID) == cfg.ServiceUID
		}) {
			return errors.New("observer: EndpointSlice Service ownership mismatch")
		}
		if err = o.r.pin("endpointslice/"+slice.Name, string(slice.UID)); err != nil {
			return err
		}
		portOK := slices.ContainsFunc(slice.Ports, func(p discovery.EndpointPort) bool {
			return p.Port != nil && *p.Port == 8080 && p.Protocol != nil && *p.Protocol == core.ProtocolTCP
		})
		if !portOK {
			return errors.New("observer: EndpointSlice port mismatch")
		}
		for _, endpoint := range slice.Endpoints {
			projected = append(projected, struct {
				Slice    meta.ObjectMeta    `json:"slice"`
				Endpoint discovery.Endpoint `json:"endpoint"`
			}{projectionMeta(slice.ObjectMeta), endpoint})
			ref := endpoint.TargetRef
			if ref == nil || ref.Kind != "Pod" || ref.Namespace != o.c.Namespace || ref.Name == "" || ref.UID == "" {
				return errors.New("observer: Endpoint target identity incomplete")
			}
			allowed := string(ref.UID) == cfg.SourcePod.UID && ref.Name == cfg.SourcePod.Name
			if len(o.incident.Status.Runtime.Attempts) == 1 {
				a := o.incident.Status.Runtime.Attempts[0]
				allowed = allowed || (string(ref.UID) == a.PodUID && ref.Name == a.PodName)
			}
			if !allowed {
				if cfg.Mode != "runtime" || len(o.incident.Status.Runtime.Attempts) != 1 {
					return errors.New("observer: Endpoint points outside bound source/Controller attempt")
				}
				if _, err = o.resolveReadinessActionPod(ctx, ref, o.incident.Status.Runtime.Attempts[0]); err != nil {
					return err
				}
			}
			if trueValue(endpoint.Conditions.Ready) && !trueValue(endpoint.Conditions.Terminating) {
				sample.EndpointReadyPodUIDs = append(sample.EndpointReadyPodUIDs, string(ref.UID))
				var pod core.Pod
				pat, _, pp, err := o.get(ctx, "pods", ref.Name, &pod)
				if err != nil {
					return err
				}
				if pp != "" || string(pod.UID) != string(ref.UID) || !podIsReady(pod) {
					return errors.New("observer: ready Endpoint Pod is unavailable")
				}
				if err = o.object("pods", ref.Name, pod.ObjectMeta, pat); err != nil {
					return err
				}
				node, err := o.readinessNode(ctx, pod.Spec.NodeName)
				if err != nil {
					return err
				}
				endpointURL, err := readinessEndpointURL(pod, node, endpoint)
				if err != nil {
					return err
				}
				record, err := o.probeReadinessEndpoint(ctx, endpointURL, pod, node)
				if err != nil {
					return err
				}
				sample.Endpoint = &record
			}
		}
	}
	if _, err = o.r.raw("readiness-endpoints", o.apiPath("endpointslices", ""), eat, es, "", projected); err != nil {
		return err
	}
	if len(o.probes) == 0 {
		return errors.New("observer: direct Service measurement missing")
	}
	sample.Service = o.probes[len(o.probes)-1]
	if cfg.Mode == "baseline" && (!sample.Service.OK || sample.Service.PodUID != cfg.SourcePod.UID || !slices.Contains(sample.EndpointReadyPodUIDs, cfg.SourcePod.UID) || sample.Endpoint == nil || !sample.Endpoint.OK) {
		return errors.New("observer: direct baseline Service/Endpoint link unconfirmed")
	}
	if sample.Service.OK {
		expectedUID := cfg.SourcePod.UID
		nodeName := cfg.SourceNode.Name
		if len(o.incident.Status.Runtime.Attempts) == 1 {
			a := o.incident.Status.Runtime.Attempts[0]
			if sample.Service.PodUID == a.PodUID {
				expectedUID = a.PodUID
				nodeName = a.NodeName
			}
		}
		if binding, ok := state.actionPods[sample.Service.PodUID]; ok {
			expectedUID = binding.UID
			nodeName = binding.Node
		}
		node, ok := state.nodes[nodeName]
		if !ok || sample.Service.PodUID != expectedUID || sample.Service.Node != nodeName || sample.Service.Version != o.c.ExpectVersion || sample.Service.Arch != node.Labels[core.LabelArchStable] {
			return errors.New("observer: direct Service identity/platform mismatch")
		}
	}
	state.samples = append(state.samples, sample)
	if cfg.Mode == "runtime" && len(o.stops) > 0 {
		latest := o.stops[len(o.stops)-1]
		if o.validStop(latest.Proof) {
			o.evidence.Stop = latest.Proof
		}
	}
	_, err = o.r.raw("readiness-sample", o.c.PathID, at, 200, "", sample)
	if err != nil {
		return err
	}
	if len(o.incident.Status.Runtime.Attempts) == 1 && !state.schedulerWitness {
		if err = o.observeReadinessScheduler(ctx, o.incident.Status.Runtime.Attempts[0]); err != nil {
			return err
		}
	}
	return o.streamReadiness(ctx, sample)
}
func readinessEndpointURL(pod core.Pod, node core.Node, endpoint discovery.Endpoint) (string, error) {
	ip := net.ParseIP(pod.Status.PodIP)
	if ip == nil || !ip.IsPrivate() || ip.IsLoopback() || pod.DeletionTimestamp != nil || !slices.Contains(endpoint.Addresses, pod.Status.PodIP) || pod.Spec.NodeName != node.Name {
		return "", errors.New("observer: Endpoint/Pod IP or node mismatch")
	}
	cidrs := append([]string{node.Spec.PodCIDR}, node.Spec.PodCIDRs...)
	inside := false
	for _, cidr := range cidrs {
		_, network, err := net.ParseCIDR(cidr)
		if err == nil && network.Contains(ip) {
			inside = true
		}
	}
	if !inside {
		return "", errors.New("observer: Endpoint outside approved Node PodCIDR")
	}
	return "http://" + net.JoinHostPort(pod.Status.PodIP, "8080") + "/", nil
}
func (o *observer) probeReadinessEndpoint(ctx context.Context, raw string, pod core.Pod, node core.Node) (ProbeRecord, error) {
	u, _ := url.Parse(raw)
	client, g, err := observerClient(u.Scheme+"://"+u.Host, "", "", o.c.timeoutDuration())
	if err != nil {
		return ProbeRecord{}, err
	}
	defer client.CloseIdleConnections()
	if o.readiness.endpointBase != nil {
		g.base = o.readiness.endpointBase
	}
	g.allowed["/"] = true
	start, err := o.r.stamp(ctx)
	if err != nil {
		return ProbeRecord{}, err
	}
	body, status, problem := observerGET(ctx, client, raw, maxBodyBytes)
	end, err := o.r.stamp(ctx)
	if err != nil {
		return ProbeRecord{}, err
	}
	record := ProbeRecord{T: start.At, LatencyMs: end.At.Sub(start.At).Milliseconds(), Status: status, PathID: "direct-endpoint", URL: raw, Error: problem}
	if problem == "" {
		var response struct{ Node, Pod, PodUID, Namespace, Arch, Version string }
		if strictDecode(body, &response, false) != nil || response.PodUID != string(pod.UID) || response.Pod != pod.Name || response.Node != node.Name || response.Namespace != o.c.Namespace || response.Arch != node.Labels[core.LabelArchStable] || response.Version != o.c.ExpectVersion {
			return record, errors.New("observer: direct Endpoint response identity mismatch")
		}
		cfg := o.c.Readiness
		platform, ok := findReadinessPlatform(cfg.Platforms, response.Arch)
		if !ok || len(pod.Status.ContainerStatuses) != 1 || pod.Status.ContainerStatuses[0].ImageID != o.c.ImageRef || pod.Status.ContainerStatuses[0].Image != platform.ConfigDigest {
			return record, errors.New("observer: runtime image/platform config mismatch")
		}
		record.OK = true
		record.Node = response.Node
		record.Pod = response.Pod
		record.PodUID = response.PodUID
		record.Arch = response.Arch
		record.Version = response.Version
	}
	_, err = o.r.raw("readiness-endpoint-response", "direct-endpoint", end, status, problem, record)
	return record, err
}
func findReadinessPlatform(platforms []ReadinessPlatform, architecture string) (ReadinessPlatform, bool) {
	for _, p := range platforms {
		if p.Architecture == architecture {
			return p, true
		}
	}
	return ReadinessPlatform{}, false
}
func (c ObserverConfig) timeoutDuration() time.Duration {
	return time.Duration(c.TimeoutMS) * time.Millisecond
}
func (o *observer) finalizeReadiness(w io.Writer) error {
	state := o.readiness
	mode := o.c.Readiness.Mode
	if state == nil || len(state.samples) < 3 || state.preview == nil {
		return errors.New("observer: readiness collection incomplete")
	}
	status := "BASELINE_OBSERVED"
	if mode == "runtime" {
		status = "RUNTIME_CAPTURED_REQUIRES_FAULT_GUARD_FINISH_VERIFICATION"
	}
	summary := struct {
		Schema          string                         `json:"schema"`
		RunID           string                         `json:"runID"`
		EvidenceKind    string                         `json:"evidenceKind"`
		Status          string                         `json:"status"`
		Mode            string                         `json:"mode"`
		OverallPass     bool                           `json:"overallPass"`
		LiveProof       bool                           `json:"liveProof"`
		Preview         *v1.RecoveryDecision           `json:"readOnlyPreviewNotControllerDecision"`
		Samples         []readinessSample              `json:"samples"`
		Incident        v1.RecoveryIncident            `json:"incident"`
		Stop            *StopProof                     `json:"stop,omitempty"`
		Limits          []string                       `json:"limits"`
		SchedulerEvents []readinessSchedulerEvent      `json:"schedulerEvents"`
		ActionPods      map[string]readinessPodBinding `json:"independentlyObservedActionPods"`
	}{"opsloop-readiness-observer/v1", o.c.RunID, o.c.EvidenceKind, status, mode, false, false, state.preview, state.samples, o.incident, o.evidence.Stop, []string{"Collection only; a preview is not the Controller's executed decision.", "Runtime success requires the independently bound fault, action, Scheduler, Service stability, Guard/Finish and cleanup evidence.", "No elapsed clock values or actor coverage are invented for legacy runtime-v1 acceptance."}, state.schedulerEvents, state.actionPods}
	if err := o.r.writeJSON("readiness-scope.json", o.c); err != nil {
		return err
	}
	if err := o.r.writeJSON("readiness-observations.json", summary); err != nil {
		return err
	}
	o.r.manifest.Acceptance = "NOT_RUN_READINESS_COLLECTION_ONLY"
	if err := o.r.finish(true); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "readinessCollector=%s mode=%s overallPass=false run=%s\n", o.r.manifest.Collector, mode, o.c.RunID); err != nil {
		return err
	}
	if o.c.Readiness.Export {
		return exportObserverRun(w, o.r.dir, o.c.RunID)
	}
	return nil
}

func (o *observer) readinessApplied() bool {
	if o.c.Readiness == nil || o.readiness == nil || o.readiness.deployment == nil || len(o.incident.Status.Runtime.Attempts) != 1 {
		return false
	}
	inc := o.incident
	cfg := o.c.Readiness
	a := inc.Status.Runtime.Attempts[0]
	d := o.readiness.deployment
	return classification.IsPodReadinessTrigger(inc.Spec.Trigger) && inc.Spec.Trigger.PodUID == cfg.SourcePod.UID && inc.Spec.Workload.UID == cfg.DeploymentUID && inc.Spec.PolicyRef.UID == cfg.PolicyUID && inc.Spec.PolicyGeneration == cfg.PolicyGeneration && inc.Spec.PolicyHash == cfg.PolicyHash && a.ActionID != "" && cfg.allowedNode(a.NodeName, a.NodeUID, true) && v1.TemplateHash(d.Spec.Template) == a.TargetTemplateHash && d.Spec.Template.Annotations["opsloop.io/attempt-id"] == a.ActionID && d.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] == a.Hostname
}

// Validate before any Incident-selected Pod/ReplicaSet/Node request. Recording
// a later gap would not undo following an unrelated dynamic object name.
func (o *observer) checkReadinessIncident(inc v1.RecoveryIncident) error {
	cfg := o.c.Readiness
	if cfg == nil {
		return nil
	}
	if cfg.Mode != "runtime" || inc.Name != o.c.Incident || inc.Namespace != o.c.Namespace || inc.Spec.PolicyRef.Name != o.c.Policy || inc.Spec.PolicyRef.UID != cfg.PolicyUID || inc.Spec.PolicyGeneration != cfg.PolicyGeneration || inc.Spec.PolicyHash != cfg.PolicyHash || v1.PolicyHash(inc.Spec.Policy) != cfg.PolicyHash || inc.Spec.Workload.Name != o.c.Workload || inc.Spec.Workload.UID != cfg.DeploymentUID || !classification.IsPodReadinessTrigger(inc.Spec.Trigger) || inc.Spec.Trigger.PodName != cfg.SourcePod.Name || inc.Spec.Trigger.PodUID != cfg.SourcePod.UID || inc.Spec.Trigger.NodeName != cfg.SourceNode.Name || inc.Spec.Trigger.NodeUID != cfg.SourceNode.UID {
		return errors.New("observer: readiness Incident outside exact trial binding")
	}
	if o.readiness == nil || o.readiness.originalContainer == "" || classification.ReadinessTriggerContainerHash(inc.Spec.Trigger) != o.readiness.originalContainer {
		return errors.New("observer: Incident has no matching prior healthy source container")
	}
	return nil
}
