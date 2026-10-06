package agent

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/tlsedge"
)

// serveWithNewCert starts an HTTPS server on addr ("127.0.0.1:0" for any)
// with a freshly generated self-signed certificate.
func serveWithNewCert(t *testing.T, addr string) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	c, k := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	fp, err := tlsedge.GenerateSelfSigned(c, k, []string{"127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.LoadX509KeyPair(c, k)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	if addr != "" {
		srv.Listener.Close()
		var ln net.Listener
		for i := 0; i < 50; i++ { // the previous server may still hold the port briefly
			if ln, err = net.Listen("tcp", addr); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		srv.Listener = ln
	}
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, fp
}

func TestTrustOnFirstUse(t *testing.T) {
	t.Setenv("ARMAGEDDON_CONFIG_DIR", t.TempDir())
	srv, fp := serveWithNewCert(t, "")
	url := srv.URL

	var out strings.Builder
	// A wrong expected fingerprint is refused.
	if _, err := trustServer(url, strings.Repeat("AB:", 31)+"AB", nil, false, &out); !errors.Is(err, tlsedge.ErrCertChanged) {
		t.Fatalf("wrong --fingerprint: %v", err)
	}
	// Interactive "no" is refused.
	if _, err := trustServer(url, "", strings.NewReader("n\n"), true, &out); err == nil {
		t.Fatal("declined certificate trusted")
	}
	// First use, non-interactive: pinned and shown for comparison.
	pin, err := trustServer(url, "", nil, false, &out)
	if err != nil || pin != fp {
		t.Fatalf("pin %q (want %q): %v", pin, fp, err)
	}
	if !strings.Contains(out.String(), fp) {
		t.Fatal("fingerprint not shown for comparison")
	}
	if b, err := os.ReadFile(pinnedCertPath()); err != nil || !strings.Contains(string(b), "BEGIN CERTIFICATE") {
		t.Fatal("pinned certificate not saved for git", err)
	}
	if err := (&Config{Server: url, DeviceID: "d", CertFingerprint: pin}).save(); err != nil {
		t.Fatal(err)
	}
	resp, err := httpClient(url, pin, 5*time.Second).Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// The certificate changes at the same address.
	addr := srv.Listener.Addr().String()
	srv.Close()
	_, newFP := serveWithNewCert(t, addr)
	if _, err := httpClient(url, pin, 5*time.Second).Get(url); !errors.Is(err, tlsedge.ErrCertChanged) {
		t.Fatalf("changed certificate accepted by the client: %v", err)
	}
	if _, err := trustServer(url, "", nil, false, &out); !errors.Is(err, tlsedge.ErrCertChanged) {
		t.Fatalf("login again silently re-pinned a changed certificate: %v", err)
	}
	// Confirming the new fingerprint explicitly re-pins.
	if pin, err := trustServer(url, newFP, nil, false, &out); err != nil || pin != newFP {
		t.Fatal(pin, err)
	}
}
