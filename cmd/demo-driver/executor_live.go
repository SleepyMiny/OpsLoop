package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// Paths and expected bindings are operator inputs, never learned from API replies.
// This struct is intentionally portable; Linux owns the actual Guard boundary.
type finishLiveConfig struct {
	GuardConfigFile        string `json:"guardConfigFile"`
	GuardBinding           string `json:"guardBinding"`
	GuardRoot              string `json:"guardRoot"`
	Epoch                  string `json:"epoch"`
	GuardAuthorizationFile string `json:"guardAuthorizationFile"`
	AuthorizationFile      string `json:"authorizationFile"`
}

func (c FinishConfig) evidenceKind() string {
	if c.EvidenceKind == "" {
		return "synthetic"
	}
	return c.EvidenceKind
}
func (c FinishConfig) validateMode() error {
	if c.Live == nil {
		if c.Environment != "synthetic-local" || c.Backend != "test-executable" || c.VMURI != "test:///opsloop-p2d" || c.evidenceKind() != "synthetic" {
			return errors.New("finish: synthetic mode boundary rejected")
		}
		return nil
	}
	l := c.Live
	if c.Environment != "lab-live" || c.Backend != "lab-libvirt" || (c.EvidenceKind != "observed" && c.EvidenceKind != "synthetic") || !recoveryHash.MatchString(l.GuardBinding) || !validObserverName(l.Epoch) || c.TimeoutMS > 2000 || c.TimeoutMS < 10 || filepath.Base(c.Executable) != "virsh" {
		return errors.New("finish: explicit bounded lab-libvirt mode and Guard binding required")
	}
	if e := (recoveryTarget{c.VMURI, c.VMUUID, c.Executable, c.ExecutableSHA256}).validate(); e != nil {
		return e
	}
	for _, p := range []string{l.GuardConfigFile, l.GuardRoot, l.GuardAuthorizationFile, l.AuthorizationFile, c.APICAFile} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return errors.New("finish: explicit lab input paths required")
		}
	}
	// The durable Finish journal and Guard target root have distinct custody.
	// A new root never substitutes for the explicitly bound Guard root.
	if c.JournalDir == l.GuardRoot || filepath.Dir(c.JournalDir) == l.GuardRoot {
		return errors.New("finish: journal must not occupy Guard root")
	}
	return nil
}
func (c FinishConfig) finishURL(raw string, api bool) (*url.URL, error) {
	if c.Live == nil {
		return localFinishURL(raw, api)
	}
	return observerURL(raw, api)
}

type finishLiveInputs struct {
	InputSHA256              string `json:"inputSHA256"`
	GuardConfigSHA256        string `json:"guardConfigSHA256"`
	GuardAuthorizationSHA256 string `json:"guardAuthorizationSHA256"`
	AuthorizationSHA256      string `json:"authorizationSHA256"`
	ApprovalReference        string `json:"approvalReference"`
}

func (x *finisher) bindLiveInputs() error {
	if x.c.Live == nil {
		return nil
	}
	inputs, e := finishLiveInputHashes(x.c, x.inputHash)
	if e != nil {
		return e
	}
	if old := x.j.last("live-inputs"); old != nil {
		var prior finishLiveInputs
		if json.Unmarshal(old.Data, &prior) != nil || prior != inputs {
			return errors.New("finish: actual live input bytes changed across reentry")
		}
		return nil
	}
	return x.j.add("live-inputs", inputs)
}

// Companion bytes are immutable and content addressed by the original receipt
// and exact config input. Consumers must verify both hashes; Finalizer does not
// yet consume this schema. This is custody evidence, not target authentication.
func (x *finisher) publishBinding(status string) error {
	event := x.j.last("live-inputs")
	if event == nil {
		return errors.New("finish: missing live input binding")
	}
	b, e := observerLocalFile(x.c.ReceiptFile, 1<<20)
	if e != nil {
		return e
	}
	var inputs finishLiveInputs
	if json.Unmarshal(event.Data, &inputs) != nil {
		return errors.New("finish: invalid live inputs")
	}
	payload := struct {
		Version             string           `json:"version"`
		RunID               string           `json:"runID"`
		Epoch               string           `json:"epoch"`
		URI                 string           `json:"uri"`
		UUID                string           `json:"uuid"`
		GuardRoot           string           `json:"guardRoot"`
		GuardBinding        string           `json:"guardBinding"`
		ConfigBinding       string           `json:"configBinding"`
		Inputs              finishLiveInputs `json:"inputs"`
		ReceiptSHA256       string           `json:"receiptSHA256"`
		Status              string           `json:"status"`
		LiveProof           bool             `json:"liveProof"`
		ConsumerIntegration string           `json:"consumerIntegration"`
	}{"opsloop-finish-binding/v1", x.c.RunID, x.c.Live.Epoch, x.c.VMURI, x.c.VMUUID, x.c.Live.GuardRoot, x.c.Live.GuardBinding, x.c.binding(), inputs, digestBytes(b), status, false, "FINALIZER_UNINTEGRATED"}
	data, _ := json.MarshalIndent(payload, "", "  ")
	if len(x.j.forbidden) > 0 && bytes.Contains(data, x.j.forbidden) {
		return errors.New("finish: credential reflection blocked")
	}
	path := x.c.JournalDir + fmt.Sprintf(".binding-%s-%s.json", payload.ReceiptSHA256, inputs.InputSHA256)
	if e = x.j.boundary("binding-before-write"); e != nil {
		return e
	}
	if e = finishExclusive(path, data); e != nil {
		if !os.IsExist(e) {
			return errors.New("finish: binding persistence failed; receipt alone is insufficient")
		}
		old, re := observerLocalFile(path, 1<<20)
		if re != nil || !bytes.Equal(old, data) {
			return errors.New("finish: binding collision")
		}
		// A prior interrupted write is not durable merely because its bytes exist.
		f, re := os.Open(path)
		if re != nil {
			return re
		}
		re = f.Sync()
		ce := f.Close()
		if re != nil {
			return re
		}
		if ce != nil {
			return ce
		}
		if e = syncFinishDir(filepath.Dir(path)); e != nil {
			return e
		}
	}
	return x.j.add("binding-published", struct{ File, SHA256, ReceiptSHA256 string }{filepath.Base(path), digestBytes(data), payload.ReceiptSHA256})
}
