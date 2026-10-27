package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Export only the immutable manifest-declared files, never an arbitrary tree or
// credentials. Compressed ASCII frames survive CRI logging; the host must verify
// the complete archive and every member hash before accepting the evidence.
func exportObserverRun(w io.Writer, dir, run string) error {
	read := func(name string, limit int) ([]byte, error) {
		if filepath.Base(name) != name || name == "." || name == ".." {
			return nil, errors.New("observer export: invalid member")
		}
		f, err := os.OpenInRoot(dir, name)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > int64(limit) {
			return nil, errors.New("observer export: member size/type invalid")
		}
		b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
		if err != nil || len(b) > limit {
			return nil, errors.New("observer export: member read incomplete")
		}
		return b, nil
	}
	raw, err := read("manifest.json", 1<<20)
	if err != nil {
		return err
	}
	complete, err := read("COMPLETE", 1<<20)
	if err != nil || !bytes.Equal(raw, complete) {
		return errors.New("observer export: completed manifest unavailable")
	}
	var manifest observerManifest
	if strictDecode(raw, &manifest, true) != nil || manifest.RunID != run || manifest.State != "complete" || len(manifest.Files) > 12000 {
		return errors.New("observer export: invalid manifest")
	}
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tarw := tar.NewWriter(gz)
	names := map[string]bool{}
	total := 0
	add := func(name string, data []byte) error {
		if names[name] || filepath.Base(name) != name {
			return errors.New("observer export: duplicate/invalid member")
		}
		names[name] = true
		total += len(data)
		if total > 66<<20 {
			return errors.New("observer export: total input limit")
		}
		if err := tarw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tarw.Write(data)
		return err
	}
	for _, member := range manifest.Files {
		body, err := read(member.Ref, maxInputBytes)
		if err != nil {
			return err
		}
		if len(body) != member.Bytes || digestBytes(body) != member.SHA256 {
			return errors.New("observer export: member hash mismatch")
		}
		if err = add(member.Ref, body); err != nil {
			return err
		}
	}
	if err = add("manifest.json", raw); err != nil {
		return err
	}
	if err = add("COMPLETE", complete); err != nil {
		return err
	}
	if err = tarw.Close(); err != nil {
		return err
	}
	if err = gz.Close(); err != nil {
		return err
	}
	archive := output.Bytes()
	if len(archive) > 4<<20 {
		return errors.New("observer export: compressed log budget exceeded; no truncated success")
	}
	const chunk = 6144
	count := (len(archive) + chunk - 1) / chunk
	encoder := json.NewEncoder(w)
	for i := 0; i < count; i++ {
		end := min((i+1)*chunk, len(archive))
		frame := struct {
			Schema string `json:"schema"`
			Run    string `json:"runID"`
			SHA256 string `json:"archiveSHA256"`
			Bytes  int    `json:"archiveBytes"`
			Chunks int    `json:"chunks"`
			Index  int    `json:"index"`
			Data   string `json:"base64"`
		}{"opsloop-observer-export/v1", run, digestBytes(archive), len(archive), count, i, base64.StdEncoding.EncodeToString(archive[i*chunk : end])}
		if err = encoder.Encode(frame); err != nil {
			return err
		}
	}
	return nil
}
