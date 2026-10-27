package publicstatus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// PrivateFile does not follow symlinks and never returns contents in errors.
func PrivateFile(path string) ([]byte, error) {
	fd, e := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if e != nil {
		return nil, errors.New("private file unavailable")
	}
	f := os.NewFile(uintptr(fd), "private")
	defer f.Close()
	s, e := f.Stat()
	if e != nil || !s.Mode().IsRegular() || s.Mode().Perm()&0077 != 0 {
		return nil, errors.New("private file permissions")
	}
	stat, ok := s.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return nil, errors.New("private file owner")
	}
	b, e := io.ReadAll(io.LimitReader(f, 65537))
	if e != nil || len(b) > 65536 {
		return nil, errors.New("private file bounds")
	}
	return b, nil
}
func SigningKey(path string) ([]byte, error) {
	b, e := PrivateFile(path)
	if e != nil {
		return nil, e
	}
	if len(b) < 32 || len(b) > 128 {
		return nil, errors.New("invalid signing key")
	}
	for _, c := range b {
		if c < 33 || c > 126 {
			return nil, errors.New("invalid signing key")
		}
	}
	return b, nil
}

type Sequence struct {
	mu   sync.Mutex
	dir  string
	lock *os.File
}

func OpenSequence(dir string, initialize bool) (*Sequence, error) {
	s, e := os.Lstat(dir)
	if e != nil || !s.IsDir() || s.Mode().Perm()&0077 != 0 {
		return nil, errors.New("sequence directory must exist and be private")
	}
	stat, ok := s.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return nil, errors.New("sequence directory owner")
	}
	fd, e := syscall.Open(filepath.Join(dir, "lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if e != nil {
		return nil, errors.New("sequence lock unavailable")
	}
	f := os.NewFile(uintptr(fd), "sequence lock")
	if e = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, errors.New("collector already running")
	}
	state := &Sequence{dir: dir, lock: f}
	path := filepath.Join(dir, "counter")
	if initialize {
		if _, e = os.Lstat(path); !os.IsNotExist(e) {
			state.Close()
			return nil, errors.New("sequence initialization refused")
		}
		if e = state.store(0); e != nil {
			state.Close()
			return nil, e
		}
	}
	if _, e = PrivateFile(path); e != nil {
		state.Close()
		return nil, errors.New("sequence missing: explicit initialization required")
	}
	return state, nil
}
func (s *Sequence) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock != nil {
		_ = s.lock.Close()
		s.lock = nil
	}
}
func (s *Sequence) store(n uint64) error {
	f, e := os.CreateTemp(s.dir, ".counter-")
	if e != nil {
		return errors.New("sequence persistence failed")
	}
	name := f.Name()
	defer os.Remove(name)
	if _, e = f.WriteString(strconv.FormatUint(n, 10)); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil || ce != nil {
		return errors.New("sequence persistence failed")
	}
	if e = os.Rename(name, filepath.Join(s.dir, "counter")); e != nil {
		return errors.New("sequence persistence failed")
	}
	d, e := os.Open(s.dir)
	if e != nil {
		return errors.New("sequence directory sync failed")
	}
	defer d.Close()
	if e = d.Sync(); e != nil {
		return errors.New("sequence directory sync failed")
	}
	return nil
}
func (s *Sequence) Next() (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return 0, errors.New("sequence closed")
	}
	b, e := PrivateFile(filepath.Join(s.dir, "counter"))
	if e != nil {
		return 0, e
	}
	n, e := strconv.ParseUint(string(b), 10, 64)
	if e != nil || n >= uint64(MaxInt) {
		return 0, errors.New("invalid sequence")
	}
	n++
	if e = s.store(n); e != nil {
		return 0, e
	}
	return n, nil
}

type Sender struct {
	Origin, KeyID, KeyFile string
	Sequence               *Sequence
	Client                 *http.Client
}

func (s Sender) Request(ctx context.Context, raw []byte, now time.Time) (*http.Request, error) {
	if _, e := Parse(raw); e != nil {
		return nil, e
	}
	u, e := url.Parse(s.Origin)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return nil, errors.New("HTTPS fixed origin required")
	}
	if len(s.KeyID) < 1 || len(s.KeyID) > 40 {
		return nil, errors.New("key ID invalid")
	}
	for _, c := range s.KeyID {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return nil, errors.New("key ID invalid")
		}
	}
	key, e := SigningKey(s.KeyFile)
	if e != nil {
		return nil, e
	}
	seq, e := s.Sequence.Next()
	if e != nil {
		return nil, e
	}
	r, e := http.NewRequestWithContext(ctx, http.MethodPost, s.Origin+"/api/collect/status", bytes.NewReader(raw))
	if e != nil {
		return nil, errors.New("invalid publish request")
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-OpsLoop-Collector", "home-v1")
	r.Header.Set("X-OpsLoop-Key-Id", s.KeyID)
	r.Header.Set("X-OpsLoop-Sequence", strconv.FormatUint(seq, 10))
	r.Header.Set("X-OpsLoop-Sent-At", strconv.FormatInt(now.UnixMilli(), 10))
	r.Header.Set("X-OpsLoop-Signature", Sign(key, s.Origin, s.KeyID, seq, now.UnixMilli(), raw))
	return r, nil
}
func (s Sender) Send(ctx context.Context, raw []byte, now time.Time) error {
	r, e := s.Request(ctx, raw, now)
	if e != nil {
		return e
	}
	client := http.Client{Timeout: 5 * time.Second}
	if s.Client != nil {
		client = *s.Client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, e := client.Do(r)
	if e != nil {
		return errors.New("status transport failed")
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 512))
	if res.StatusCode != http.StatusAccepted {
		return fmt.Errorf("status publish rejected (HTTP %d)", res.StatusCode)
	}
	return nil
}
