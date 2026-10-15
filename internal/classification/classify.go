// Package classification turns Monitor observations into a deterministic trigger and signature.
package classification

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
)

const RuleVersion = "classify-v1"

const (
	ClassInfrastructure = "INFRASTRUCTURE_FAILURE"
	ClassResource       = "RESOURCE_FAILURE"
	ClassConfiguration  = "CONFIGURATION_FAILURE"
	ClassApplication    = "APPLICATION_FAILURE"
	ClassUnknown        = "UNKNOWN"

	ReasonNodeNotReady         = "NODE_NOT_READY"
	ReasonOOMKilled            = "OOM_KILLED"
	ReasonNodeMemoryPressure   = "NODE_MEMORY_PRESSURE"
	ReasonImagePull            = "IMAGE_PULL"
	ReasonRepeatedExit         = "REPEATED_EXIT"
	ReasonInsufficientEvidence = "INSUFFICIENT_EVIDENCE"
)

const revisionAnnotation = "deployment.kubernetes.io/revision"

type RestartObservation struct {
	PodUID    types.UID
	Container string
	At        time.Time
	ExitCode  int32
	Reason    string
}

type Input struct {
	Now         time.Time
	Detection   v1alpha1.DetectionSpec
	WorkloadUID types.UID
	ImageDigest string
	Pods        []corev1.Pod
	Nodes       map[string]*corev1.Node
	Restarts    []RestartObservation
}

// Result.RecheckAfter is the time until a NotReady Node reaches nodeUnreadyFor; zero when nothing is pending.
type Result struct {
	Detected     bool
	Trigger      v1alpha1.Trigger
	RecheckAfter time.Duration
}

// MonitoredPods keeps Pods of the current-revision ReplicaSet that are neither terminating nor finished,
// so old or terminating Pods cannot re-trigger the same failure after recovery.
func MonitoredPods(d *appsv1.Deployment, rss []appsv1.ReplicaSet, pods []corev1.Pod) []corev1.Pod {
	current := map[types.UID]bool{}
	for i := range rss {
		rs := &rss[i]
		owner := metav1.GetControllerOf(rs)
		if owner == nil || owner.UID != d.UID {
			continue
		}
		if rs.Annotations[revisionAnnotation] == d.Annotations[revisionAnnotation] {
			current[rs.UID] = true
		}
	}
	var out []corev1.Pod
	for _, p := range pods {
		owner := metav1.GetControllerOf(&p)
		if owner == nil || !current[owner.UID] || p.DeletionTimestamp != nil {
			continue
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Classify evaluates every rule on every Pod, records all matches as evidence and picks the highest-priority one.
func Classify(in Input) Result {
	pods := append([]corev1.Pod(nil), in.Pods...)
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })

	var best v1alpha1.Trigger
	bestRank := 0
	var evidence []string
	var recheck time.Duration
	digest := in.ImageDigest
	wuid := string(in.WorkloadUID)
	window := in.Detection.RestartWindow.Duration

	add := func(rank int, p *corev1.Pod, node *corev1.Node, class, reason, ev string, sigParts ...string) {
		t := v1alpha1.Trigger{
			Class:       class,
			Reason:      reason,
			Signature:   Signature(sigParts...),
			RuleVersion: RuleVersion,
			NodeName:    p.Spec.NodeName,
			PodName:     p.Name,
			PodUID:      string(p.UID),
			ObservedAt:  metav1.NewTime(in.Now),
		}
		if node != nil {
			t.NodeUID = string(node.UID)
		}
		if bestRank == 0 || rank < bestRank {
			best, bestRank = t, rank
		}
		evidence = append(evidence, v1alpha1.Truncate(ev, v1alpha1.MaxEvidenceLength))
	}

	for i := range pods {
		p := &pods[i]
		var node *corev1.Node
		nodeReady := false
		podMatched := false // rules 1-3 matched on this Pod
		if p.Spec.NodeName != "" {
			node = in.Nodes[p.Spec.NodeName]
			ready := condition(node, corev1.NodeReady)
			switch {
			case node == nil || ready == nil:
				add(5, p, node, ClassUnknown, ReasonInsufficientEvidence,
					fmt.Sprintf("%s pod=%s node=%s", ReasonInsufficientEvidence, p.Name, p.Spec.NodeName),
					ClassUnknown, ReasonInsufficientEvidence, wuid, digest, p.Spec.NodeName)
			case ready.Status == corev1.ConditionTrue:
				nodeReady = true
			default:
				elapsed := in.Now.Sub(ready.LastTransitionTime.Time)
				if remaining := in.Detection.NodeUnreadyFor.Duration - elapsed; remaining > 0 {
					if recheck == 0 || remaining < recheck {
						recheck = remaining
					}
				} else {
					podMatched = true
					add(1, p, node, ClassInfrastructure, ReasonNodeNotReady,
						fmt.Sprintf("%s pod=%s node=%s", ReasonNodeNotReady, p.Name, p.Spec.NodeName),
						ClassInfrastructure, ReasonNodeNotReady, wuid, digest, string(node.UID))
				}
			}
			if c := condition(node, corev1.NodeMemoryPressure); c != nil && c.Status == corev1.ConditionTrue {
				podMatched = true
				add(2, p, node, ClassResource, ReasonNodeMemoryPressure,
					fmt.Sprintf("%s pod=%s node=%s", ReasonNodeMemoryPressure, p.Name, p.Spec.NodeName),
					ClassResource, ReasonNodeMemoryPressure, wuid, digest, string(node.UID))
			}
		}

		statuses := append([]corev1.ContainerStatus(nil), p.Status.ContainerStatuses...)
		sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
		for _, cs := range statuses {
			if t := cs.LastTerminationState.Terminated; t != nil && t.Reason == "OOMKilled" && in.Now.Sub(t.FinishedAt.Time) <= window {
				podMatched = true
				add(2, p, node, ClassResource, ReasonOOMKilled,
					fmt.Sprintf("%s pod=%s container=%s", ReasonOOMKilled, p.Name, cs.Name),
					ClassResource, ReasonOOMKilled, wuid, digest, cs.Name)
			}
			if w := cs.State.Waiting; w != nil && (w.Reason == "ErrImagePull" || w.Reason == "ImagePullBackOff" || w.Reason == "InvalidImageName") {
				podMatched = true
				add(3, p, node, ClassConfiguration, ReasonImagePull,
					fmt.Sprintf("%s pod=%s container=%s waiting=%s", ReasonImagePull, p.Name, cs.Name, w.Reason),
					ClassConfiguration, ReasonImagePull, wuid, digest, cs.Name)
			}
		}

		if !nodeReady || podMatched {
			continue
		}
		// shortcut: exit code and terminated reason only, stack frame normalization lands in P5
		for _, cs := range statuses {
			var obs []RestartObservation
			for _, r := range in.Restarts {
				if r.PodUID == p.UID && r.Container == cs.Name && in.Now.Sub(r.At) <= window {
					obs = append(obs, r)
				}
			}
			if len(obs) == 0 || int32(len(obs)) < in.Detection.RestartThreshold {
				continue
			}
			first := obs[0]
			same := true
			for _, r := range obs[1:] {
				if r.ExitCode != first.ExitCode || r.Reason != first.Reason {
					same = false
					break
				}
			}
			if !same || first.ExitCode == 0 || first.Reason == "OOMKilled" {
				continue
			}
			add(4, p, node, ClassApplication, ReasonRepeatedExit,
				fmt.Sprintf("%s pod=%s container=%s exit=%d reason=%s count=%d", ReasonRepeatedExit, p.Name, cs.Name, first.ExitCode, first.Reason, len(obs)),
				ClassApplication, wuid, digest, cs.Name, fmt.Sprintf("exit=%d", first.ExitCode), first.Reason)
		}
	}

	if bestRank == 0 {
		return Result{RecheckAfter: recheck}
	}
	best.Evidence = v1alpha1.CapList(evidence, v1alpha1.MaxEvidenceItems)
	return Result{Detected: true, Trigger: best}
}

// TriggerPersists reports whether the failure behind an active Incident is still present.
func TriggerPersists(t v1alpha1.Trigger, nodes map[string]*corev1.Node) bool {
	if t.Class != ClassInfrastructure {
		// shortcut: persistence for non-infrastructure triggers is decided in P5
		return true
	}
	n := nodes[t.NodeName]
	if n == nil || string(n.UID) != t.NodeUID {
		return true
	}
	c := condition(n, corev1.NodeReady)
	return c == nil || c.Status != corev1.ConditionTrue
}

// Signature is the sha256 hex of the parts joined by NUL.
func Signature(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func condition(n *corev1.Node, t corev1.NodeConditionType) *corev1.NodeCondition {
	if n == nil {
		return nil
	}
	for i := range n.Status.Conditions {
		if n.Status.Conditions[i].Type == t {
			return &n.Status.Conditions[i]
		}
	}
	return nil
}
