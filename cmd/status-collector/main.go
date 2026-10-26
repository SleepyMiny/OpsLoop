// status-collector only observes and sends allowlisted summaries. It has no HTTP listener.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/SleepyMiny/OpsLoop/internal/publicstatus"
	"k8s.io/client-go/rest"
)

type config struct {
	APIServer     string            `json:"apiServer"`
	CAFile        string            `json:"caFile"`
	TokenFile     string            `json:"tokenFile"`
	PrometheusURL string            `json:"prometheusURL"`
	Nodes         map[string]string `json:"nodes"`
	Origin        string            `json:"origin"`
	KeyID         string            `json:"keyId"`
	KeyFile       string            `json:"keyFile"`
	SequenceDir   string            `json:"sequenceDir"`
}

func run() error {
	cfgFile := flag.String("config", "", "Private JSON config, not an administrator kubeconfig")
	once := flag.Bool("once", false, "Collect and publish once")
	initSeq := flag.Bool("init-sequence", false, "Initialize a NEW private counter; refuse existing state")
	initOnly := flag.Bool("init-only", false, "Initialize local sequence state only; never query or publish (requires --init-sequence)")
	fixture := flag.String("offline-fixture", "", "Synthetic JSON only; requires output file and never sends network requests")
	output := flag.String("envelope-output", "", "Offline signed request envelope (test data only)")
	flag.Parse()
	if *initOnly && (!*initSeq || *fixture != "" || *output != "" || *once) {
		return errors.New("init-only requires init-sequence and cannot collect or sign")
	}
	if *cfgFile == "" {
		return errors.New("private --config required")
	}
	raw, e := publicstatus.PrivateFile(*cfgFile)
	if e != nil {
		return e
	}
	var cfg config
	// Configuration is typed and rejects administrator kubeconfig/exec plugin fields.
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e = d.Decode(&cfg); e != nil {
		return errors.New("invalid private config")
	}
	if e = d.Decode(new(any)); e != io.EOF {
		return errors.New("invalid private config")
	}
	seq, e := publicstatus.OpenSequence(cfg.SequenceDir, *initSeq)
	if e != nil {
		return e
	}
	defer seq.Close()
	if *initOnly {
		if !*initSeq {
			return errors.New("init-only requires explicit init-sequence")
		}
		return nil
	}
	sender := publicstatus.Sender{Origin: cfg.Origin, KeyID: cfg.KeyID, KeyFile: cfg.KeyFile, Sequence: seq}
	if *fixture != "" {
		if *output == "" {
			return errors.New("offline fixture requires output file")
		}
		raw, e = os.ReadFile(*fixture)
		if e != nil {
			return errors.New("offline fixture unavailable")
		}
		request, e := sender.Request(context.Background(), raw, time.Now())
		if e != nil {
			return e
		}
		body := struct {
			URL     string      `json:"url"`
			Method  string      `json:"method"`
			Headers http.Header `json:"headers"`
			Body    string      `json:"body"`
		}{request.URL.String(), request.Method, request.Header, string(raw)}
		encoded, e := json.Marshal(body)
		if e != nil {
			return errors.New("offline encoding failed")
		}
		f, e := os.OpenFile(*output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return errors.New("offline output already exists or unavailable")
		}
		_, e = f.Write(encoded)
		ce := f.Close()
		if e != nil {
			return e
		}
		return ce
	}
	if *output != "" {
		return errors.New("output requires explicit offline fixture")
	}
	if !publicstatus.ValidateEndpoints(cfg.APIServer, cfg.PrometheusURL) {
		return errors.New("invalid observation endpoints")
	}
	if _, e = publicstatus.PrivateFile(cfg.TokenFile); e != nil {
		return e
	}
	api, e := publicstatus.NewPrometheusAPI(cfg.PrometheusURL, nil)
	if e != nil {
		return errors.New("Prometheus client unavailable")
	}
	collector, e := publicstatus.NewCollector(&rest.Config{Host: cfg.APIServer, TLSClientConfig: rest.TLSClientConfig{CAFile: cfg.CAFile}, BearerTokenFile: cfg.TokenFile}, cfg.Nodes, api)
	if e != nil {
		return errors.New("Kubernetes client unavailable")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	delay := 30 * time.Second
	for {
		collection, stop := context.WithTimeout(ctx, 12*time.Second)
		snapshot := collector.Collect(collection)
		stop()
		raw, e = snapshot.Marshal()
		if e == nil {
			e = sender.Send(ctx, raw, time.Now())
		}
		if *once {
			return e
		}
		if e != nil {
			log.Print("public status cycle failed; details omitted")
			delay *= 2
			if delay > 300*time.Second {
				delay = 300 * time.Second
			}
		} else {
			delay = 30 * time.Second
		}
		timer := time.NewTimer(delay + time.Duration(rand.IntN(1000))*time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
func main() {
	log.SetFlags(0)
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, "status collector stopped:", e.Error())
		os.Exit(1)
	}
}
