package runtimes

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRanges(t *testing.T) {
	cases := []struct {
		rng      string
		composer bool
		yes, no  []string
	}{
		{"20", false, []string{"20.0.0", "20.11.1"}, []string{"19.9.9", "21.0.0"}},
		{"v20.11.1", false, []string{"20.11.1"}, []string{"20.11.0", "20.12.0"}},
		{">=18 <21", false, []string{"18.0.0", "20.99.0"}, []string{"17.9.0", "21.0.0"}},
		{">= 18", false, []string{"18.0.0", "22.1.0"}, []string{"16.20.0"}},
		{"^18.17.0 || >=20", false, []string{"18.17.0", "18.20.1", "22.0.0"}, []string{"18.16.0", "19.0.0"}},
		{"~20.1", false, []string{"20.1.5"}, []string{"20.2.0"}},
		{"20.x", false, []string{"20.3.0"}, []string{"21.0.0"}},
		{"18 - 20", false, []string{"18.0.0", "20.9.0"}, []string{"21.0.0"}},
		{"*", false, []string{"1.0.0"}, nil},
		{"", false, []string{"1.0.0"}, nil},
		{"^8.1", true, []string{"8.1.0", "8.3.6"}, []string{"8.0.30", "9.0.0"}},
		{"~8.2", true, []string{"8.2.0", "8.9.0"}, []string{"8.1.0", "9.0.0"}},
		{"~8.2.1", true, []string{"8.2.5"}, []string{"8.3.0"}},
		{"^8.1|^8.2", true, []string{"8.2.0"}, []string{"7.4.0"}},
		{">=8.1,<8.4", true, []string{"8.3.6"}, []string{"8.4.0"}},
		{"8.1.*", true, []string{"8.1.9"}, []string{"8.2.0"}},
		{">8", false, []string{"9.0.0"}, []string{"8.5.0"}},
		{"<=8", false, []string{"8.5.0"}, []string{"9.0.0"}},
		{"^0.2.3", false, []string{"0.2.9"}, []string{"0.3.0"}},
	}
	for _, c := range cases {
		parse := ParseRange
		if c.composer {
			parse = ParseComposerRange
		}
		r, err := parse(c.rng)
		if err != nil {
			t.Fatalf("%q: %v", c.rng, err)
		}
		for _, v := range c.yes {
			pv, _ := ParseVersion(v)
			if !r.Match(pv) {
				t.Errorf("%q should match %s", c.rng, v)
			}
		}
		for _, v := range c.no {
			pv, _ := ParseVersion(v)
			if r.Match(pv) {
				t.Errorf("%q should not match %s", c.rng, v)
			}
		}
	}
	if _, err := ParseRange(">=abc"); err == nil {
		t.Error("a bad version should not parse")
	}
}

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDetectNode(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"package.json":   `{"scripts":{"dev":"next dev","build":"next build"},"dependencies":{"next":"14.2.0","react":"18"},"engines":{"node":">=18"}}`,
		".nvmrc":         "20\n",
		"pnpm-lock.yaml": "lockfileVersion: '6.0'\n",
	})
	p, d, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "node" || d.Framework != "next" || d.Version != "20" || d.VersionFrom != ".nvmrc" || d.PackageManager != "pnpm" {
		t.Fatalf("detection: %+v", d)
	}
	write(t, dir, map[string]string{"package.json": `{"packageManager":"yarn@4.1.0+sha512.abc","dependencies":{"vite":"5"}}`})
	os.Remove(filepath.Join(dir, ".nvmrc"))
	_, d, _ = Detect(dir)
	if d.PackageManager != "yarn" || d.PMVersion != "4.1.0" || d.Framework != "vite" || d.Version != "" {
		t.Fatalf("detection: %+v", d)
	}
}

func TestDetectPHP(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"composer.json": `{"require":{"php":"^8.2","laravel/framework":"^11.0"}}`,
		"artisan":       "<?php\n",
	})
	p, d, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "php" || d.Framework != "laravel" || d.Version != "^8.2" || d.PackageManager != "composer" {
		t.Fatalf("detection: %+v", d)
	}
	plan, err := p.Plan(context.Background(), d, &Host{Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(plan.Start, " ") != "php artisan serve --host=127.0.0.1 --port=8000" || plan.Port != 8000 {
		t.Fatalf("start: %v port %d", plan.Start, plan.Port)
	}
	plan.SetPort(8100)
	if plan.Start[len(plan.Start)-1] != "--port=8100" || plan.Env[0] != "PORT=8100" {
		t.Fatalf("SetPort: %v %v", plan.Start, plan.Env)
	}

	empty := t.TempDir()
	if _, _, err := Detect(empty); err != ErrNothingDetected {
		t.Fatalf("empty tree: %v", err)
	}
	write(t, empty, map[string]string{"public/index.php": "<?php echo 1;"})
	p, d, _ = Detect(empty)
	plan, _ = p.Plan(context.Background(), d, &Host{Home: t.TempDir()})
	if strings.Join(plan.Start, " ") != "php -S 127.0.0.1:8000 -t public" {
		t.Fatalf("plain PHP start: %v", plan.Start)
	}
}

// fakeNodeDist serves a Node.js distribution with one release whose node
// is a shell script.
func fakeNodeDist(t *testing.T, tamper bool) (*httptest.Server, string) {
	t.Helper()
	ver := "20.11.1"
	plat, err := nodePlatform(&Host{OS: runtime.GOOS, Arch: runtime.GOARCH})
	if err != nil {
		t.Skip(err)
	}
	top := "node-v" + ver + "-" + plat
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	add := func(name, body string, mode int64) {
		tw.WriteHeader(&tar.Header{Name: top + "/" + name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.WriteHeader(&tar.Header{Name: top + "/", Typeflag: tar.TypeDir, Mode: 0o755})
	add("bin/node", "#!/bin/sh\necho v"+ver+"\n", 0o755)
	add("lib/node_modules/npm/bin/npm-cli.js", "#!/bin/sh\necho npm \"$@\" >> \"$HOME/npm.log\"\n", 0o755)
	tw.WriteHeader(&tar.Header{Name: top + "/bin/npm", Linkname: "../lib/node_modules/npm/bin/npm-cli.js", Typeflag: tar.TypeSymlink})
	tw.Close()
	zw.Close()
	tgz := buf.Bytes()
	sum := sha256.Sum256(tgz)
	if tamper {
		tgz = append([]byte(nil), tgz...)
		tgz[len(tgz)-1] ^= 0xff
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/index.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `[{"version":"v21.6.0","lts":false},{"version":"v%s","lts":"Iron"},{"version":"v18.19.0","lts":"Hydrogen"}]`, ver)
	})
	mux.HandleFunc("/v"+ver+"/SHASUMS256.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s.tar.gz\n", hex.EncodeToString(sum[:]), top)
	})
	mux.HandleFunc("/v"+ver+"/"+top+".tar.gz", func(w http.ResponseWriter, r *http.Request) { w.Write(tgz) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, ver
}

func TestNodeInstallFromMirror(t *testing.T) {
	srv, ver := fakeNodeDist(t, false)
	home := t.TempDir()
	h := &Host{Home: home, NodeMirror: srv.URL, OS: runtime.GOOS, Arch: runtime.GOARCH}
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"package.json":      `{"scripts":{"dev":"next dev"},"dependencies":{"next":"14"}}`,
		"package-lock.json": "{}",
		".nvmrc":            "lts/iron",
	})
	prov, plan, err := PlanFor(context.Background(), dir, h)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Toolchain.Source != "install" || plan.Toolchain.Version != ver {
		t.Fatalf("toolchain: %+v", plan.Toolchain)
	}
	if strings.Join(plan.Start, " ") != "npm run dev" || plan.Port != 3000 || plan.Env[0] != "PORT=3000" {
		t.Fatalf("plan: %+v", plan)
	}
	var log bytes.Buffer
	if err := prov.Install(context.Background(), plan, h, &log); err != nil {
		t.Fatalf("install: %v\n%s", err, log.String())
	}
	if out, err := h.Output(context.Background(), "node"); err != nil || out != "v"+ver {
		t.Fatalf("node on the workspace PATH: %q %v", out, err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, "npm.log")); strings.TrimSpace(string(b)) != "npm ci" {
		t.Fatalf("dependency install: %q\n%s", b, log.String())
	}
	prof, _ := os.ReadFile(filepath.Join(home, ".profile"))
	if !strings.Contains(string(prof), ".armageddon/bin") {
		t.Fatalf("profile: %q", prof)
	}
	// Idempotent: a second plan finds the workspace install; the profile
	// block is not repeated.
	_, plan, _ = PlanFor(context.Background(), dir, h)
	if plan.Toolchain.Source != "workspace" {
		t.Fatalf("second plan: %+v", plan.Toolchain)
	}
	h.EnsureProfile()
	prof, _ = os.ReadFile(filepath.Join(home, ".profile"))
	if strings.Count(string(prof), profileBegin) != 1 {
		t.Fatalf("profile block repeated: %q", prof)
	}
}

func TestNodeInstallRefusesBadChecksum(t *testing.T) {
	srv, ver := fakeNodeDist(t, true)
	home := t.TempDir()
	h := &Host{Home: home, NodeMirror: srv.URL, OS: runtime.GOOS, Arch: runtime.GOARCH}
	v, _ := ParseVersion(ver)
	var log bytes.Buffer
	err := installNode(context.Background(), h, v, &log)
	if err == nil {
		t.Fatal("a tampered tarball was installed")
	}
	if exists(home, ".armageddon/runtimes/node/"+ver) {
		t.Fatal("the tampered tarball was left in place")
	}
}

func TestUntarRefusesEscapes(t *testing.T) {
	for _, hd := range []*tar.Header{
		{Name: "top/../../evil", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "top/link", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink},
		{Name: "top/link", Linkname: "../../outside", Typeflag: tar.TypeSymlink},
	} {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(zw)
		tw.WriteHeader(hd)
		tw.Close()
		zw.Close()
		if err := untarGz(&buf, t.TempDir()); err == nil {
			t.Errorf("%s -> %s: accepted", hd.Name, hd.Linkname)
		}
	}
}

func TestResolveNodeRelease(t *testing.T) {
	srv, _ := fakeNodeDist(t, false)
	h := &Host{NodeMirror: srv.URL}
	for spec, want := range map[string]string{"lts/*": "20.11.1", "node": "21.6.0", "18": "18.19.0", "lts/hydrogen": "18.19.0", ">=20": "21.6.0"} {
		v, err := resolveNodeRelease(context.Background(), h, spec)
		if err != nil || v.String() != want {
			t.Errorf("%s: got %s %v, want %s", spec, v, err, want)
		}
	}
	if _, err := resolveNodeRelease(context.Background(), h, "16"); err == nil {
		t.Error("16 should not resolve")
	}
}

// TestLaravelInstall runs the PHP provider against stand-in php and
// composer programs in the workspace's bin directory: composer install, a
// .env from .env.example, the application key, and the default SQLite
// database created and migrated.
func TestLaravelInstall(t *testing.T) {
	home := t.TempDir()
	h := &Host{Home: home}
	calls := filepath.Join(home, "calls.log")
	fake := "#!/bin/sh\necho \"$(basename \"$0\") $*\" >> " + calls + "\ncase \"$*\" in *PHP_VERSION*) printf 8.3.6;; esac\n"
	write(t, h.BinDir(), map[string]string{"php": fake, "composer": fake})
	os.Chmod(filepath.Join(h.BinDir(), "php"), 0o755)
	os.Chmod(filepath.Join(h.BinDir(), "composer"), 0o755)
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"composer.json":     `{"require":{"php":"^8.2","laravel/framework":"^11.0"}}`,
		"package.json":      `{"scripts":{"dev":"vite"},"devDependencies":{"vite":"5"}}`,
		"artisan":           "<?php\n",
		".env.example":      "APP_KEY=\nDB_CONNECTION=sqlite\n# DB_DATABASE=laravel\n",
		"database/.gitkeep": "",
	})
	prov, plan, err := PlanFor(context.Background(), dir, h)
	if err != nil {
		t.Fatal(err)
	}
	if prov.Name() != "php" || plan.Framework != "laravel" || plan.Toolchain.Version != "8.3.6" || len(plan.Notes) != 0 {
		t.Fatalf("plan: %s %+v", prov.Name(), plan)
	}
	var log bytes.Buffer
	if err := prov.Install(context.Background(), plan, h, &log); err != nil {
		t.Fatalf("install: %v\n%s", err, log.String())
	}
	got, _ := os.ReadFile(calls)
	for _, want := range []string{"composer install --no-interaction --no-progress", "php artisan key:generate --no-interaction", "php artisan migrate --force --no-interaction"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if !exists(dir, ".env") || !exists(dir, "database/database.sqlite") {
		t.Error(".env or the SQLite database was not created")
	}
	// A version the server cannot satisfy is a note, not a failure.
	write(t, dir, map[string]string{"composer.json": `{"require":{"php":"^8.4","laravel/framework":"^11.0"}}`})
	_, plan, _ = PlanFor(context.Background(), dir, h)
	if len(plan.Notes) != 1 || !strings.Contains(plan.Notes[0], "8.4") {
		t.Fatalf("notes: %q", plan.Notes)
	}
}
