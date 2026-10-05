package tlsedge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"

	"github.com/callmehalpha/Armageddon-/internal/config"
)

func TestSelfSignedIPOnlyServesPinnedCertificate(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := SelfSignedPaths(dir)
	fp, err := GenerateSelfSigned(certPath, keyPath, []string{"127.0.0.1"}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(keyPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("private key must be 0600: %v %v", fi.Mode(), err)
	}
	if again, _ := FingerprintFile(certPath); again != fp {
		t.Fatalf("fingerprint mismatch %s vs %s", again, fp)
	}
	if len(fp) != 95 || strings.Count(fp, ":") != 31 {
		t.Fatalf("unexpected fingerprint format %q", fp)
	}

	cfg := config.Default(dir)
	cfg.Listen = "127.0.0.1:0"
	cfg.TLSMode, cfg.TLSCert, cfg.TLSKey = ModeSelfSigned, certPath, keyPath
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	serve := func() string {
		ln, err := Listen(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: handler}
		go srv.Serve(ln)
		t.Cleanup(func() { srv.Close() })
		return ln.Addr().String()
	}
	addr := serve()

	// First use: the probe sees an untrusted certificate with the printed fingerprint.
	leaf, trusted, err := Probe(addr, "127.0.0.1", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if trusted {
		t.Fatal("a self-signed certificate must not verify against the system roots")
	}
	if Fingerprint(leaf.Raw) != fp {
		t.Fatal("probe fingerprint differs from the generated one")
	}

	get := func(pin string) error {
		c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: ClientConfig("127.0.0.1", pin)}}
		resp, err := c.Get("https://" + addr + "/")
		if err != nil {
			return err
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil
	}
	// Pinned (also in a sloppy user-typed form): accepted.
	if err := get("sha256:" + strings.ToLower(strings.ReplaceAll(fp, ":", ""))); err != nil {
		t.Fatalf("pinned connection refused: %v", err)
	}
	// Without a pin, normal verification refuses the self-signed certificate.
	if err := get(""); err == nil {
		t.Fatal("unpinned client accepted a self-signed certificate")
	}

	// The certificate changes (reinstall, or an interceptor): refused with a clear error.
	if _, err := GenerateSelfSigned(certPath, keyPath, []string{"127.0.0.1"}, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	addr = serve()
	err = get(fp)
	if !errors.Is(err, ErrCertChanged) {
		t.Fatalf("changed certificate: want ErrCertChanged, got %v", err)
	}
	if !strings.Contains(err.Error(), "armageddon login") {
		t.Fatalf("error should tell the user what to do: %v", err)
	}
}

func TestACMEWiring(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default(dir)
	if _, _, err := ACME(cfg); err == nil {
		t.Fatal("acme without a domain accepted")
	}
	cfg.TLSMode, cfg.Domain, cfg.Listen = ModeACME, "dev.example.com", ":443"
	if _, _, err := ACME(cfg); err == nil {
		t.Fatal("acme without an email accepted")
	}
	cfg.ACMEEmail, cfg.ACMECA = "ops@example.com", LetsEncryptStaging
	magic, issuer, err := ACME(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if issuer.CA != LetsEncryptStaging || issuer.Email != "ops@example.com" || !issuer.Agreed {
		t.Fatalf("issuer not wired: %+v", issuer)
	}
	if issuer.DisableHTTPChallenge || issuer.DisableTLSALPNChallenge {
		t.Fatal("both challenges must be enabled on :80/:443")
	}
	if len(magic.Issuers) != 1 {
		t.Fatal("issuer not installed")
	}
	if fs, ok := magic.Storage.(*certmagic.FileStorage); !ok || fs.Path != filepath.Join(dir, "keys", "acme") {
		t.Fatalf("ACME storage should live under keys/acme, got %#v", magic.Storage)
	}
	cfg.ACMECA = ""
	if _, issuer, _ = ACME(cfg); issuer.CA != certmagic.LetsEncryptProductionCA {
		t.Fatalf("default CA should be Let's Encrypt production, got %s", issuer.CA)
	}
	cfg.Listen = ":8443"
	if _, issuer, _ = ACME(cfg); !issuer.DisableTLSALPNChallenge {
		t.Fatal("TLS-ALPN-01 needs the main listener on 443")
	}
	u, sn := HealthURL(cfg)
	if u != "https://127.0.0.1:8443/healthz" || sn != "dev.example.com" {
		t.Fatalf("health URL %s %s", u, sn)
	}
	cfg.TLSMode = ""
	if u, _ := HealthURL(cfg); u != "http://127.0.0.1:8443/healthz" {
		t.Fatal(u)
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	if NormalizeFingerprint("SHA256:abcd") != "AB:CD" || NormalizeFingerprint("ab:cd") != "AB:CD" {
		t.Fatal(NormalizeFingerprint("SHA256:abcd"))
	}
}
