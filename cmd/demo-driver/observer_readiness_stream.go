package main

import (
	"context"
	"encoding/json"
	"fmt"

	v1 "github.com/SleepyMiny/OpsLoop/api/v1alpha1"
)

type readinessProgress struct {
	Schema           string          `json:"schema"`
	RunID            string          `json:"runID"`
	Captured         Stamp           `json:"captured"`
	Sample           readinessSample `json:"sample"`
	PolicyUID        string          `json:"policyUID"`
	PolicyGeneration int64           `json:"policyGeneration"`
	PolicyEnabled    bool            `json:"policyEnabled"`
	DeploymentUID    string          `json:"deploymentUID"`
	IncidentUID      string          `json:"incidentUID"`
	Phase            string          `json:"phase"`
	ActionID         string          `json:"actionID"`
	ControllerPodUID string          `json:"controllerPodUID"`
	SelectedNodeUID  string          `json:"selectedNodeUID"`
	SelectedNode     string          `json:"selectedNode"`
	TemplateApplied  bool            `json:"templateApplied"`
	SchedulerWitness bool            `json:"schedulerWitness"`
	Stop             *StopProof      `json:"stop,omitempty"`
}

func (o *observer) streamReadiness(ctx context.Context, sample readinessSample) error {
	if !o.c.Readiness.Export || o.readiness.stream == nil {
		return nil
	}
	at, err := o.r.stamp(ctx)
	if err != nil {
		return err
	}
	frame := readinessProgress{Schema: "opsloop-readiness-progress/v1", RunID: o.c.RunID, Captured: at, Sample: sample, PolicyUID: string(o.lastPolicy.UID), PolicyGeneration: o.lastPolicy.Generation, PolicyEnabled: o.lastPolicy.Spec.Enabled, DeploymentUID: o.c.Readiness.DeploymentUID, IncidentUID: string(o.incident.UID), Phase: o.incident.Status.Runtime.Phase, SchedulerWitness: o.readiness.schedulerWitness, Stop: o.evidence.Stop}
	if len(o.incident.Status.Runtime.Attempts) == 1 {
		a := o.incident.Status.Runtime.Attempts[0]
		frame.ActionID = a.ActionID
		frame.ControllerPodUID = a.PodUID
		frame.SelectedNodeUID = a.NodeUID
		frame.SelectedNode = a.NodeName
		d := o.readiness.deployment
		frame.TemplateApplied = d != nil && v1.TemplateHash(d.Spec.Template) == a.TargetTemplateHash && d.Spec.Template.Annotations["opsloop.io/attempt-id"] == a.ActionID && d.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] == a.Hostname
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	ref := fmt.Sprintf("progress-%05d.json", len(o.readiness.samples))
	if err = o.r.write(ref, raw); err != nil {
		return err
	}
	envelope := struct {
		Schema string          `json:"schema"`
		RunID  string          `json:"runID"`
		Ref    string          `json:"ref"`
		SHA256 string          `json:"sha256"`
		Frame  json.RawMessage `json:"frame"`
	}{"opsloop-readiness-progress-envelope/v1", o.c.RunID, ref, digestBytes(raw), raw}
	return json.NewEncoder(o.readiness.stream).Encode(envelope)
}
