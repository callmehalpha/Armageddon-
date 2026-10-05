package wslock

import (
	"testing"
	"time"
)

func TestLockExcludes(t *testing.T) {
	root := t.TempDir()
	unlock, err := Lock(root)
	if err != nil {
		t.Fatal(err)
	}
	// flock locks belong to the open file description, so a second open in
	// the same process contends exactly like another process would.
	if _, err := TryLock(root, 50*time.Millisecond); err != ErrTimeout {
		t.Fatalf("second lock: want ErrTimeout, got %v", err)
	}
	unlock()
	u2, err := TryLock(root, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	u2()
}
