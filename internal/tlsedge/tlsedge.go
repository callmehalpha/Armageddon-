// Package tlsedge is the server's embedded TLS edge (contract §9.1: no
// external reverse proxy). Three modes:
//
//   - acme: a domain with a certificate from an ACME CA through certmagic,
//     HTTP-01 on port 80 and TLS-ALPN-01 on 443;
//   - self-signed: IP-only installs, with a generated certificate whose
//     SHA-256 fingerprint clients pin on first use;
//   - files: an operator-supplied certificate and key.
package tlsedge

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/caddyserver/certmagic"

	"github.com/callmehalpha/Armageddon-/internal/config"
)

const (
	ModeACME       = "acme"
	ModeSelfSigned = "self-signed"
	ModeFiles      = "files"

	// LetsEncryptStaging is the staging directory used by --acme-staging.
	LetsEncryptStaging = certmagic.LetsEncryptStagingCA
)

// Fingerprint formats the SHA-256 of a DER certificate the way
// `openssl x509 -fingerprint -sha256` does: AA:BB:….
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// NormalizeFingerprint makes user-typed fingerprints comparable: upper case,
// no "SHA256:" prefix, colons between bytes.
func NormalizeFingerprint(fp string) string {
	fp = strings.ToUpper(strings.TrimSpace(fp))
	fp = strings.TrimPrefix(fp, "SHA256:")
	fp = strings.TrimPrefix(fp, "SHA256 ")
	hex := strings.NewReplacer(":", "", " ", "").Replace(fp)
	if len(hex)%2 != 0 {
		return fp
	}
	var parts []string
	for i := 0; i < len(hex); i += 2 {
		parts = append(parts, hex[i:i+2])
	}
	return strings.Join(parts, ":")
}

// FingerprintFile returns the fingerprint of the first certificate in a PEM file.
func FingerprintFile(certPath string) (string, error) {
	b, err := os.ReadFile(certPath)
	if err != nil {
		return "", err
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return "", fmt.Errorf("%s: no PEM certificate", certPath)
	}
	return Fingerprint(blk.Bytes), nil
}

// SelfSignedPaths are where IP-only mode keeps its certificate (contract
// §2.4: keys/ holds the server's keys).
func SelfSignedPaths(dataDir string) (cert, key string) {
	d := filepath.Join(dataDir, "keys")
	return filepath.Join(d, "tls-selfsigned.crt"), filepath.Join(d, "tls-selfsigned.key")
}

// GenerateSelfSigned writes a self-signed certificate for hosts (IP
// addresses or names) and returns its fingerprint. The certificate is its
// own CA (BasicConstraints CA:TRUE, KeyUsageCertSign), so clients can use the
// pinned file directly as a trust anchor, for example git's http.sslCAInfo.
func GenerateSelfSigned(certPath, keyPath string, hosts []string, validFor time.Duration) (string, error) {
	if len(hosts) == 0 {
		return "", errors.New("self-signed certificate needs at least one IP address or host name")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return "", err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: hosts[0], Organization: []string{"Armageddon self-signed"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validFor),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		return "", err
	}
	if err := writeFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		return "", err
	}
	if err := writeFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return "", err
	}
	return Fingerprint(der), nil
}

func writeFile(p string, b []byte, mode os.FileMode) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Mode returns the effective TLS mode of a configuration.
func Mode(cfg *config.Server) string {
	if cfg.TLSMode != "" {
		return cfg.TLSMode
	}
	if cfg.TLSCert != "" {
		return ModeFiles
	}
	return ""
}

// ACME builds the certmagic configuration for acme mode without contacting
// the CA. Certificates and the ACME account live under <data>/keys/acme.
func ACME(cfg *config.Server) (*certmagic.Config, *certmagic.ACMEIssuer, error) {
	if cfg.Domain == "" {
		return nil, nil, errors.New("acme mode needs a domain (server init --domain)")
	}
	if cfg.ACMEEmail == "" {
		return nil, nil, errors.New("acme mode needs an email address (server init --acme-email)")
	}
	storage := &certmagic.FileStorage{Path: filepath.Join(cfg.DataDir, "keys", "acme")}
	var magic *certmagic.Config
	cache := certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) { return magic, nil },
	})
	magic = certmagic.New(cache, certmagic.Config{Storage: storage})
	ca := cfg.ACMECA
	if ca == "" {
		ca = certmagic.LetsEncryptProductionCA
	}
	issuer := certmagic.NewACMEIssuer(magic, certmagic.ACMEIssuer{
		CA:     ca,
		Email:  cfg.ACMEEmail,
		Agreed: true,
		// TLS-ALPN-01 is answered on the main listener, which must be what
		// the CA reaches on port 443; HTTP-01 on the :80 listener.
		DisableTLSALPNChallenge: port(cfg.Listen) != "443",
		DisableHTTPChallenge:    port(httpListen(cfg)) != "80",
	})
	magic.Issuers = []certmagic.Issuer{issuer}
	return magic, issuer, nil
}

func httpListen(cfg *config.Server) string {
	if cfg.HTTPListen != "" {
		return cfg.HTTPListen
	}
	return ":80"
}

func port(addr string) string {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return p
}

// Listen opens the server's main listener according to the TLS mode. In
// acme mode it also starts the port-80 listener (HTTP-01 and a redirect to
// HTTPS) and begins certificate management in the background; both stop
// when ctx ends.
func Listen(ctx context.Context, cfg *config.Server) (net.Listener, error) {
	switch Mode(cfg) {
	case "":
		return net.Listen("tcp", cfg.Listen)
	case ModeFiles, ModeSelfSigned:
		cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			return nil, fmt.Errorf("TLS certificate: %w", err)
		}
		return tls.Listen("tcp", cfg.Listen, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}})
	case ModeACME:
		magic, issuer, err := ACME(cfg)
		if err != nil {
			return nil, err
		}
		tc := magic.TLSConfig()
		tc.NextProtos = append([]string{"h2", "http/1.1"}, tc.NextProtos...)
		tc.MinVersion = tls.VersionTLS12
		ln, err := tls.Listen("tcp", cfg.Listen, tc)
		if err != nil {
			return nil, err
		}
		hsrv := &http.Server{Addr: httpListen(cfg), ReadHeaderTimeout: 10 * time.Second,
			Handler: issuer.HTTPChallengeHandler(redirect(cfg))}
		go func() {
			if err := hsrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("ACME HTTP listener on %s: %v (HTTP-01 challenges and the HTTPS redirect are unavailable)", hsrv.Addr, err)
			}
		}()
		go func() {
			<-ctx.Done()
			hsrv.Close()
		}()
		if err := magic.ManageAsync(ctx, []string{cfg.Domain}); err != nil {
			ln.Close()
			hsrv.Close()
			return nil, fmt.Errorf("ACME certificate management for %s: %w", cfg.Domain, err)
		}
		return ln, nil
	}
	return nil, fmt.Errorf("unknown tls_mode %q", cfg.TLSMode)
}

func redirect(cfg *config.Server) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte("ok\n"))
			return
		}
		target := "https://" + cfg.Domain
		if p := port(cfg.Listen); p != "" && p != "443" {
			target += ":" + p
		}
		http.Redirect(w, r, target+r.URL.RequestURI(), http.StatusMovedPermanently)
	})
}

// HealthURL is where a local process (update health check, doctor) can
// reach /healthz, plus the TLS server name to use and whether to skip
// certificate verification (always on loopback).
func HealthURL(cfg *config.Server) (url, serverName string) {
	host, p, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		p = "8080"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	scheme := "http"
	if Mode(cfg) != "" {
		scheme = "https"
	}
	if Mode(cfg) == ModeACME {
		serverName = cfg.Domain
	}
	return fmt.Sprintf("%s://%s/healthz", scheme, net.JoinHostPort(host, p)), serverName
}

// HealthClient returns an HTTP client for HealthURL. It skips certificate
// verification: it only ever talks to the local server.
func HealthClient(serverName string, timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: serverName}, // #nosec G402 -- loopback health check
	}}
}

// ErrCertChanged is returned when a pinned server presents a different
// certificate.
var ErrCertChanged = errors.New("the server's TLS certificate has changed")

// CertChangedError carries both fingerprints.
type CertChangedError struct{ Pinned, Presented string }

func (e *CertChangedError) Error() string {
	return fmt.Sprintf("%v: pinned %s, server presented %s. Someone may be intercepting the connection. "+
		"If the administrator replaced the certificate, compare the new fingerprint with `armageddon server fingerprint` "+
		"on the server, then run `armageddon login <url> --fingerprint <new>`", ErrCertChanged, e.Pinned, e.Presented)
}

func (e *CertChangedError) Unwrap() error { return ErrCertChanged }

// ClientConfig is the client side of trust-on-first-use pinning. With a pin,
// the server's leaf certificate must have exactly that fingerprint (the
// usual chain checks are replaced by the pin). Without one, normal
// verification against the system roots applies.
func ClientConfig(serverName, pin string) *tls.Config {
	if pin == "" {
		return &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
	}
	want := NormalizeFingerprint(pin)
	return &tls.Config{
		ServerName:         serverName,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // replaced by the pin check below
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("server presented no certificate")
			}
			got := Fingerprint(cs.PeerCertificates[0].Raw)
			if got != want {
				return &CertChangedError{Pinned: want, Presented: got}
			}
			return nil
		},
	}
}

// Probe connects to addr (host:port) and returns the server's leaf
// certificate and whether it verifies against the system roots for
// serverName. It is used once, at login, to decide whether to pin.
func Probe(addr, serverName string, timeout time.Duration) (*x509.Certificate, bool, error) {
	d := &net.Dialer{Timeout: timeout}
	conn, err := tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: serverName, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) // #nosec G402 -- verified below
	if err != nil {
		return nil, false, err
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, false, errors.New("server presented no certificate")
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	_, verr := certs[0].Verify(x509.VerifyOptions{DNSName: serverName, Intermediates: inter})
	return certs[0], verr == nil, nil
}
