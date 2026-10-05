package main

// Install and operations commands (contract §9.2–§9.3, plan M5): server
// init with TLS, backup/restore, update/rollback, uninstall, doctor and
// the release tooling used by the release workflow.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/backup"
	"github.com/callmehalpha/Armageddon-/internal/config"
	"github.com/callmehalpha/Armageddon-/internal/doctor"
	"github.com/callmehalpha/Armageddon-/internal/lifecycle"
	"github.com/callmehalpha/Armageddon-/internal/release"
	"github.com/callmehalpha/Armageddon-/internal/store"
	"github.com/callmehalpha/Armageddon-/internal/sysuser"
	"github.com/callmehalpha/Armageddon-/internal/tlsedge"
)

var opsCommands = map[string]func(context.Context, []string) error{
	"init":        serverInit,
	"fingerprint": serverFingerprint,
	"migrate":     serverMigrate,
	"backup":      serverBackup,
	"restore":     serverRestore,
	"update":      serverUpdate,
	"rollback":    serverRollback,
	"uninstall":   serverUninstall,
	"health":      serverHealth,
}

// serverHealth waits until the local server answers /healthz (installer,
// scripts). It reads the listen address and TLS mode from the config.
func serverHealth(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("server health", flag.ExitOnError)
	data := fs.String("data", config.DefaultDataDir(), "data directory")
	timeout := fs.Duration("timeout", 10*time.Second, "how long to wait")
	fs.Parse(args)
	cfg, err := config.Load(*data)
	if err != nil {
		return err
	}
	return lifecycle.WaitHealthy(ctx, cfg, *timeout)
}

func serverInit(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("server init", flag.ExitOnError)
	data := fs.String("data", config.DefaultDataDir(), "data directory")
	listen := fs.String("listen", "", "listen address (default :443 with TLS, :8080 without)")
	public := fs.String("public-url", "", "URL clients use to reach this server")
	cert := fs.String("tls-cert", "", "TLS certificate file (operator-supplied)")
	key := fs.String("tls-key", "", "TLS key file (operator-supplied)")
	domain := fs.String("domain", "", "domain name for an ACME (Let's Encrypt) certificate")
	email := fs.String("acme-email", "", "ACME account email")
	staging := fs.Bool("acme-staging", false, "use the Let's Encrypt staging CA (for testing)")
	ca := fs.String("acme-ca", "", "ACME directory URL (default: Let's Encrypt production)")
	httpListen := fs.String("http-listen", "", "ACME HTTP-01 and redirect listener (default :80)")
	ipOnly := fs.Bool("ip-only", false, "no domain: generate a self-signed certificate and print its fingerprint")
	ips := fs.String("ip", "", "comma-separated IP addresses or names for the self-signed certificate (default: detected)")
	newCert := fs.Bool("new-cert", false, "replace an existing self-signed certificate (clients must re-pin)")
	owner := fs.String("owner", "", "user that runs the server; server files are chowned to it (installer sets this)")
	fs.Parse(args)
	modes := 0
	for _, b := range []bool{*domain != "", *ipOnly, *cert != ""} {
		if b {
			modes++
		}
	}
	if modes > 1 {
		return errors.New("choose one of --domain, --ip-only and --tls-cert")
	}
	cfg, err := config.Load(*data)
	if err != nil {
		cfg = config.Default(*data)
	}
	tlsListen := func() {
		if *listen != "" {
			cfg.Listen = *listen
		} else if cfg.Listen == ":8080" || cfg.Listen == "" || strings.HasPrefix(cfg.Listen, "127.0.0.1:") {
			cfg.Listen = ":443"
		}
	}
	portSuffix := func() string {
		if _, p, err := net.SplitHostPort(cfg.Listen); err == nil && p != "443" {
			return ":" + p
		}
		return ""
	}
	fingerprint := ""
	switch {
	case *domain != "":
		if *email == "" {
			return errors.New("--domain needs --acme-email")
		}
		tlsListen()
		cfg.TLSMode, cfg.Domain, cfg.ACMEEmail, cfg.TLSCert, cfg.TLSKey = tlsedge.ModeACME, *domain, *email, "", ""
		cfg.ACMECA = *ca
		if *staging {
			cfg.ACMECA = tlsedge.LetsEncryptStaging
		}
		cfg.HTTPListen = *httpListen
		if _, _, err := tlsedge.ACME(cfg); err != nil {
			return err
		}
		cfg.PublicURL = "https://" + *domain + portSuffix()
	case *ipOnly:
		tlsListen()
		hosts := splitList(*ips)
		if len(hosts) == 0 {
			hosts = detectIPs()
		}
		certPath, keyPath := tlsedge.SelfSignedPaths(*data)
		if _, err := os.Stat(certPath); err == nil && !*newCert && cfg.TLSMode == tlsedge.ModeSelfSigned {
			fingerprint, err = tlsedge.FingerprintFile(certPath)
			if err != nil {
				return err
			}
			fmt.Println("Keeping the existing self-signed certificate (pass --new-cert to replace it).")
		} else {
			fingerprint, err = tlsedge.GenerateSelfSigned(certPath, keyPath, hosts, 10*365*24*time.Hour)
			if err != nil {
				return err
			}
		}
		cfg.TLSMode, cfg.TLSCert, cfg.TLSKey, cfg.Domain = tlsedge.ModeSelfSigned, certPath, keyPath, ""
		cfg.PublicURL = "https://" + hostForURL(hosts[0]) + portSuffix()
	case *cert != "":
		if *key == "" {
			return errors.New("--tls-cert needs --tls-key")
		}
		tlsListen()
		cfg.TLSMode, cfg.TLSCert, cfg.TLSKey = tlsedge.ModeFiles, *cert, *key
		fingerprint, _ = tlsedge.FingerprintFile(*cert)
	default:
		if *listen != "" {
			cfg.Listen = *listen
		}
	}
	if *public != "" {
		cfg.PublicURL = strings.TrimRight(*public, "/")
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	if *owner != "" {
		if err := chownServerFiles(*data, *owner); err != nil {
			return err
		}
	}
	fmt.Printf("Wrote %s\n", config.Path(*data))
	switch tlsedge.Mode(cfg) {
	case tlsedge.ModeACME:
		ca := cfg.ACMECA
		if ca == "" {
			ca = "Let's Encrypt (production)"
		}
		fmt.Printf("TLS: ACME certificate for %s from %s, requested when the server starts.\n"+
			"     Ports 80 and 443 must reach this machine and DNS for %s must point here.\n", cfg.Domain, ca, cfg.Domain)
	case tlsedge.ModeSelfSigned, tlsedge.ModeFiles:
		fmt.Printf("TLS: certificate %s\n\n  Certificate SHA-256 fingerprint:\n    %s\n\n"+
			"  `armageddon login` shows this fingerprint on first use; compare it before trusting.\n", cfg.TLSCert, fingerprint)
	default:
		fmt.Println("TLS: none (plain HTTP). Use --domain or --ip-only for anything reachable from other machines.")
	}
	fmt.Printf("\nServer URL: %s\n", cfg.PublicURL)
	fmt.Printf("Start (or restart) the server: systemctl restart armageddon   (or: armageddon server run --data %s)\n", *data)
	fmt.Println("The one-time setup URL for the first admin is printed in the server log (journalctl -u armageddon).")
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func hostForURL(h string) string {
	if ip := net.ParseIP(h); ip != nil && ip.To4() == nil {
		return "[" + h + "]"
	}
	return h
}

// detectIPs returns the address of the default route's interface (no
// packet is sent) plus loopback.
func detectIPs() []string {
	var out []string
	if c, err := net.Dial("udp", "192.0.2.1:9"); err == nil {
		out = append(out, c.LocalAddr().(*net.UDPAddr).IP.String())
		c.Close()
	}
	return append(out, "127.0.0.1")
}

// chownServerFiles hands the server-owned files to the server user (the
// helper layout runs the server unprivileged, contract §9.2).
func chownServerFiles(data, owner string) error {
	u, err := user.Lookup(owner)
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	paths := []string{data, config.Path(data), filepath.Join(data, "armageddon.db"), filepath.Join(data, "armageddon.db-wal"), filepath.Join(data, "armageddon.db-shm")}
	for _, p := range paths {
		if err := os.Lchown(p, uid, gid); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	// Server-only trees: keys and, per workspace, checkpoints.git and journal.
	trees := []string{filepath.Join(data, "keys"), filepath.Join(data, "backups")}
	ents, _ := os.ReadDir(filepath.Join(data, "workspaces"))
	for _, e := range ents {
		root := filepath.Join(data, "workspaces", e.Name())
		trees = append(trees, filepath.Join(root, "checkpoints.git"), filepath.Join(root, "journal"))
	}
	for _, t := range trees {
		filepath.Walk(t, func(p string, _ os.FileInfo, err error) error {
			if err == nil {
				os.Lchown(p, uid, gid)
			}
			return nil
		})
	}
	return nil
}

func serverFingerprint(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("server fingerprint", flag.ExitOnError)
	data := fs.String("data", config.DefaultDataDir(), "data directory")
	fs.Parse(args)
	cfg, err := config.Load(*data)
	if err != nil {
		return err
	}
	switch tlsedge.Mode(cfg) {
	case tlsedge.ModeSelfSigned, tlsedge.ModeFiles:
		fp, err := tlsedge.FingerprintFile(cfg.TLSCert)
		if err != nil {
			return err
		}
		fmt.Println(fp)
		return nil
	case tlsedge.ModeACME:
		return fmt.Errorf("%s uses an ACME certificate trusted through the system roots; clients do not pin it", cfg.Domain)
	}
	return errors.New("the server has no TLS certificate (plain HTTP)")
}

func serverMigrate(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("server migrate", flag.ExitOnError)
	data := fs.String("data", config.DefaultDataDir(), "data directory")
	fs.Parse(args)
	st, err := store.Open(filepath.Join(*data, "armageddon.db"))
	if err != nil {
		return err
	}
	fmt.Printf("database schema is at version %d\n", store.SchemaVersion())
	return st.Close()
}

func passphrase(file string) ([]byte, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		return []byte(strings.TrimRight(string(b), "\r\n")), nil
	}
	return []byte(os.Getenv("ARMAGEDDON_BACKUP_PASSPHRASE")), nil
}

func serverBackup(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("server backup", flag.ExitOnError)
	data := fs.String("data", config.DefaultDataDir(), "data directory")
	to := fs.String("to", "", "directory to write the backup into (default <data>/backups)")
	pf := fs.String("passphrase-file", "", "include keys/, encrypted with the passphrase in this file (or $ARMAGEDDON_BACKUP_PASSPHRASE)")
	fs.Parse(args)
	pw, err := passphrase(*pf)
	if err != nil {
		return err
	}
	dir, err := backup.Run(backup.Options{DataDir: *data, To: *to, Passphrase: pw, ServerVersion: version, Log: os.Stdout})
	if err != nil {
		return err
	}
	if len(pw) == 0 {
		fmt.Println("keys not included (no passphrase): a restore generates new server keys")
	}
	fmt.Printf("Backup written to %s\n", dir)
	return nil
}

func serverRestore(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("server restore", flag.ExitOnError)
	data := fs.String("data", config.DefaultDataDir(), "fresh data directory to restore into")
	pf := fs.String("passphrase-file", "", "passphrase for the encrypted keys (or $ARMAGEDDON_BACKUP_PASSPHRASE)")
	owner := fs.String("owner", "", "user that runs the server; server files are chowned to it")
	fs.Parse(reorder(args))
	if fs.NArg() != 1 {
		return errors.New("usage: armageddon server restore <backup-dir> [--data DIR] [--passphrase-file F]")
	}
	pw, err := passphrase(*pf)
	if err != nil {
		return err
	}
	rep, err := backup.Restore(backup.RestoreOptions{Backup: fs.Arg(0), DataDir: *data, Passphrase: pw, Log: os.Stdout})
	if rep != nil {
		fmt.Printf("%d workspaces restored, %d failed\n", len(rep.Restored), len(rep.Failed))
	}
	if err != nil {
		return err
	}
	if *owner != "" {
		if err := chownServerFiles(*data, *owner); err != nil {
			return err
		}
	}
	fmt.Println("Start the server: systemctl start armageddon. Devices that held a lease must take it again (the epoch moved).")
	return nil
}

func layoutFlags(fs *flag.FlagSet) (*string, *string) {
	return fs.String("data", config.DefaultDataDir(), "data directory"),
		fs.String("prefix", "", "root of the installed layout (testing)")
}

func serverUpdate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("server update", flag.ExitOnError)
	data, prefix := layoutFlags(fs)
	to := fs.String("to", "", "version to install (default: latest)")
	bundle := fs.String("bundle", "", "offline release bundle (.tar)")
	base := fs.String("base", "", "releases URL (default "+lifecycle.DefaultReleaseBase+")")
	pub := fs.String("pubkey", "", "minisign public key (default: the key built into this binary)")
	unsigned := fs.Bool("allow-unsigned", false, "skip the signature check (development builds only)")
	timeout := fs.Duration("health-timeout", 60*time.Second, "how long the new version has to answer /healthz")
	fs.Parse(args)
	l := lifecycle.Layout{Prefix: *prefix, DataDir: *data}
	u := &lifecycle.Updater{Layout: l, Services: &lifecycle.Systemd{Layout: l}, HealthTimeout: *timeout, Log: os.Stdout,
		Source: &lifecycle.Source{Bundle: *bundle, Base: *base, Version: *to, PublicKey: *pub, AllowUnsigned: *unsigned}}
	_, err := u.Update(ctx)
	return err
}

func serverRollback(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("server rollback", flag.ExitOnError)
	data, prefix := layoutFlags(fs)
	keep := fs.Bool("keep-db", false, "keep the current database (only if the update did not change the schema)")
	fs.Parse(args)
	l := lifecycle.Layout{Prefix: *prefix, DataDir: *data}
	u := &lifecycle.Updater{Layout: l, Services: &lifecycle.Systemd{Layout: l}, Log: os.Stdout}
	step, err := u.Rollback(ctx, *keep)
	if err != nil {
		return err
	}
	fmt.Printf("Rolled back from %s to %s", step.To, step.From)
	if !*keep && step.DBBackup != "" {
		fmt.Printf(" with the database from %s", step.DBBackup)
	}
	fmt.Println()
	return nil
}

func serverUninstall(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("server uninstall", flag.ExitOnError)
	data, prefix := layoutFlags(fs)
	purge := fs.Bool("purge", false, "also delete all data, configuration and workspace users (asks for a typed confirmation)")
	fs.Parse(args)
	l := lifecycle.Layout{Prefix: *prefix, DataDir: *data}
	u := &lifecycle.Uninstaller{Layout: l, Log: os.Stdout, Passwd: "/etc/passwd"}
	if *prefix == "" {
		u.Systemctl = func(a ...string) error {
			out, err := exec.Command("systemctl", a...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("systemctl %s: %v: %s", strings.Join(a, " "), err, strings.TrimSpace(string(out)))
			}
			return nil
		}
		u.DeleteUser = func(n string) error {
			if n != "armageddon" {
				return sysuser.Delete(n)
			}
			return exec.Command("userdel", n).Run()
		}
	}
	confirmed := false
	if *purge {
		confirmed = lifecycle.Confirm(os.Stdin, os.Stdout, *data)
	}
	return u.Run(*purge, confirmed)
}

// helperSupported reports whether this binary has the privileged helper
// (Phase 2), the same test install.sh uses.
func helperSupported() bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	cmd := exec.Command(self, "helper", "--help")
	return cmd.Run() == nil
}

func doctorCmd(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	data := fs.String("data", config.DefaultDataDir(), "data directory")
	repair := fs.Bool("repair", false, "fix what can be fixed safely (checkpoint refs from the database)")
	asJSON := fs.Bool("json", false, "JSON output")
	sock := fs.String("helper-socket", "", "privileged helper socket (default /run/armageddon/helper.sock)")
	sample := fs.Int("fsck-sample", 3, "number of workspaces to fsck")
	fs.Parse(args)
	rs := doctor.Run(doctor.Env{DataDir: *data, Repair: *repair, HelperSocket: *sock, FsckSample: *sample, HelperSupported: helperSupported()})
	if *asJSON {
		b, _ := json.MarshalIndent(rs, "", "  ")
		fmt.Println(string(b))
	} else {
		doctor.Print(os.Stdout, rs)
	}
	if doctor.Failed(rs) {
		return errors.New("doctor found problems (see the remedies above)")
	}
	return nil
}

func releaseCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: armageddon release verify|manifest|sign|pubkey|keygen")
	}
	fs := flag.NewFlagSet("release "+args[0], flag.ExitOnError)
	switch args[0] {
	case "verify":
		pub := fs.String("pubkey", "", "minisign public key (default: built in)")
		pubFile := fs.String("pubkey-file", "", "minisign .pub file")
		dir := fs.String("dir", "", "directory with the artefacts (default: the manifest's directory)")
		fs.Parse(reorder(args[1:]))
		if fs.NArg() != 2 {
			return errors.New("usage: armageddon release verify <manifest.json> <manifest.json.minisig> [--pubkey KEY | --pubkey-file F] [--dir DIR]")
		}
		key := *pub
		if *pubFile != "" {
			b, err := os.ReadFile(*pubFile)
			if err != nil {
				return err
			}
			key = string(b)
		}
		mb, err := os.ReadFile(fs.Arg(0))
		if err != nil {
			return err
		}
		sig, err := os.ReadFile(fs.Arg(1))
		if err != nil {
			return err
		}
		m, err := release.Verify(mb, sig, key)
		if err != nil {
			return err
		}
		fmt.Printf("manifest for %s: signature OK\n", m.Version)
		d := *dir
		if d == "" {
			d = filepath.Dir(fs.Arg(0))
		}
		checked := 0
		for _, f := range m.Files {
			p := filepath.Join(d, f.Name)
			if _, err := os.Stat(p); err != nil {
				continue
			}
			if err := m.VerifyFile(f.Name, p); err != nil {
				return err
			}
			fmt.Printf("  %s: sha256 OK\n", f.Name)
			checked++
		}
		fmt.Printf("%d of %d listed artefacts present and verified\n", checked, len(m.Files))
		return nil
	case "manifest":
		dir := fs.String("dir", "dist", "directory with the artefacts")
		ver := fs.String("version", version, "release version")
		com := fs.String("commit", commit, "commit")
		minFrom := fs.String("min-upgrade-from", "0.1.0", "oldest version that may upgrade directly")
		cs := fs.String("code-server", "", "JSON file with the pinned code-server entry")
		fs.Parse(args[1:])
		var pin *release.CodeServer
		if *cs != "" {
			b, err := os.ReadFile(*cs)
			if err != nil {
				return err
			}
			pin = &release.CodeServer{}
			if err := json.Unmarshal(b, pin); err != nil {
				return fmt.Errorf("%s: %w", *cs, err)
			}
			for _, f := range pin.Files {
				if len(f.SHA256) != 64 {
					return fmt.Errorf("%s: code-server %s/%s has no sha256", *cs, f.OS, f.Arch)
				}
			}
		}
		m, err := release.Build(*dir, *ver, *com, *minFrom, store.SchemaVersion(), pin)
		if err != nil {
			return err
		}
		out := filepath.Join(*dir, "manifest.json")
		if err := os.WriteFile(out, m.Marshal(), 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s (%d files, schema %d)\n", out, len(m.Files), m.SchemaVersion)
		return nil
	case "sign":
		fs.Parse(args[1:])
		if fs.NArg() != 1 {
			return errors.New("usage: MINISIGN_SECRET_KEY=… MINISIGN_PASSWORD=… armageddon release sign <manifest.json>")
		}
		secret := os.Getenv("MINISIGN_SECRET_KEY")
		if secret == "" {
			return errors.New("MINISIGN_SECRET_KEY is not set")
		}
		mb, err := os.ReadFile(fs.Arg(0))
		if err != nil {
			return err
		}
		sig, err := release.Sign(mb, []byte(secret), os.Getenv("MINISIGN_PASSWORD"))
		if err != nil {
			return err
		}
		if err := os.WriteFile(fs.Arg(0)+".minisig", sig, 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s.minisig\n", fs.Arg(0))
		return nil
	case "pubkey":
		secret := os.Getenv("MINISIGN_SECRET_KEY")
		if secret == "" {
			return errors.New("MINISIGN_SECRET_KEY is not set")
		}
		pk, err := release.PublicKeyOf([]byte(secret), os.Getenv("MINISIGN_PASSWORD"))
		if err != nil {
			return err
		}
		fmt.Println(pk)
		return nil
	case "keygen":
		out := fs.String("out", "armageddon-release", "path prefix for the .key and .pub files")
		fs.Parse(args[1:])
		pw := os.Getenv("MINISIGN_PASSWORD")
		if pw == "" {
			return errors.New("set MINISIGN_PASSWORD to the password that will protect the secret key")
		}
		secret, pub, err := release.GenerateKey(pw)
		if err != nil {
			return err
		}
		if err := os.WriteFile(*out+".key", secret, 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(*out+".pub", []byte("untrusted comment: armageddon release public key\n"+pub+"\n"), 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s.key (secret: store it as the MINISIGN_SECRET_KEY repository secret, never commit it) and %s.pub\n", *out, *out)
		return nil
	}
	return fmt.Errorf("unknown release command %q", args[0])
}
