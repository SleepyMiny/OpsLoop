package controller

// Approved reproduction is a signed recipe. CurrentOccurrence is learned only
// from the Controller's live owner-checked Pod, never supplied by a client.
// Identity binding is separate from the classifier's genuine healthy observation.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/classification"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strings"
	"time"
)

const CurrentOccurrenceAnnotation = "opsloop.io/current-exhibition-occurrence"
const reproductionStartupLimit = 60 * time.Second

var errReproductionPending = errors.New("reproduction source startup pending")

// No authorization or identity is manufactured by waiting. The deadline is set
// only once, under this process-bound permit, and never extends its fixed grant.
func (a *ExhibitionAuthority) pendingReproduction(now time.Time) error {
	active := a.current
	if active.occurrence != nil {
		return a.closeReproduction("bound reproduction source runtime unavailable")
	}
	if active.sourceWaitStarted.IsZero() {
		active.sourceWaitStarted = now
		active.sourceWaitDeadline = now.Add(reproductionStartupLimit)
		if active.permit.Grant.NotAfter.Before(active.sourceWaitDeadline) {
			active.sourceWaitDeadline = active.permit.Grant.NotAfter
		}
	}
	if now.Before(active.sourceWaitStarted) || !now.Before(active.sourceWaitDeadline) {
		return a.closeReproduction("reproduction source startup deadline exceeded")
	}
	return errReproductionPending
}

type ApprovedReproduction struct {
	Schema        string `json:"schema"`
	Kind          string `json:"kind"`
	PolicyUID     string `json:"policyUID"`
	DeploymentUID string `json:"deploymentUID"`
	WorkloadImage string `json:"workloadImage"`
	ContainerName string `json:"containerName"`
	Input         struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"input"`
	Classification struct {
		Class       string `json:"class"`
		Reason      string `json:"reason"`
		RuleVersion string `json:"ruleVersion"`
	} `json:"classification"`
	BaselineTemplate json.RawMessage `json:"baselineTemplate"`
	Template         json.RawMessage `json:"template"`
}
type CurrentOccurrence struct {
	RunID                   string    `json:"runID"`
	Epoch                   string    `json:"epoch"`
	PolicyUID               string    `json:"policyUID"`
	DeploymentUID           string    `json:"deploymentUID"`
	SourcePodUID            string    `json:"sourcePodUID"`
	SourcePodName           string    `json:"sourcePodName"`
	ReplicaSetUID           string    `json:"replicaSetUID"`
	NodeUID                 string    `json:"nodeUID"`
	NodeName                string    `json:"nodeName"`
	ContainerIdentitySHA256 string    `json:"containerIdentitySHA256"`
	SourceImageID           string    `json:"sourceImageID"`
	HealthyObservedAt       time.Time `json:"healthyObservedAt"`
	Signature               string    `json:"signature"`
}

func reproductionTemplates(v *ApprovedReproduction) (corev1.PodTemplateSpec, corev1.PodTemplateSpec, error) {
	var baseline, fault corev1.PodTemplateSpec
	if v == nil {
		return baseline, fault, errors.New("approved reproduction absent")
	}
	if err := strictAuthorityJSON(v.BaselineTemplate, &baseline); err != nil {
		return baseline, fault, err
	}
	if err := strictAuthorityJSON(v.Template, &fault); err != nil {
		return baseline, fault, err
	}
	return baseline, fault, nil
}
func validateReproductionScope(p ExhibitionPermit) error {
	v := p.ApprovedReproduction
	s := p.Grant.Scope
	if v == nil || v.Schema != "opsloop-approved-reproduction/v1" || v.Kind != "DEMO_B_EMPTY_ORDER_READINESS" || v.PolicyUID != s.PolicyUID || v.DeploymentUID != s.DeploymentUID || v.WorkloadImage != p.CircuitEvidence.WorkloadImage || v.ContainerName == "" || v.Input.Name != "DEMO_B_REPLAY" || v.Input.Value != "empty-order-after-30s" || v.Classification.Class != classification.ClassApplication || v.Classification.Reason != classification.ReasonPodNotReady || v.Classification.RuleVersion != classification.ReadinessRuleVersion {
		return errors.New("approved reproduction scope changed")
	}
	baseline, fault, err := reproductionTemplates(v)
	if err != nil {
		return err
	}
	if len(baseline.Spec.Containers) != 1 || len(baseline.Spec.InitContainers) != 0 || baseline.Spec.Containers[0].Name != v.ContainerName || baseline.Spec.Containers[0].Image != v.WorkloadImage || baseline.Spec.Containers[0].ReadinessProbe == nil {
		return errors.New("approved reproduction container changed")
	}
	count := 0
	for i := range baseline.Spec.Containers[0].Env {
		e := &baseline.Spec.Containers[0].Env[i]
		if e.Name == v.Input.Name {
			count++
			if e.Value != "" || e.ValueFrom != nil {
				return errors.New("reproduction baseline input changed")
			}
			e.Value = v.Input.Value
		}
	}
	if count != 1 || !reflect.DeepEqual(baseline, fault) {
		return errors.New("unapproved reproduction template change")
	}
	if p.NewExecutionAuthorization.ApprovedReproductionSHA256 != evidenceProjectionHash(v) {
		return errors.New("reproduction authorization hash changed")
	}
	return nil
}
func (a *ExhibitionAuthority) reproductionEnabled(p *v1alpha1.RecoveryPolicy) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.current != nil && a.current.permit.Schema == "opsloop-exhibition-permit/v3" && a.current.permit.Grant.Scope.PolicyUID == string(p.UID)
}
func (a *ExhibitionAuthority) reproductionSourceBound() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.current != nil && a.current.occurrence != nil
}

func (a *ExhibitionAuthority) closeReproduction(reason string) error {
	if a.current != nil {
		a.current.authority.closed = reason
	}
	return errors.New(reason)
}

// Called with APIReader results only. No browser/host provides a Pod UID here.
func (a *ExhibitionAuthority) observeReproduction(p *v1alpha1.RecoveryPolicy, d *appsv1.Deployment, pods []corev1.Pod, rss []appsv1.ReplicaSet, nodes map[string]*corev1.Node, now time.Time) error {
	if err := a.Authorize(p, nil); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	active := a.current
	if active == nil || active.permit.Schema != "opsloop-exhibition-permit/v3" {
		return nil
	}
	baseline, fault, err := reproductionTemplates(active.permit.ApprovedReproduction)
	if err != nil {
		return a.closeReproduction(err.Error())
	}
	if !replayImageMatches(active.permit, d) {
		return a.closeReproduction("reproduction workload changed")
	}
	if reflect.DeepEqual(d.Spec.Template, baseline) && active.occurrence == nil {
		if !active.sourceWaitStarted.IsZero() {
			return a.closeReproduction("reproduction rolled back during startup")
		}
		// The host must observe a genuine healthy baseline before its fault CAS.
		// No source is bound here; the trigger binder still blocks actions.
		return nil
	}
	if !reflect.DeepEqual(d.Spec.Template, fault) {
		return a.closeReproduction("reproduction template changed")
	}
	if active.occurrence == nil && !active.sourceWaitStarted.IsZero() && (now.Before(active.sourceWaitStarted) || !now.Before(active.sourceWaitDeadline)) {
		return a.closeReproduction("reproduction source startup deadline exceeded")
	}
	// An unsettled rollout cannot supply an unambiguous initial source. Once bound,
	// any gap/multiplicity is a stop, never a chance to adopt another Pod.
	if len(pods) != 1 {
		if active.occurrence != nil {
			return a.closeReproduction("bound reproduction source absent/ambiguous")
		}
		return a.pendingReproduction(now)
	}
	pod := &pods[0]
	owner := metav1.GetControllerOf(pod)
	var rs *appsv1.ReplicaSet
	if owner != nil {
		for i := range rss {
			if rss[i].UID == owner.UID {
				rs = &rss[i]
				break
			}
		}
	}
	if owner == nil || owner.Kind != "ReplicaSet" || rs == nil || rs.Name != owner.Name || rs.DeletionTimestamp != nil || pod.Namespace != d.Namespace || pod.DeletionTimestamp != nil {
		return a.closeReproduction("reproduction owner absent")
	}
	dep := metav1.GetControllerOf(rs)
	tmpl := rs.Spec.Template.DeepCopy()
	delete(tmpl.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
	if dep == nil || dep.Kind != "Deployment" || dep.UID != d.UID || dep.Name != d.Name || !reflect.DeepEqual(*tmpl, fault) || !reflect.DeepEqual(pod.Spec.Containers, fault.Spec.Containers) || len(pod.Spec.InitContainers) != 0 {
		return a.closeReproduction("reproduction owner/template changed")
	}
	if pod.UID == "" || rs.UID == "" {
		return a.closeReproduction("reproduction source identity absent")
	}
	// Validate any available status before treating missing startup information
	// as transient. Wrong names/images and terminated runtimes are not pending.
	if len(pod.Status.ContainerStatuses) > 1 {
		return a.closeReproduction("reproduction source container changed")
	}
	if len(pod.Status.ContainerStatuses) == 1 {
		cs := pod.Status.ContainerStatuses[0]
		if cs.Name != active.permit.ApprovedReproduction.ContainerName || cs.State.Terminated != nil || (cs.ImageID != "" && !strings.HasSuffix(cs.ImageID, "@"+strings.Split(active.permit.ApprovedReproduction.WorkloadImage, "@")[1])) {
			return a.closeReproduction("reproduction source runtime changed")
		}
	}
	node := nodes[pod.Spec.NodeName]
	if pod.Spec.NodeName != "" && (node == nil || node.UID == "" || !nodeReady(node)) {
		return a.closeReproduction("reproduction source node unconfirmed")
	}
	identity := classification.ReadinessContainerHash(pod)
	if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
		return a.closeReproduction("reproduction source terminal")
	}
	if pod.Spec.NodeName == "" || identity == "" || len(pod.Status.ContainerStatuses) == 0 || pod.Status.ContainerStatuses[0].ImageID == "" || pod.Status.ContainerStatuses[0].State.Running == nil || pod.Status.Phase != corev1.PodRunning {
		return a.pendingReproduction(now)
	}
	current := CurrentOccurrence{RunID: active.permit.Grant.Scope.RunID, Epoch: active.permit.Grant.Scope.Epoch, PolicyUID: string(p.UID), DeploymentUID: string(d.UID), SourcePodUID: string(pod.UID), SourcePodName: pod.Name, ReplicaSetUID: string(rs.UID), NodeUID: string(node.UID), NodeName: node.Name, ContainerIdentitySHA256: identity, SourceImageID: pod.Status.ContainerStatuses[0].ImageID}
	if active.occurrence == nil {
		if classification.PodHealthy(pod) {
			current.HealthyObservedAt = now
		}
		active.occurrence = &current
		return nil
	}
	current.HealthyObservedAt = active.occurrence.HealthyObservedAt
	current.Signature = active.occurrence.Signature
	if current != *active.occurrence {
		return a.closeReproduction("reproduction source rebound forbidden")
	}
	if active.occurrence.HealthyObservedAt.IsZero() && classification.PodHealthy(pod) {
		active.occurrence.HealthyObservedAt = now
	}
	return nil
}
func (a *ExhibitionAuthority) bindReproductionTrigger(p *v1alpha1.RecoveryPolicy, t v1alpha1.Trigger, digest string) error {
	if err := a.Authorize(p, nil); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	active := a.current
	if active == nil || active.permit.Schema != "opsloop-exhibition-permit/v3" {
		return nil
	}
	o := active.occurrence
	v := active.permit.ApprovedReproduction
	if o == nil || o.HealthyObservedAt.IsZero() || !classification.IsPodReadinessTrigger(t) || t.RuleVersion != classification.ReadinessRuleVersion || t.PodUID != o.SourcePodUID || t.PodName != o.SourcePodName || t.NodeUID != o.NodeUID || t.NodeName != o.NodeName || classification.ReadinessTriggerContainerHash(t) != o.ContainerIdentitySHA256 || !t.ObservedAt.After(o.HealthyObservedAt) || digest != strings.Split(v.WorkloadImage, "@")[1] {
		return a.closeReproduction("trigger outside approved current reproduction")
	}
	expected := classification.Signature(classification.ClassApplication, classification.ReasonPodNotReady, o.DeploymentUID, digest, o.SourcePodUID, o.ContainerIdentitySHA256)
	if t.Signature != expected || (o.Signature != "" && o.Signature != t.Signature) {
		return a.closeReproduction("current occurrence signature changed")
	}
	o.Signature = t.Signature
	return nil
}
func (a *ExhibitionAuthority) currentOccurrenceMatches(inc *v1alpha1.RecoveryIncident) bool {
	if a.current == nil || a.current.occurrence == nil {
		return false
	}
	o := a.current.occurrence
	t := inc.Spec.Trigger
	raw, _ := json.Marshal(o)
	return o.Signature != "" && inc.Annotations[CurrentOccurrenceAnnotation] == string(raw) && t.Signature == o.Signature && t.PodUID == o.SourcePodUID && t.PodName == o.SourcePodName && t.NodeUID == o.NodeUID && t.NodeName == o.NodeName && classification.ReadinessTriggerContainerHash(t) == o.ContainerIdentitySHA256 && classification.IsPodReadinessTrigger(t) && t.RuleVersion == classification.ReadinessRuleVersion
}

// Fresh inputs used by the actual classifier; cached Pod identities cannot bind
// a new occurrence. The existing live pre-dispatch readiness/owner checks remain.
func (r *PolicyReconciler) reproductionInputs(ctx context.Context, p *v1alpha1.RecoveryPolicy, d *appsv1.Deployment, now time.Time) ([]corev1.Pod, map[string]*corev1.Node, error) {
	live, err := r.liveDeployment(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	if live == nil || live.UID != d.UID || !reflect.DeepEqual(live.Spec, d.Spec) {
		return nil, nil, errors.New("reproduction live workload changed during observation")
	}
	var rss appsv1.ReplicaSetList
	var pods corev1.PodList
	if err = r.APIReader.List(ctx, &rss, client.InNamespace(p.Namespace)); err != nil {
		return nil, nil, err
	}
	if err = r.APIReader.List(ctx, &pods, client.InNamespace(p.Namespace)); err != nil {
		return nil, nil, err
	}
	filtered := []appsv1.ReplicaSet{}
	for _, rs := range rss.Items {
		template := rs.Spec.Template.DeepCopy()
		delete(template.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
		if reflect.DeepEqual(*template, live.Spec.Template) {
			filtered = append(filtered, rs)
		}
	}
	monitored := classification.MonitoredPods(live, filtered, pods.Items)
	nodes := map[string]*corev1.Node{}
	for _, pod := range monitored {
		if pod.Spec.NodeName == "" {
			continue
		}
		node := &corev1.Node{}
		if err = r.APIReader.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, node); err != nil {
			return nil, nil, err
		}
		nodes[node.Name] = node
	}
	a, ok := r.Authority.(*ExhibitionAuthority)
	if !ok {
		return nil, nil, fmt.Errorf("reproduction authority absent")
	}
	if err = a.observeReproduction(p, live, monitored, filtered, nodes, now); err != nil {
		return nil, nil, err
	}
	return monitored, nodes, nil
}

// Invoked by the existing live readiness check immediately before workload CAS.
// It does not bind anew and is never used to adopt the post-action replacement.
func (a *ExhibitionAuthority) validateCurrentReproductionSource(inc *v1alpha1.RecoveryIncident, pod *corev1.Pod, rs *appsv1.ReplicaSet, node *corev1.Node) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.current == nil || a.current.permit.Schema != "opsloop-exhibition-permit/v3" {
		return nil
	}
	o := a.current.occurrence
	if a.current.retired || a.current.authority.closed != "" || !a.currentOccurrenceMatches(inc) || o == nil || pod == nil || rs == nil || node == nil {
		return a.closeReproduction("current reproduction source unavailable")
	}
	_, fault, err := reproductionTemplates(a.current.permit.ApprovedReproduction)
	if err != nil {
		return a.closeReproduction(err.Error())
	}
	owner := metav1.GetControllerOf(pod)
	dep := metav1.GetControllerOf(rs)
	tmpl := rs.Spec.Template.DeepCopy()
	delete(tmpl.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
	if string(pod.UID) != o.SourcePodUID || pod.Name != o.SourcePodName || pod.Namespace != inc.Namespace || pod.DeletionTimestamp != nil || string(rs.UID) != o.ReplicaSetUID || rs.DeletionTimestamp != nil || owner == nil || owner.Kind != "ReplicaSet" || owner.UID != rs.UID || owner.Name != rs.Name || dep == nil || dep.Kind != "Deployment" || string(dep.UID) != o.DeploymentUID || dep.Name != inc.Spec.Workload.Name || string(node.UID) != o.NodeUID || node.Name != o.NodeName || pod.Spec.NodeName != node.Name || !nodeReady(node) || !reflect.DeepEqual(*tmpl, fault) || !reflect.DeepEqual(pod.Spec.Containers, fault.Spec.Containers) || len(pod.Spec.InitContainers) != 0 || len(pod.Status.ContainerStatuses) != 1 || pod.Status.ContainerStatuses[0].Name != a.current.permit.ApprovedReproduction.ContainerName || pod.Status.ContainerStatuses[0].ImageID != o.SourceImageID || classification.ReadinessContainerHash(pod) != o.ContainerIdentitySHA256 {
		return a.closeReproduction("bound source changed before execution")
	}
	return nil
}
