package agent

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/callmehalpha/Armageddon-/internal/treesync/gitshadow"
)

// watcher reports that something in a replica changed (contract §6.3,
// plan M6.6): working files, the index, HEAD and refs (§5.3). It only says
// "something changed": capture is always a full capture, and a periodic
// full capture is the backstop for anything the watcher misses (inotify
// overflow, kqueue limits on macOS, directories created between a create
// event and the watch being added).
type watcher struct {
	root   string
	w      *fsnotify.Watcher
	Events chan struct{}
}

// skipDir reports directories never watched: Git's object store and logs
// (noise), and the built-in class C excludes (§6.6), which are never
// captured anyway.
func skipDir(rel string) bool {
	switch rel {
	case ".git/objects", ".git/logs", ".git/lfs", ".git/modules":
		return true
	}
	base := filepath.Base(rel)
	for _, ex := range gitshadow.BuiltinExcludes {
		if base == strings.TrimSuffix(ex, "/") {
			return true
		}
	}
	return false
}

func newWatcher(root string) (*watcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &watcher{root: root, w: fw, Events: make(chan struct{}, 1)}
	w.addTree(root)
	go w.run()
	return w, nil
}

func (w *watcher) addTree(dir string) {
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(w.root, p)
		if rel != "." && skipDir(filepath.ToSlash(rel)) {
			return filepath.SkipDir
		}
		w.w.Add(p)
		return nil
	})
}

func (w *watcher) signal() {
	select {
	case w.Events <- struct{}{}:
	default:
	}
}

func (w *watcher) run() {
	for {
		select {
		case ev, ok := <-w.w.Events:
			if !ok {
				return
			}
			rel, _ := filepath.Rel(w.root, ev.Name)
			rel = filepath.ToSlash(rel)
			if strings.HasPrefix(rel, ".git/objects/") || strings.HasPrefix(rel, ".git/logs/") {
				continue
			}
			if ev.Op&fsnotify.Create != 0 {
				if fi, err := os.Lstat(ev.Name); err == nil && fi.IsDir() && !skipDir(rel) {
					w.addTree(ev.Name)
				}
			}
			w.signal()
		case _, ok := <-w.w.Errors:
			if !ok {
				return
			}
			w.signal() // overflow: the next capture is full anyway
		}
	}
}

func (w *watcher) Close() {
	if w != nil {
		w.w.Close()
	}
}

// debouncer turns a burst of events into one capture (§6.3): it fires
// after `quiet` without events, or `max` after the first event of a burst
// during continuous edits.
type debouncer struct {
	quiet, max  time.Duration
	first, last time.Time
}

func (d *debouncer) touch(now time.Time) {
	if d.first.IsZero() {
		d.first = now
	}
	d.last = now
}

func (d *debouncer) pending() bool { return !d.first.IsZero() }

func (d *debouncer) due(now time.Time) bool {
	return d.pending() && (now.Sub(d.last) >= d.quiet || now.Sub(d.first) >= d.max)
}

func (d *debouncer) reset() { d.first, d.last = time.Time{}, time.Time{} }
