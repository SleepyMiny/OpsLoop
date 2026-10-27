package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"os"
	"time"
)

func newFaultAdminHandler(fault *readinessFault) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /_opsloop/fault", fault)
	return mux
}

// The host never supplies the secret. The explicitly approved exec runs this
// binary inside the bound Pod and authenticates over that Pod's loopback only.
func runLocalFault(args []string, m meta) error {
	if len(args) == 0 || args[0] != "inject-readiness-fault" {
		return errors.New("unknown command")
	}
	flags := flag.NewFlagSet("inject-readiness-fault", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	uid := flags.String("pod-uid", "", "exact approved Pod UID")
	seconds := flags.Int64("seconds", 0, "bounded fault duration")
	if flags.Parse(args[1:]) != nil || flags.NArg() != 0 || *uid == "" || *uid != m.PodUID {
		return errors.New("explicit current Pod UID and bounded duration required")
	}
	f, err := faultFromEnvironment(m)
	if err != nil || f == nil || *seconds < 30 || *seconds > int64(f.maxTTL/time.Second) {
		return errors.New("fault capability disabled or duration rejected")
	}
	payload, _ := json.Marshal(struct {
		PodUID          string `json:"podUID"`
		Namespace       string `json:"namespace"`
		DurationSeconds int64  `json:"durationSeconds"`
	}{m.PodUID, m.Namespace, *seconds})
	request, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:18081/_opsloop/fault", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-OpsLoop-Fault-Token", f.token)
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect rejected") }}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("local fault request unconfirmed; inspect current state before retry")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(body) > 4096 || bytes.Contains(body, []byte(f.token)) || response.StatusCode != http.StatusAccepted {
		return errors.New("local fault response rejected or unconfirmed; no automatic replay")
	}
	_, err = os.Stdout.Write(body)
	return err
}
