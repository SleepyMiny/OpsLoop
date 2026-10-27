package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	v1 "github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type readinessSchedulerEvent struct {
	Observed  Stamp           `json:"observed"`
	Metadata  meta.ObjectMeta `json:"metadata"`
	PodName   string          `json:"podName"`
	PodUID    string          `json:"podUID"`
	Scheduler string          `json:"scheduler"`
	Reason    string          `json:"reason"`
	Message   string          `json:"message"`
	Node      string          `json:"node"`
}

func (o *observer) observeReadinessScheduler(ctx context.Context, a v1.Attempt) error {
	if a.PodUID == "" || a.PodName == "" {
		return nil
	}
	if !o.c.Readiness.allowedNode(a.NodeName, a.NodeUID, true) {
		return errors.New("observer: scheduler target outside candidate scope")
	}
	path := "/api/v1/namespaces/" + o.c.Namespace + "/events?" + url.Values{"fieldSelector": {"involvedObject.uid=" + a.PodUID}, "limit": {"100"}}.Encode()
	o.apiGuard.allowed[path] = true
	body, status, problem := observerGET(ctx, o.api, o.c.APIOrigin+path, 1<<20)
	at, err := o.r.stamp(ctx)
	if err != nil {
		return err
	}
	var events core.EventList
	if problem != "" || json.Unmarshal(body, &events) != nil || events.Continue != "" || len(events.Items) > 100 {
		return errors.New("observer: Scheduler event observation incomplete")
	}
	projected := []readinessSchedulerEvent{}
	for _, event := range events.Items {
		ref := event.InvolvedObject
		if event.Namespace != o.c.Namespace || ref.Namespace != o.c.Namespace || ref.Kind != "Pod" || ref.Name != a.PodName || string(ref.UID) != a.PodUID {
			return errors.New("observer: Scheduler event target mismatch")
		}
		if event.Reason != "Scheduled" {
			continue
		}
		scheduler := event.ReportingController
		if scheduler == "" {
			scheduler = event.Source.Component
		}
		if scheduler != "default-scheduler" || !strings.HasSuffix(event.Message, " to "+a.NodeName) {
			return errors.New("observer: Scheduler assignment contradicts Controller target")
		}
		if len(event.Message) > 1024 {
			return errors.New("observer: Scheduler event message limit")
		}
		projected = append(projected, readinessSchedulerEvent{at, projectionMeta(event.ObjectMeta), a.PodName, a.PodUID, scheduler, event.Reason, event.Message, a.NodeName})
	}
	if _, err = o.r.raw("readiness-scheduler", path, at, status, "", projected); err != nil {
		return err
	}
	if len(projected) > 0 {
		o.readiness.schedulerWitness = true
		o.readiness.schedulerEvents = append(o.readiness.schedulerEvents, projected...)
	}
	return nil
}
