// Package verification decides whether a recovery attempt's Pod is healthy on the planned Node.
package verification

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	"github.com/SleepyMiny/OpsLoop/internal/recovery"
)

const (
	probeTimeout = 2 * time.Second
	maxBodyBytes = 64 << 10
)

// AttemptPod finds the ReplicaSet created for actionID and its live Pod (Ready first, then newest).
func AttemptPod(rss []appsv1.ReplicaSet, pods []corev1.Pod, deploymentUID types.UID, actionID string) (*appsv1.ReplicaSet, *corev1.Pod) {
	var rs *appsv1.ReplicaSet
	for i := range rss {
		ref := metav1.GetControllerOf(&rss[i])
		if ref != nil && ref.UID == deploymentUID && rss[i].Spec.Template.Annotations[recovery.AttemptAnnotation] == actionID {
			rs = &rss[i]
			break
		}
	}
	if rs == nil {
		return nil, nil
	}
	var best *corev1.Pod
	for i := range pods {
		p := &pods[i]
		ref := metav1.GetControllerOf(p)
		if ref == nil || ref.UID != rs.UID || p.DeletionTimestamp != nil {
			continue
		}
		if best == nil || better(p, best) {
			best = p
		}
	}
	return rs, best
}

func better(p, q *corev1.Pod) bool {
	if pr, qr := podReady(p) != nil, podReady(q) != nil; pr != qr {
		return pr
	}
	return q.CreationTimestamp.Before(&p.CreationTimestamp)
}

// Probe checks the attempt Pod directly by IP so an old Pod behind the Service cannot mask a failing new one.
func Probe(ctx context.Context, c *http.Client, podIP string, port int32, podUID string) error {
	if c == nil {
		c = http.DefaultClient
	}
	nc := *c
	nc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	base := "http://" + net.JoinHostPort(podIP, strconv.Itoa(int(port)))
	for _, path := range []string{"/health", "/ready"} {
		if _, err := get(ctx, &nc, base+path); err != nil {
			return err
		}
	}
	body, err := get(ctx, &nc, base+"/")
	if err != nil {
		return err
	}
	var info struct {
		PodUID string `json:"podUID"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return fmt.Errorf("GET /: decode: %w", err)
	}
	if info.PodUID != podUID {
		return fmt.Errorf("GET /: podUID %q, want %q", info.PodUID, podUID)
	}
	return nil
}

// get returns the body of a 200 response; the workload is untrusted, so the body is size-limited.
func get(ctx context.Context, c *http.Client, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", req.URL.Path, err)
	}
	if len(body) > maxBodyBytes {
		return nil, fmt.Errorf("GET %s: body exceeds %d bytes", req.URL.Path, maxBodyBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", req.URL.Path, resp.StatusCode)
	}
	return body, nil
}

type Input struct {
	Now          time.Time
	Attempt      v1alpha1.Attempt
	Verification v1alpha1.VerificationSpec
	Budget       v1alpha1.BudgetSpec
	RS           *appsv1.ReplicaSet
	Pod          *corev1.Pod
	Slices       []discoveryv1.EndpointSlice
	ProbeRan     bool
	ProbeErr     error
}

type Outcome int

const (
	Pending Outcome = iota
	Succeeded
	Failed
)

// Step advances one verification observation. On Succeeded or Failed the returned attempt
// carries its final outcome, failureReason and completedAt.
func Step(in Input) (v1alpha1.Attempt, Outcome) {
	a := in.Attempt
	now := metav1.NewTime(in.Now)
	if in.RS != nil {
		a.ReplicaSetName, a.ReplicaSetUID = in.RS.Name, string(in.RS.UID)
	}

	pod := in.Pod
	var ready *corev1.PodCondition
	var restarts int32
	if pod != nil {
		if a.PodUID != string(pod.UID) {
			// A replacement Pod starts its own readiness, restart and probe history.
			a.PodReadyAt, a.PodRestartCountAtReady, a.ConsecutiveSuccesses, a.FirstHealthyAt = nil, 0, 0, nil
		}
		a.PodName, a.PodUID, a.PodNodeName = pod.Name, string(pod.UID), pod.Spec.NodeName
		ready = podReady(pod)
		restarts = restartCount(pod)
		if ready != nil && a.PodReadyAt == nil {
			t := ready.LastTransitionTime
			if t.IsZero() {
				t = now
			}
			a.PodReadyAt = &t
			a.PodRestartCountAtReady = restarts
		}
	}

	switch {
	case ready == nil:
		// Only consecutive probes of a Ready Pod count toward stability.
		a.ConsecutiveSuccesses, a.FirstHealthyAt = 0, nil
	case in.ProbeRan && in.ProbeErr != nil:
		a.ConsecutiveSuccesses, a.FirstHealthyAt = 0, nil
		a.LastProbeError = v1alpha1.Truncate(in.ProbeErr.Error(), v1alpha1.MaxLastProbeErrorLength)
	case in.ProbeRan:
		a.ConsecutiveSuccesses++
		if a.FirstHealthyAt == nil {
			a.FirstHealthyAt = &now
		}
	}
	a.EndpointReady = pod != nil && endpointReady(in.Slices, pod.UID)

	crashed := a.PodReadyAt != nil && restarts > a.PodRestartCountAtReady
	succeeded := in.RS != nil && pod != nil && ready != nil && !crashed &&
		pod.Spec.NodeName == a.NodeName &&
		a.ConsecutiveSuccesses >= in.Verification.ConsecutiveSuccesses &&
		a.FirstHealthyAt != nil && in.Now.Sub(a.FirstHealthyAt.Time) >= in.Verification.StabilityWindow.Duration &&
		a.EndpointReady

	patchedAt := a.ReservedAt.Time
	if a.PatchedAt != nil {
		patchedAt = a.PatchedAt.Time
	}
	reason := ""
	switch {
	case pod != nil && imagePullReason(pod) != "":
		reason = imagePullReason(pod)
	case pod != nil && pod.Spec.NodeName != "" && pod.Spec.NodeName != a.NodeName:
		reason = "POD_ON_UNEXPECTED_NODE"
	case crashed:
		reason = "CRASHED_DURING_STABILITY"
	case ready == nil && in.Now.Sub(patchedAt) > in.Budget.PlacementTimeout.Duration:
		reason = "PLACEMENT_TIMEOUT"
	case !succeeded && a.PodReadyAt != nil && in.Now.Sub(a.PodReadyAt.Time) > in.Budget.VerificationTimeout.Duration:
		reason = "VERIFICATION_TIMEOUT"
	}

	switch {
	case reason != "":
		a.Outcome, a.FailureReason, a.CompletedAt = recovery.OutcomeFailed, reason, &now
		return a, Failed
	case succeeded:
		a.Outcome, a.CompletedAt = recovery.OutcomeSucceeded, &now
		return a, Succeeded
	default:
		return a, Pending
	}
}

func podReady(p *corev1.Pod) *corev1.PodCondition {
	for i := range p.Status.Conditions {
		c := &p.Status.Conditions[i]
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return c
		}
	}
	return nil
}

func restartCount(p *corev1.Pod) int32 {
	var n int32
	for _, s := range p.Status.ContainerStatuses {
		n += s.RestartCount
	}
	return n
}

func imagePullReason(p *corev1.Pod) string {
	pullBackOff := false
	for _, s := range slices.Concat(p.Status.InitContainerStatuses, p.Status.ContainerStatuses) {
		w := s.State.Waiting
		if w == nil {
			continue
		}
		if (w.Reason == "ErrImagePull" || w.Reason == "ImagePullBackOff") && strings.Contains(w.Message, "no match for platform") {
			return "IMAGE_PLATFORM_MISSING"
		}
		pullBackOff = pullBackOff || w.Reason == "ImagePullBackOff"
	}
	if pullBackOff {
		return "IMAGE_PULL_FAILED"
	}
	return ""
}

func endpointReady(slices []discoveryv1.EndpointSlice, podUID types.UID) bool {
	for _, s := range slices {
		for _, e := range s.Endpoints {
			if e.TargetRef != nil && e.TargetRef.UID == podUID && e.Conditions.Ready != nil && *e.Conditions.Ready {
				return true
			}
		}
	}
	return false
}
