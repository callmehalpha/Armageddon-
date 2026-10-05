package lifecycle

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"aead.dev/minisign"

	"github.com/callmehalpha/Armageddon-/internal/release"
	"github.com/callmehalpha/Armageddon-/internal/store"
)

// testKey is generated per test run; no key is ever committed.
func testKey(t *testing.T) (secret []byte, pub string) {
	t.Helper()
	pk, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret, _ = priv.MarshalText()
	b, _ := pk.MarshalText()
	return secret, string(b)
}

const goodBinary = "#!/bin/sh\nexit 0\n"
const failingMigration = "#!/bin/sh\nif [ \"$1 $2\" = 'server migrate' ]; then echo 'migration 0002: boom' >&2; exit 3; fi\nexit 0\n"

// makeBundle writes a signed release bundle like the release workflow does.
func makeBundle(t *testing.T, secret []byte, version, minFrom, binary string, tamper func(dir string)) string {
	t.Helper()
	dir := t.TempDir()
	bin := release.BinaryName(runtime.GOOS, runtime.GOARCH)
	os.WriteFile(filepath.Join(dir, bin), []byte(binary), 0o755)
	m, err := release.Build(dir, version, "c0ffee", minFrom, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	mb := m.Marshal()
	sig, err := release.Sign(mb, secret, "")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "manifest.json"), mb, 0o644)
	os.WriteFile(filepath.Join(dir, "manifest.json.minisig"), sig, 0o644)
	if tamper != nil {
		tamper(dir)
	}
	out := filepath.Join(t.TempDir(), "armageddon-"+version+".tar")
	f, _ := os.Create(out)
	tw := tar.NewWriter(f)
	for _, n := range []string{"manifest.json", "manifest.json.minisig", bin} {
		b, _ := os.ReadFile(filepath.Join(dir, n))
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg})
		tw.Write(b)
	}
	tw.Close()
	f.Close()
	return out
}

// fakeServices records calls and tracks which version is "running".
type fakeServices struct {
	l       Layout
	calls   []string
	running string
}

func (f *fakeServices) Stop() error { f.calls = append(f.calls, "stop"); f.running = ""; return nil }
func (f *fakeServices) Start() error {
	v, err := f.l.Current()
	f.calls = append(f.calls, "start "+v)
	f.running = v
	return err
}

type env struct {
	l    Layout
	svc  *fakeServices
	pub  string
	key  []byte
	db   *store.Store
	path string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	l := Layout{Prefix: filepath.Join(root, "sys"), DataDir: filepath.Join(root, "data")}
	os.MkdirAll(l.VersionDir("0.1.0"), 0o755)
	os.WriteFile(l.Binary("0.1.0"), []byte(goodBinary), 0o755)
	if err := l.Switch("0.1.0"); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(l.DataDir, "armageddon.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	st.SetSetting("marker", "before-update")
	st.Close()
	key, pub := testKey(t)
	return &env{l: l, svc: &fakeServices{l: l, running: "0.1.0"}, pub: pub, key: key, path: dbPath}
}

func (e *env) updater(bundle string) *Updater {
	return &Updater{Layout: e.l, Services: e.svc, Source: &Source{Bundle: bundle, PublicKey: e.pub},
		Healthy: func(context.Context) error { return nil }}
}

func (e *env) setting(t *testing.T, k string) string {
	t.Helper()
	st, err := store.Open(e.path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	v, _ := st.Setting(k)
	return v
}

func TestInjectedMigrationFailureRollsBack(t *testing.T) {
	e := newEnv(t)
	u := e.updater(makeBundle(t, e.key, "0.2.0", "0.1.0", goodBinary, nil))
	// The new version's migration changes the database, then fails halfway.
	u.Migrate = func(bin, dataDir string) error {
		st, err := store.Open(filepath.Join(dataDir, "armageddon.db"))
		if err != nil {
			return err
		}
		st.SetSetting("marker", "half-migrated")
		st.DB().Exec(`CREATE TABLE from_new_version (x INTEGER)`)
		st.Close()
		return errors.New("migration 0002_new.sql: boom")
	}
	_, err := u.Update(context.Background())
	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("want ErrRolledBack, got %v", err)
	}
	if cur, _ := e.l.Current(); cur != "0.1.0" {
		t.Fatalf("current is %s after rollback", cur)
	}
	if e.svc.running != "0.1.0" {
		t.Fatalf("running %q, calls %v", e.svc.running, e.svc.calls)
	}
	if got := e.setting(t, "marker"); got != "before-update" {
		t.Fatalf("database not restored: marker=%q", got)
	}
	st, _ := store.Open(e.path)
	var n int
	st.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'from_new_version'`).Scan(&n)
	st.Close()
	if n != 0 {
		t.Fatal("the failed migration's table survived the rollback")
	}
	if want := []string{"stop", "start 0.1.0"}; strings.Join(e.svc.calls[len(e.svc.calls)-2:], ",") != strings.Join(want, ",") {
		t.Fatalf("calls %v", e.svc.calls)
	}
	if _, err := os.Stat(e.l.StateFile()); err == nil {
		t.Fatal("a failed update was recorded as rollback-able")
	}
}

func TestMigrationFailureThroughTheRealCommand(t *testing.T) {
	e := newEnv(t)
	// The new binary's `server migrate` exits non-zero (default Migrate execs it).
	u := e.updater(makeBundle(t, e.key, "0.2.0", "", failingMigration, nil))
	_, err := u.Update(context.Background())
	if !errors.Is(err, ErrRolledBack) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want rollback with the migration's error, got %v", err)
	}
	if cur, _ := e.l.Current(); cur != "0.1.0" {
		t.Fatal(cur)
	}
}

func TestHealthCheckFailureRollsBack(t *testing.T) {
	e := newEnv(t)
	u := e.updater(makeBundle(t, e.key, "0.2.0", "", goodBinary, nil))
	u.Migrate = func(string, string) error { return nil }
	u.Healthy = func(context.Context) error {
		if e.svc.running == "0.2.0" {
			return errors.New("no healthy answer within 60s")
		}
		return nil
	}
	if _, err := u.Update(context.Background()); !errors.Is(err, ErrRolledBack) {
		t.Fatal(err)
	}
	if e.svc.running != "0.1.0" {
		t.Fatal(e.svc.running)
	}
}

func TestUpdateThenRollback(t *testing.T) {
	e := newEnv(t)
	u := e.updater(makeBundle(t, e.key, "0.2.0", "0.1.0", goodBinary, nil))
	u.Migrate = func(string, string) error { return nil }
	step, err := u.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cur, _ := e.l.Current(); cur != "0.2.0" || e.svc.running != "0.2.0" || step.DBBackup == "" {
		t.Fatalf("after update: current %s running %s step %+v", cur, e.svc.running, step)
	}
	if b, _ := os.ReadFile(filepath.Join(e.l.VersionDir("0.2.0"), "manifest.json")); len(b) == 0 {
		t.Fatal("manifest not kept with the version")
	}
	// Running the same update again is a no-op.
	if s, err := u.Update(context.Background()); err != nil || s != nil {
		t.Fatalf("re-run: %v %v", s, err)
	}
	st, _ := store.Open(e.path)
	st.SetSetting("marker", "written-by-0.2.0")
	st.Close()
	if _, err := u.Rollback(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if cur, _ := e.l.Current(); cur != "0.1.0" || e.svc.running != "0.1.0" {
		t.Fatal(cur, e.svc.running)
	}
	if got := e.setting(t, "marker"); got != "before-update" {
		t.Fatalf("rollback should restore the pre-update database, marker=%q", got)
	}
	if _, err := u.Rollback(context.Background(), false); err == nil {
		t.Fatal("second rollback without a recorded update accepted")
	}
}

func TestBadReleasesRefusedBeforeStopping(t *testing.T) {
	e := newEnv(t)
	cases := map[string]string{
		"tampered binary": makeBundle(t, e.key, "0.2.0", "", goodBinary, func(dir string) {
			os.WriteFile(filepath.Join(dir, release.BinaryName(runtime.GOOS, runtime.GOARCH)), []byte("#!/bin/sh\n# backdoor\n"), 0o755)
		}),
		"tampered manifest": makeBundle(t, e.key, "0.2.0", "", goodBinary, func(dir string) {
			b, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
			os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(strings.Replace(string(b), "0.2.0", "0.2.1", 1)), 0o644)
		}),
		"min_upgrade_from": makeBundle(t, e.key, "0.3.0", "0.2.0", goodBinary, nil),
	}
	for name, b := range cases {
		e.svc.calls = nil
		if _, err := e.updater(b).Update(context.Background()); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if len(e.svc.calls) != 0 {
			t.Errorf("%s: services touched: %v", name, e.svc.calls)
		}
		if cur, _ := e.l.Current(); cur != "0.1.0" {
			t.Errorf("%s: current %s", name, cur)
		}
	}
	// No public key and no --allow-unsigned: refused.
	u := e.updater(makeBundle(t, e.key, "0.2.0", "", goodBinary, nil))
	u.Source.PublicKey = ""
	old := release.PublicKey
	release.PublicKey = ""
	defer func() { release.PublicKey = old }()
	if _, err := u.Update(context.Background()); !errors.Is(err, release.ErrNoPublicKey) {
		t.Fatalf("want ErrNoPublicKey, got %v", err)
	}
}

func TestSystemdOrder(t *testing.T) {
	l := Layout{Prefix: t.TempDir()}
	os.MkdirAll(l.UnitDir(), 0o755)
	var calls []string
	s := &Systemd{Layout: l, Run: func(a ...string) error { calls = append(calls, strings.Join(a, " ")); return nil }}
	if err := s.Start(); err == nil {
		t.Fatal("no units: start should fail")
	}
	os.WriteFile(filepath.Join(l.UnitDir(), ServerUnit), nil, 0o644)
	os.WriteFile(filepath.Join(l.UnitDir(), HelperUnit), nil, 0o644)
	s.Stop()
	s.Start()
	if strings.Join(calls, "|") != "stop armageddon.service armageddon-helper.service|start armageddon-helper.service armageddon.service" {
		t.Fatal(calls)
	}
}

func TestUninstall(t *testing.T) {
	e := newEnv(t)
	l := e.l
	os.MkdirAll(l.UnitDir(), 0o755)
	os.WriteFile(filepath.Join(l.UnitDir(), ServerUnit), nil, 0o644)
	os.MkdirAll(filepath.Dir(l.BinLink()), 0o755)
	os.Symlink(filepath.Join(l.CurrentLink(), "armageddon"), l.BinLink())
	os.MkdirAll(l.Etc(), 0o755)
	passwd := filepath.Join(t.TempDir(), "passwd")
	os.WriteFile(passwd, []byte("root:x:0:0::/root:/bin/sh\nws-0123abcd4567:x:999:999::/x:/usr/sbin/nologin\nws-bad:x:1:1::/:/\n"), 0o644)
	var sysc, deleted []string
	u := &Uninstaller{Layout: l, Passwd: passwd,
		Systemctl:  func(a ...string) error { sysc = append(sysc, strings.Join(a, " ")); return nil },
		DeleteUser: func(n string) error { deleted = append(deleted, n); return nil }}

	if err := u.Run(true, false); !errors.Is(err, ErrNotConfirmed) {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.Opt()); err != nil {
		t.Fatal("unconfirmed purge removed files")
	}
	if Confirm(strings.NewReader("yes\n"), &strings.Builder{}, l.DataDir) {
		t.Fatal("'yes' accepted as purge confirmation")
	}
	if !Confirm(strings.NewReader(PurgePhrase(l.DataDir)+"\n"), &strings.Builder{}, l.DataDir) {
		t.Fatal("exact phrase refused")
	}

	if err := u.Run(false, false); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{l.Opt(), l.BinLink(), filepath.Join(l.UnitDir(), ServerUnit)} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s not removed", p)
		}
	}
	if _, err := os.Stat(e.path); err != nil {
		t.Fatal("uninstall without --purge removed data")
	}
	if len(sysc) == 0 || sysc[0] != "disable --now armageddon.service" {
		t.Fatal(sysc)
	}
	// Re-running is fine; with a confirmed purge the data and users go.
	if err := u.Run(true, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.DataDir); err == nil {
		t.Fatal("purge kept the data")
	}
	if strings.Join(deleted, ",") != "ws-0123abcd4567,armageddon" {
		t.Fatal(deleted)
	}
}
