package main

import (
	"bufio"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	v1alpha1 "github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	summarySchemaVersion = "demo-a-summary/v1"
	scenarioAllRejected  = "all-rejected"
	modeEnforce          = "Enforce"
	modeObserveOnly      = "ObserveOnly"
	// Consecutive successes from the attempt Pod that end the outage (8.1 tRecoverSeconds).
	recoverRunLength = 5
	// The controller runs on this Node, so tPlace is single-clock only when it is the target.
	controllerNode = "opsloop-control"
)

// ProbeRecord is one line of probe.jsonl.
type ProbeRecord struct {
	PathID    string    `json:"pathID,omitempty"`
	URL       string    `json:"url,omitempty"`
	T         time.Time `json:"t"`
	OK        bool      `json:"ok"`
	Status    int       `json:"status"`
	LatencyMs int64     `json:"latencyMs"`
	Node      string    `json:"node"`
	Pod       string    `json:"pod"`
	PodUID    string    `json:"podUID"`
	Arch      string    `json:"arch"`
	Version   string    `json:"version"`
	Error     string    `json:"error"`
}

type DeploymentState struct {
	Generation int64  `json:"generation"`
	SpecSHA256 string `json:"specSha256"`
}

// Env is the part of run.sh env.json that the report reads.
type Env struct {
	RuntimeService   *ServiceIdentity  `json:"runtimeService,omitempty"`
	RunID            string            `json:"runId"`
	InjectCmdSeconds *float64          `json:"injectCmdSeconds"`
	ImageCache       string            `json:"imageCache"`
	NetPath          string            `json:"netPath"`
	ImageRef         string            `json:"imageRef"`
	NodeTimeSync     map[string]string `json:"nodeTimeSync"`
	DeploymentBefore *DeploymentState  `json:"deploymentBefore"`
	DeploymentAfter  *DeploymentState  `json:"deploymentAfter"`
}

// PodList is the subset of `kubectl get pods -o json` the report needs.
type PodList struct {
	Items []struct {
		Metadata struct {
			Name            string                  `json:"name"`
			UID             string                  `json:"uid"`
			Namespace       string                  `json:"namespace"`
			OwnerReferences []metav1.OwnerReference `json:"ownerReferences"`
		} `json:"metadata"`
		Spec struct {
			NodeName   string `json:"nodeName"`
			Containers []struct {
				Image string `json:"image"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			ContainerStatuses []struct {
				ImageID string `json:"imageID"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

type Summary struct {
	SchemaVersion        string              `json:"schemaVersion"`
	RunID                string              `json:"runId"`
	Scenario             string              `json:"scenario"`
	Mode                 string              `json:"mode"`
	InjectAt             string              `json:"injectAt"`
	InjectCmdSeconds     *float64            `json:"injectCmdSeconds"`
	Incident             string              `json:"incident"`
	Phase                string              `json:"phase"`
	Reason               string              `json:"reason"`
	Class                string              `json:"class"`
	PolicyHash           string              `json:"policyHash"`
	ImageRef             string              `json:"imageRef"`
	ImageIndexDigest     string              `json:"imageIndexDigest"`
	PodImageID           string              `json:"podImageID"`
	ImageDigestMatch     bool                `json:"imageDigestMatch"`
	ImageCache           string              `json:"imageCache"`
	NetPath              string              `json:"netPath"`
	ExpectedNode         string              `json:"expectedNode"`
	SelectedNode         string              `json:"selectedNode"`
	SelectedPlatform     string              `json:"selectedPlatform"`
	ServedArch           string              `json:"servedArch"`
	ServedVersion        string              `json:"servedVersion"`
	ServedNode           string              `json:"servedNode"`
	ExpectedVersion      string              `json:"expectedVersion"`
	Attempts             int                 `json:"attempts"`
	ExpectedRejects      map[string]string   `json:"expectedRejects"`
	Rejects              map[string][]string `json:"rejects"`
	CandidateCPUPermille map[string]*int32   `json:"candidateCPUPermille"`
	OldPodsOutstanding   int                 `json:"oldPodsOutstanding"`
	TDetectSeconds       *float64            `json:"tDetectSeconds"`
	TDecideSeconds       *float64            `json:"tDecideSeconds"`
	TPlaceSeconds        *float64            `json:"tPlaceSeconds"`
	TRecoverSeconds      *float64            `json:"tRecoverSeconds"`
	HostMaxGapSeconds    *float64            `json:"hostMaxGapSeconds"`
	Pass                 bool                `json:"pass"`
	Notes                []string            `json:"notes"`
	NodeTimeSync         map[string]string   `json:"nodeTimeSync"`
}

type ReportInput struct {
	SourceHashes    map[string]string
	Probe           []ProbeRecord
	Incident        v1alpha1.RecoveryIncident
	Pods            PodList
	Env             Env
	InjectAt        time.Time
	Mode            string
	Scenario        string
	ExpectNode      string
	ExpectVersion   string
	ExpectReject    map[string]string
	PlatformDigests []string
}

// ReadProbe parses probe.jsonl; a malformed line is an error so a broken file never yields a PASS.
func ReadProbe(r io.Reader) ([]ProbeRecord, error) {
	var recs []ProbeRecord
	lr := &io.LimitedReader{R: r, N: maxInputBytes + 1}
	sc := bufio.NewScanner(lr)
	sc.Buffer(make([]byte, 4096), maxProbeLineBytes)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec ProbeRecord
		if err := strictDecode([]byte(line), &rec, false); err != nil {
			return nil, fmt.Errorf("probe line %d: %w", n, err)
		}
		if len(recs) >= maxProbeRecords {
			return nil, fmt.Errorf("probe exceeds %d records", maxProbeRecords)
		}
		recs = append(recs, rec)
	}
	if sc.Err() != nil {
		return nil, fmt.Errorf("probe line exceeds 64 KiB or read failed: %w", sc.Err())
	}
	if lr.N <= 0 {
		return nil, fmt.Errorf("probe input exceeds 16 MiB")
	}
	return recs, nil
}

func Report(in ReportInput) Summary {
	inc := in.Incident
	st := inc.Status.Runtime
	var success *v1alpha1.Attempt
	for i := range st.Attempts {
		if st.Attempts[i].Outcome == "Succeeded" {
			success = &st.Attempts[i]
			break
		}
	}
	// Decision source (8.1 r3): the successful attempt's decision, otherwise status.runtime.decision.
	dec := st.Decision
	if success != nil {
		dec = &success.Decision
	}

	s := Summary{
		SchemaVersion:        summarySchemaVersion,
		RunID:                in.Env.RunID,
		Scenario:             in.Scenario,
		Mode:                 in.Mode,
		InjectAt:             in.InjectAt.UTC().Format(time.RFC3339Nano),
		InjectCmdSeconds:     in.Env.InjectCmdSeconds,
		Incident:             inc.Name,
		Phase:                st.Phase,
		Reason:               st.Reason,
		Class:                inc.Spec.Trigger.Class,
		PolicyHash:           inc.Spec.PolicyHash,
		ImageRef:             in.Env.ImageRef,
		ImageCache:           in.Env.ImageCache,
		NetPath:              in.Env.NetPath,
		ExpectedNode:         in.ExpectNode,
		ExpectedVersion:      in.ExpectVersion,
		Attempts:             len(st.Attempts),
		ExpectedRejects:      in.ExpectReject,
		Rejects:              map[string][]string{},
		CandidateCPUPermille: map[string]*int32{},
		OldPodsOutstanding:   len(st.OldPodsOutstanding),
		TDetectSeconds:       secondsBetween(in.InjectAt, inc.Spec.DetectedAt.Time),
		HostMaxGapSeconds:    hostMaxGap(in.Probe),
		Pass:                 true,
		Notes:                []string{"VM clock values are second-resolution (+-1s)"},
		NodeTimeSync:         in.Env.NodeTimeSync,
	}
	check := func(ok bool, what string) {
		if !ok {
			s.Pass = false
			s.Notes = append(s.Notes, "FAIL: "+what)
		}
	}
	// An Incident detected before injection (beyond the +-1s VM clock resolution) is not this run's.
	check(!inc.Spec.DetectedAt.Time.Before(in.InjectAt.Add(-time.Second)), "incident detectedAt is before injectAt-1s")

	if dec != nil {
		s.ImageIndexDigest = dec.ImageIndexDigest
		s.SelectedNode = dec.SelectedNodeName
		for _, c := range dec.Candidates {
			s.Rejects[c.NodeName] = c.RejectReasons
			s.CandidateCPUPermille[c.NodeName] = c.Measurements.CPUUtilizationPermille
			if c.NodeName == dec.SelectedNodeName {
				s.SelectedPlatform = c.Platform
			}
		}
	}
	for n, r := range in.ExpectReject {
		c := candidate(dec, n)
		check(c != nil && slices.Contains(c.RejectReasons, r), fmt.Sprintf("candidate %s not rejected with %s", n, r))
	}

	switch {
	case in.Scenario == scenarioAllRejected:
		check(st.Phase == "FAILED" && st.Reason == "DEADLINE_EXCEEDED", "phase/reason is not FAILED/DEADLINE_EXCEEDED")
		check(len(st.Attempts) == 0, "attempts is not 0")
		check(dec != nil && dec.DecisionReason == "NO_CANDIDATE", "decisionReason is not NO_CANDIDATE")
	case in.Mode == modeObserveOnly:
		check(st.Phase == "OBSERVED", "phase is not OBSERVED")
		check(dec != nil && dec.SelectedNodeName == in.ExpectNode, "selected node is not the expected node")
		b, a := in.Env.DeploymentBefore, in.Env.DeploymentAfter
		check(b != nil && a != nil && b.SpecSHA256 != "" && *b == *a, "deployment generation or .spec sha256 changed")
	default:
		reportEnforce(&s, in, success, dec, check)
	}
	return s
}

func reportEnforce(s *Summary, in ReportInput, success *v1alpha1.Attempt, dec *v1alpha1.RecoveryDecision, check func(bool, string)) {
	st := in.Incident.Status.Runtime
	check(st.Phase == "RECOVERED", "phase is not RECOVERED")
	check(s.SelectedNode == in.ExpectNode, "selected node is not the expected node")
	if st.Phase == "RECOVERED" && len(st.Attempts) > 1 {
		s.Notes = append(s.Notes, "retry")
	}
	check(len(st.Attempts) == 1, "attempts is not 1")
	c := candidate(dec, in.ExpectNode)
	check(c != nil && c.Eligible, "expected node candidate is not eligible")
	if success == nil {
		check(false, "no succeeded attempt")
		return
	}

	s.TDecideSeconds = secondsBetween(in.Incident.Spec.DetectedAt.Time, dec.DecidedAt.Time)
	if success.PodReadyAt != nil {
		s.TPlaceSeconds = secondsBetween(dec.DecidedAt.Time, success.PodReadyAt.Time)
	}
	if success.NodeName != controllerNode {
		s.Notes = append(s.Notes, "tPlace cross-clock (controller VM vs target kubelet)")
	}

	s.ImageDigestMatch = imageMatches(s, in, success.PodUID, dec.ImageIndexDigest)
	check(s.ImageDigestMatch, "attempt Pod image or imageID digest does not match the index")

	var nodeArch string
	if c := candidate(dec, success.NodeName); c != nil {
		_, nodeArch, _ = strings.Cut(c.Platform, "/")
	}
	run := recoverRun(in.Probe, success.PodUID)
	if len(run) == 0 {
		check(false, "no recover run from the attempt Pod")
		return
	}
	s.TRecoverSeconds = secondsBetween(in.InjectAt, run[0].T)
	s.ServedArch, s.ServedVersion, s.ServedNode = run[0].Arch, run[0].Version, run[0].Node
	for _, r := range run {
		check(r.Arch == nodeArch, "served arch differs from the attempt Node arch")
		check(r.Version == in.ExpectVersion, "served version differs from expect-version")
		check(r.Node == success.PodNodeName, "served node differs from attempt podNodeName")
		if !s.Pass {
			return
		}
	}
}

// imageMatches checks the attempt Pod runs the decided index (8.1 PASS 3) and records its imageID.
func imageMatches(s *Summary, in ReportInput, podUID, indexDigest string) bool {
	allowed := append([]string{indexDigest}, in.PlatformDigests...)
	for _, p := range in.Pods.Items {
		if p.Metadata.UID != podUID || podUID == "" {
			continue
		}
		if indexDigest == "" || len(p.Spec.Containers) == 0 || len(p.Status.ContainerStatuses) == 0 {
			return false
		}
		s.PodImageID = p.Status.ContainerStatuses[0].ImageID
		for _, c := range p.Spec.Containers {
			if !strings.Contains(c.Image, "@"+indexDigest) {
				return false
			}
		}
		for _, cs := range p.Status.ContainerStatuses {
			if !slices.Contains(allowed, imageIDDigest(cs.ImageID)) {
				return false
			}
		}
		return true
	}
	return false
}

// imageIDDigest accepts both "repo@sha256:x" and bare "sha256:x" as containerd reports either.
func imageIDDigest(id string) string {
	if i := strings.LastIndex(id, "@"); i >= 0 {
		return id[i+1:]
	}
	return id
}

func candidate(dec *v1alpha1.RecoveryDecision, node string) *v1alpha1.CandidateEvaluation {
	if dec == nil {
		return nil
	}
	for i := range dec.Candidates {
		if dec.Candidates[i].NodeName == node {
			return &dec.Candidates[i]
		}
	}
	return nil
}

// recoverRun returns the first run of at least recoverRunLength consecutive successes served by podUID.
func recoverRun(recs []ProbeRecord, podUID string) []ProbeRecord {
	if podUID == "" {
		return nil
	}
	for i := 0; i < len(recs); {
		if !recs[i].OK || recs[i].PodUID != podUID {
			i++
			continue
		}
		j := i
		for j < len(recs) && recs[j].OK && recs[j].PodUID == podUID {
			j++
		}
		if j-i >= recoverRunLength {
			return recs[i:j]
		}
		i = j
	}
	return nil
}

// hostMaxGap is the longest "first failure -> next success" span; an unterminated span ends at the last response.
func hostMaxGap(recs []ProbeRecord) *float64 {
	if len(recs) == 0 {
		return nil
	}
	maxGap := 0.0
	for i := 0; i < len(recs); {
		if recs[i].OK {
			i++
			continue
		}
		j := i
		for j < len(recs) && !recs[j].OK {
			j++
		}
		end := recs[len(recs)-1].T
		if j < len(recs) {
			end = recs[j].T
		}
		maxGap = max(maxGap, end.Sub(recs[i].T).Seconds())
		i = j
	}
	return &maxGap
}

func secondsBetween(from, to time.Time) *float64 {
	if from.IsZero() || to.IsZero() {
		return nil
	}
	v := to.Sub(from).Seconds()
	return &v
}
