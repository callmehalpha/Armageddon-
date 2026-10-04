package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// replicaLock is an exclusive advisory lock on a replica: two agent
// processes must never capture or apply into the same replica at once.
type replicaLock struct{ f *os.File }

var errBusy = fmt.Errorf("busy")

func lockReplica(wsID string) (*replicaLock, error) {
	if err := os.MkdirAll(replicaDir(wsID), 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(replicaDir(wsID), "lock")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		b, _ := os.ReadFile(p)
		f.Close()
		pid := strings.TrimSpace(string(b))
		if pid == "" {
			pid = "another process"
		} else {
			pid = "pid " + pid
		}
		return nil, fmt.Errorf("%w: this replica is in use by %s (an `armageddon follow`?)", errBusy, pid)
	}
	f.Truncate(0)
	f.WriteString(strconv.Itoa(os.Getpid()) + "\n")
	return &replicaLock{f}, nil
}

func (l *replicaLock) release() {
	if l != nil && l.f != nil {
		syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
		l.f.Close()
	}
}
