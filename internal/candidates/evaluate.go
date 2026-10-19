// Package candidates evaluates relocation target Nodes with hard filters and an integer score.
package candidates

import (
	"slices"
	"sort"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
)

const (
	RejectNodeNotReady                = "NODE_NOT_READY"
	RejectUnschedulable               = "UNSCHEDULABLE"
	RejectUntoleratedTaint            = "UNTOLERATED_TAINT"
	RejectMemoryPressure              = "MEMORY_PRESSURE"
	RejectDiskPressure                = "DISK_PRESSURE"
	RejectPIDPressure                 = "PID_PRESSURE"
	RejectCPUPressure                 = "CPU_PRESSURE"
	RejectArchMismatch                = "ARCH_MISMATCH"
	RejectInsufficientRequestedCPU    = "INSUFFICIENT_REQUESTED_CPU"
	RejectInsufficientRequestedMemory = "INSUFFICIENT_REQUESTED_MEMORY"
	RejectInsufficientAvailableMemory = "INSUFFICIENT_AVAILABLE_MEMORY"
	RejectMetricsMissing              = "METRICS_MISSING"
	RejectMetricsStale                = "METRICS_STALE"
	RejectCooldown                    = "COOLDOWN"
	RejectUnsupportedConstraint       = "UNSUPPORTED_CONSTRAINT"

	DecisionSelected           = "SELECTED_HIGHEST_SCORE"
	DecisionNoCandidate        = "NO_CANDIDATE"
	DecisionMetricsUnavailable = "METRICS_UNAVAILABLE"

	ActionRelocation = "Relocation"
	ActionNone       = "None"
)

const attemptFailed = "Failed"

const (
	hostnameLabel = "kubernetes.io/hostname"
	osLabel       = "kubernetes.io/os"
	archLabel     = "kubernetes.io/arch"
)

type Input struct {
	Now                               time.Time
	Spec                              v1alpha1.RecoveryPolicySpec // incident.spec.policy snapshot
	RecentFailures                    []v1alpha1.NodeFailure      // live Policy status.recentFailures
	Attempts                          []v1alpha1.Attempt          // current Incident attempts
	PolicyHash                        string
	PolicyGeneration                  int64
	IncidentName, ActionID            string
	Deployment                        *appsv1.Deployment
	Nodes                             []corev1.Node // every Node matching candidateSelector
	PodsByNode                        map[string][]corev1.Pod
	Metrics                           Metrics
	MetricsErr                        error
	ImageDigest, BaselineTemplateHash string
}

// Evaluate filters and scores every candidate and returns the full decision; it never mutates the input.
func Evaluate(in Input) v1alpha1.RecoveryDecision {
	nodes := append([]corev1.Node(nil), in.Nodes...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	// shortcut: candidates capped at 16 (CRD MaxItems), redesign recording for large candidate pools
	if len(nodes) > v1alpha1.MaxCandidates {
		nodes = nodes[:v1alpha1.MaxCandidates]
	}

	failures := mergeFailures(in)
	newCPU, newMem := PodRequests(in.Deployment.Spec.Template.Spec)
	maxAge := in.Spec.Filter.MaxSampleAge.Duration

	cands := []v1alpha1.CandidateEvaluation{}
	best := -1
	for i := range nodes {
		c := evaluateNode(in, &nodes[i], failures, newCPU, newMem, maxAge)
		cands = append(cands, c)
		if c.Eligible && (best < 0 || c.Score.ScoreMilli > cands[best].Score.ScoreMilli) {
			best = len(cands) - 1
		}
	}

	observed := in.Metrics.ObservedAt
	if in.MetricsErr != nil || observed.IsZero() {
		observed = in.Now
	}
	d := v1alpha1.RecoveryDecision{
		IncidentID:           in.IncidentName,
		PolicyGeneration:     in.PolicyGeneration,
		PolicyHash:           in.PolicyHash,
		WorkloadUID:          string(in.Deployment.UID),
		BaselineTemplateHash: in.BaselineTemplateHash,
		ImageIndexDigest:     in.ImageDigest,
		QueryVersion:         QueryVersion,
		SnapshotObservedAt:   metav1.NewTime(observed),
		SnapshotExpiresAt:    metav1.NewTime(observed.Add(maxAge)),
		Candidates:           cands,
		ActionType:           ActionNone,
		DecisionReason:       DecisionNoCandidate,
		DecidedAt:            metav1.NewTime(in.Now),
	}
	if best < 0 {
		if in.MetricsErr != nil {
			d.DecisionReason = DecisionMetricsUnavailable
		}
		return d
	}
	sel := cands[best]
	m := sel.Measurements
	oldest := m.CPUSampleTime.Time
	if m.MemSampleTime.Before(m.CPUSampleTime) {
		oldest = m.MemSampleTime.Time
	}
	d.ActionID = in.ActionID
	d.SnapshotExpiresAt = metav1.NewTime(oldest.Add(maxAge))
	d.SelectedNodeName = sel.NodeName
	d.SelectedNodeUID = sel.NodeUID
	d.SelectedHostname = sel.Hostname
	d.ActionType = ActionRelocation
	d.DecisionReason = DecisionSelected
	return d
}

// mergeFailures joins the cross-incident ledger with this Incident's failed attempts, deduplicated by actionID.
func mergeFailures(in Input) []v1alpha1.NodeFailure {
	seen := map[string]bool{}
	var out []v1alpha1.NodeFailure
	for _, f := range in.RecentFailures {
		if !seen[f.ActionID] {
			seen[f.ActionID] = true
			out = append(out, f)
		}
	}
	for _, a := range in.Attempts {
		if a.Outcome != attemptFailed || seen[a.ActionID] {
			continue
		}
		seen[a.ActionID] = true
		at := metav1.NewTime(in.Now) // a Failed attempt without completedAt counts as failing now (longest cooldown)
		if a.CompletedAt != nil {
			at = *a.CompletedAt
		}
		out = append(out, v1alpha1.NodeFailure{ActionID: a.ActionID, NodeName: a.NodeName, NodeUID: a.NodeUID, At: at, Reason: a.FailureReason})
	}
	return out
}

func evaluateNode(in Input, n *corev1.Node, failures []v1alpha1.NodeFailure, newCPU, newMem int64, maxAge time.Duration) v1alpha1.CandidateEvaluation {
	c := v1alpha1.CandidateEvaluation{
		NodeName: n.Name,
		NodeUID:  string(n.UID),
		Hostname: n.Labels[hostnameLabel],
		Platform: n.Labels[osLabel] + "/" + n.Labels[archLabel],
	}
	var reasons, details []string
	reject := func(r string, detail ...string) {
		if !slices.Contains(reasons, r) {
			reasons = append(reasons, r)
		}
		details = append(details, detail...)
	}

	meas := &c.Measurements
	var promMissing, promStale bool
	var promDetails []string
	if in.MetricsErr != nil {
		promMissing, promDetails = true, []string{"metrics query failed"}
	} else {
		promMissing, promStale, promDetails = assessMetrics(in.Metrics, n, in.Now, maxAge, meas)
	}
	promOK := !promMissing && !promStale

	for _, reason := range StructuralRejectReasons(n, in.Spec, in.Deployment.Spec.Template.Spec.Tolerations) {
		reject(reason)
	}
	if promOK && (*meas.MemTotalBytes-*meas.MemAvailableBytes)*100 > int64(in.Spec.Filter.MaxMemoryUtilizationPercent)**meas.MemTotalBytes {
		reject(RejectMemoryPressure, "prometheus memory utilization above limit")
	}
	if promOK && *meas.CPUUtilizationPermille > in.Spec.Filter.MaxCPUUtilizationPercent*10 {
		reject(RejectCPUPressure)
	}

	meas.AllocatableCPUMilli = n.Status.Allocatable.Cpu().MilliValue()
	meas.AllocatableMemoryBytes = n.Status.Allocatable.Memory().Value()
	for _, p := range in.PodsByNode[n.Name] {
		// Terminating Pods still hold their requests until they are gone.
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		cpu, mem := PodRequests(p.Spec)
		meas.RequestedCPUMilli += cpu
		meas.RequestedMemoryBytes += mem
	}
	meas.NewPodCPUMilli, meas.NewPodMemoryBytes = newCPU, newMem
	reserve := int64(in.Spec.Filter.RequestedCapacityReservePercent)
	meas.ReserveCPUMilli = meas.AllocatableCPUMilli * reserve / 100
	meas.ReserveMemoryBytes = meas.AllocatableMemoryBytes * reserve / 100
	// shortcut: Pod overhead (RuntimeClass) not counted, add when a RuntimeClass with overhead is used
	if meas.RequestedCPUMilli+newCPU+meas.ReserveCPUMilli > meas.AllocatableCPUMilli {
		reject(RejectInsufficientRequestedCPU)
	}
	if meas.RequestedMemoryBytes+newMem+meas.ReserveMemoryBytes > meas.AllocatableMemoryBytes {
		reject(RejectInsufficientRequestedMemory)
	}
	minAvail := in.Spec.Filter.MinimumAvailableMemory.Value() + in.Spec.Filter.StartupMemoryBudget.Value()
	if promOK && *meas.MemAvailableBytes < minAvail {
		reject(RejectInsufficientAvailableMemory)
	}
	if promMissing {
		reject(RejectMetricsMissing)
	}
	if promStale {
		reject(RejectMetricsStale)
	}
	details = append(details, promDetails...)

	cooldown := in.Spec.Budget.Cooldown.Duration
	window := in.Spec.Score.RecentFailureWindow.Duration
	var recent int64
	for _, f := range failures {
		if f.NodeUID != c.NodeUID {
			continue
		}
		if f.At.Add(cooldown).After(in.Now) {
			reject(RejectCooldown, "cooldown after "+f.ActionID)
		}
		if in.Now.Sub(f.At.Time) <= window {
			recent++
		}
	}
	if c.Hostname == "" {
		reject(RejectUnsupportedConstraint, "hostname label missing")
	}

	for i := range details {
		details[i] = v1alpha1.Truncate(details[i], v1alpha1.MaxDetailLength)
	}
	c.RejectReasons = reasons
	c.Details = v1alpha1.CapList(details, v1alpha1.MaxDetailsItems)
	c.Eligible = len(reasons) == 0
	if c.Eligible {
		c.Score = score(in.Spec.Score, meas, recent)
	}
	return c
}

// score uses integer permille headrooms so the recorded values reproduce the result exactly.
func score(s v1alpha1.ScoreSpec, m *v1alpha1.Measurements, recent int64) *v1alpha1.ScoreBreakdown {
	cpuH := int64(1000 - *m.CPUUtilizationPermille)
	memH := (1000**m.MemAvailableBytes + *m.MemTotalBytes/2) / *m.MemTotalBytes
	reqH := min(
		headroom(m.AllocatableCPUMilli, m.ReserveCPUMilli+m.RequestedCPUMilli+m.NewPodCPUMilli),
		headroom(m.AllocatableMemoryBytes, m.ReserveMemoryBytes+m.RequestedMemoryBytes+m.NewPodMemoryBytes),
	)
	penalty := min(int64(s.RecentFailurePenalty)*recent, int64(s.MaxRecentFailurePenalty)) * 1000
	return &v1alpha1.ScoreBreakdown{
		CPUHeadroomPermille:       int32(cpuH),
		MemoryHeadroomPermille:    int32(memH),
		RequestedHeadroomPermille: int32(reqH),
		RecentFailures:            int32(recent),
		PenaltyMilli:              penalty,
		ScoreMilli:                int64(s.CPUWeight)*cpuH + int64(s.MemoryWeight)*memH + int64(s.RequestsWeight)*reqH - penalty,
	}
}

func headroom(alloc, used int64) int64 {
	if alloc <= 0 {
		return 0
	}
	return max(0, (alloc-used)*1000/alloc)
}

// CheckWorkload lists why the Deployment is outside the supported relocation scope; empty means supported.
func CheckWorkload(d *appsv1.Deployment) (issues []string) {
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 {
		issues = append(issues, "replicas must be 1")
	}
	ru := d.Spec.Strategy.RollingUpdate
	if d.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType || ru == nil ||
		ru.MaxSurge == nil || *ru.MaxSurge != intstr.FromInt32(1) ||
		ru.MaxUnavailable == nil || *ru.MaxUnavailable != intstr.FromInt32(0) {
		issues = append(issues, "strategy must be RollingUpdate with maxSurge=1 and maxUnavailable=0")
	}
	ps := d.Spec.Template.Spec
	for _, c := range ps.Containers {
		for _, rl := range []corev1.ResourceList{c.Resources.Requests, c.Resources.Limits} {
			_, cpu := rl[corev1.ResourceCPU]
			_, mem := rl[corev1.ResourceMemory]
			if !cpu || !mem {
				issues = append(issues, "container "+c.Name+" must set cpu and memory requests and limits")
				break
			}
		}
	}
	for _, v := range ps.Volumes {
		if v.PersistentVolumeClaim != nil {
			issues = append(issues, "persistentVolumeClaim volume "+v.Name+" is not supported")
		}
	}
	if ps.NodeName != "" {
		issues = append(issues, "template nodeName is not supported")
	}
	if ps.Affinity != nil {
		issues = append(issues, "affinity is not supported")
	}
	if len(ps.TopologySpreadConstraints) > 0 {
		issues = append(issues, "topologySpreadConstraints are not supported")
	}
	keys := make([]string, 0, len(ps.NodeSelector))
	for k := range ps.NodeSelector {
		if k != hostnameLabel {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		issues = append(issues, "nodeSelector key "+k+" is not supported")
	}
	for _, c := range slices.Concat(ps.InitContainers, ps.Containers) {
		for _, p := range c.Ports {
			if p.HostPort != 0 {
				issues = append(issues, "container "+c.Name+" hostPort is not supported")
				break
			}
		}
	}
	return issues
}

// PodRequests returns the effective Pod requests: max(sum of containers, largest init container).
// shortcut: native sidecar (restartPolicy=Always init container) not summed, fix when a workload uses sidecars
func PodRequests(spec corev1.PodSpec) (cpuMilli, memBytes int64) {
	for _, c := range spec.Containers {
		cpuMilli += c.Resources.Requests.Cpu().MilliValue()
		memBytes += c.Resources.Requests.Memory().Value()
	}
	for _, c := range spec.InitContainers {
		cpuMilli = max(cpuMilli, c.Resources.Requests.Cpu().MilliValue())
		memBytes = max(memBytes, c.Resources.Requests.Memory().Value())
	}
	return cpuMilli, memBytes
}

func condTrue(n *corev1.Node, t corev1.NodeConditionType) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == t {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// StructuralRejectReasons is shared by preflight and the post-write safety guard.
// Resource and metrics eligibility belongs only to preflight.
func StructuralRejectReasons(n *corev1.Node, spec v1alpha1.RecoveryPolicySpec, tolerations []corev1.Toleration) []string {
	var reasons []string
	reject := func(reason string, _ ...string) { reasons = append(reasons, reason) }
	if !condTrue(n, corev1.NodeReady) {
		reject(RejectNodeNotReady)
	}
	if n.Spec.Unschedulable {
		reject(RejectUnschedulable)
	}
	tols := tolerations
	for i := range n.Spec.Taints {
		t := &n.Spec.Taints[i]
		if t.Effect != corev1.TaintEffectNoSchedule && t.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		tolerated := false
		for j := range tols {
			if tols[j].ToleratesTaint(logr.Discard(), t, false) {
				tolerated = true
				break
			}
		}
		if !tolerated {
			reject(RejectUntoleratedTaint, "untolerated taint "+t.Key+":"+string(t.Effect))
		}
	}
	if condTrue(n, corev1.NodeMemoryPressure) {
		reject(RejectMemoryPressure, "MemoryPressure condition")
	}
	if condTrue(n, corev1.NodeDiskPressure) {
		reject(RejectDiskPressure)
	}
	if condTrue(n, corev1.NodePIDPressure) {
		reject(RejectPIDPressure)
	}
	if !slices.Contains(spec.RequiredPlatforms, n.Labels[osLabel]+"/"+n.Labels[archLabel]) {
		reject(RejectArchMismatch)
	}

	return reasons
}
