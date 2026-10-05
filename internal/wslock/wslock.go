// Package wslock is the cross-process form of a workspace's commit lock.
//
// The authority serialises commits per workspace with an in-process mutex
// (contract §4.1). Operations that run in another process, such as
// `armageddon server backup`, need to pause commits briefly too. The
// authority therefore also holds an exclusive flock on
// <workspace>/journal/commit.lock while it commits, and a backup takes the
// same lock while it bundles that workspace's repositories.
//
// The lock file is 0600 and owned by the server user, so workspace processes
// cannot open it and stall their own commits.
package wslock

import (
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Path is the lock file of the workspace rooted at wsRoot.
func Path(wsRoot string) string { return filepath.Join(wsRoot, "journal", "commit.lock") }

// Lock blocks until it holds the commit lock of the workspace rooted at
// wsRoot, then returns the function that releases it.
func Lock(wsRoot string) (func(), error) {
	return lock(wsRoot, syscall.LOCK_EX)
}

// TryLock is Lock with a deadline: it polls until timeout and returns
// ErrTimeout if the lock stayed busy.
func TryLock(wsRoot string, timeout time.Duration) (func(), error) {
	deadline := time.Now().Add(timeout)
	for {
		unlock, err := lock(wsRoot, syscall.LOCK_EX|syscall.LOCK_NB)
		if err != syscall.EWOULDBLOCK {
			return unlock, err
		}
		if time.Now().After(deadline) {
			return nil, ErrTimeout
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type timeoutError struct{}

func (timeoutError) Error() string { return "workspace commit lock is busy" }

// ErrTimeout is returned by TryLock.
var ErrTimeout error = timeoutError{}

func lock(wsRoot string, how int) (func(), error) {
	p := Path(wsRoot)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), how)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
