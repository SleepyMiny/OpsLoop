package publicstatus

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/idna"
	"golang.org/x/text/unicode/norm"
)

// Exercise the upgraded x/net client against a local TLS server, including the
// trust boundary. No system trust store or external endpoint is needed.
func TestDependencyHTTP2VerifiedTLS(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("expected HTTP/2, got %s", r.Proto)
		}
		_, _ = io.WriteString(w, "synthetic-h2")
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	transport := server.Client().Transport.(*http.Transport).Clone()
	if transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("test transport disabled certificate verification")
	}
	if _, err := http2.ConfigureTransports(transport); err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	var requests sync.WaitGroup
	for range 8 {
		requests.Go(func() {
			response, err := client.Get(server.URL)
			if err != nil {
				t.Error(err)
				return
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.ProtoMajor != 2 || string(body) != "synthetic-h2" {
				t.Errorf("HTTP/2 response: proto=%s body=%q err=%v", response.Proto, body, err)
			}
		})
	}
	requests.Wait()
	untrusted := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12}}
	if _, err := http2.ConfigureTransports(untrusted); err != nil {
		t.Fatal(err)
	}
	defer untrusted.CloseIdleConnections()
	response, err := (&http.Client{Transport: untrusted, Timeout: 5 * time.Second}).Get(server.URL)
	if response != nil {
		response.Body.Close()
	}
	var authorityError x509.UnknownAuthorityError
	if !errors.As(err, &authorityError) {
		t.Fatalf("untrusted server must fail certificate validation: %v", err)
	}
}

func TestDependencyUnicodeIDNA(t *testing.T) {
	for _, name := range []string{"café.example", "cafe\u0301.example"} {
		ascii, err := idna.Lookup.ToASCII(name)
		if err != nil || ascii != "xn--caf-dma.example" {
			t.Fatalf("IDNA normalization for %q: %q, %v", name, ascii, err)
		}
	}
	if _, err := idna.Lookup.ToASCII("bad\x00.example"); err == nil {
		t.Fatal("IDNA accepted a prohibited hostname character")
	}
	if got := norm.NFC.String("cafe\u0301"); got != "café" || !norm.NFC.IsNormalString(got) {
		t.Fatalf("Unicode NFC regression: %q", got)
	}
}
