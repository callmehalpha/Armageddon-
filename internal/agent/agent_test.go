package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDebouncer(t *testing.T) {
	d := debouncer{quiet: 2 * time.Second, max: 15 * time.Second}
	t0 := time.Unix(1000, 0)
	if d.due(t0) {
		t.Fatal("due with no events")
	}
	d.touch(t0)
	if d.due(t0.Add(time.Second)) {
		t.Fatal("due before 2 s of quiet")
	}
	if !d.due(t0.Add(2 * time.Second)) {
		t.Fatal("not due after 2 s of quiet")
	}
	// Continuous edits: one event per second never gives 2 s of quiet, but
	// a capture still happens 15 s into the burst.
	d.reset()
	for i := 0; i < 14; i++ {
		d.touch(t0.Add(time.Duration(i) * time.Second))
		if d.due(t0.Add(time.Duration(i) * time.Second)) {
			t.Fatalf("due at %d s during a burst", i)
		}
	}
	d.touch(t0.Add(15 * time.Second))
	if !d.due(t0.Add(15 * time.Second)) {
		t.Fatal("not due 15 s into a burst")
	}
}

func TestWatcherSeesEditsAndRefs(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"src", ".git/refs/heads", ".git/objects/ab", "node_modules/x"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	w, err := newWatcher(root)
	if err != nil {
		t.Skip("no watcher on this platform:", err)
	}
	defer w.Close()
	expect := func(what string, fire func(), want bool) {
		t.Helper()
		for len(w.Events) > 0 {
			<-w.Events
		}
		time.Sleep(50 * time.Millisecond)
		for len(w.Events) > 0 {
			<-w.Events
		}
		fire()
		select {
		case <-w.Events:
			if !want {
				t.Fatalf("%s: unexpected event", what)
			}
		case <-time.After(time.Second):
			if want {
				t.Fatalf("%s: no event", what)
			}
		}
	}
	expect("file edit", func() { os.WriteFile(filepath.Join(root, "src", "a.go"), []byte("x"), 0o644) }, true)
	expect("ref update", func() { os.WriteFile(filepath.Join(root, ".git/refs/heads/main"), []byte("x"), 0o644) }, true)
	expect("new directory, then a file in it", func() {
		os.MkdirAll(filepath.Join(root, "new/deep"), 0o755)
		time.Sleep(200 * time.Millisecond)
		for len(w.Events) > 0 {
			<-w.Events
		}
		os.WriteFile(filepath.Join(root, "new/deep/f"), []byte("x"), 0o644)
	}, true)
	expect("object store churn", func() { os.WriteFile(filepath.Join(root, ".git/objects/ab/cd"), []byte("x"), 0o644) }, false)
	expect("excluded node_modules", func() { os.WriteFile(filepath.Join(root, "node_modules/x/y.js"), []byte("x"), 0o644) }, false)
}

func TestServiceFiles(t *testing.T) {
	env := map[string]string{"ARMAGEDDON_DATA_DIR": "/home/a b/data", "ARMAGEDDON_CONFIG_DIR": "/home/a/cfg"}
	sf, err := ServiceFor("linux", "/home/a", "/usr/local/bin/armageddon", env)
	if err != nil {
		t.Fatal(err)
	}
	if sf.Path != "/home/a/.config/systemd/user/armageddon-agent.service" {
		t.Fatalf("path = %s", sf.Path)
	}
	for _, want := range []string{
		"ExecStart=/usr/local/bin/armageddon agent run\n",
		"Environment=ARMAGEDDON_CONFIG_DIR=/home/a/cfg\n",
		"Environment=\"ARMAGEDDON_DATA_DIR=/home/a b/data\"\n",
		"Restart=always\n", "WantedBy=default.target\n",
	} {
		if !strings.Contains(sf.Content, want) {
			t.Fatalf("systemd unit lacks %q:\n%s", want, sf.Content)
		}
	}
	if !strings.Contains(sf.Enable, "systemctl --user enable --now armageddon-agent") {
		t.Fatalf("enable = %s", sf.Enable)
	}
	sf, err = ServiceFor("darwin", "/Users/a", "/opt/arm & co/armageddon", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sf.Path != "/Users/a/Library/LaunchAgents/dev.armageddon.agent.plist" {
		t.Fatalf("path = %s", sf.Path)
	}
	for _, want := range []string{
		"<key>Label</key><string>dev.armageddon.agent</string>",
		"<string>/opt/arm &amp; co/armageddon</string><string>agent</string><string>run</string>",
		"<key>KeepAlive</key><true/>", "<key>RunAtLoad</key><true/>",
	} {
		if !strings.Contains(sf.Content, want) {
			t.Fatalf("plist lacks %q:\n%s", want, sf.Content)
		}
	}
	if strings.Contains(sf.Content, "EnvironmentVariables") {
		t.Fatal("empty environment written")
	}
	if _, err := ServiceFor("windows", "", "", nil); err == nil {
		t.Fatal("windows accepted")
	}
}
