//go:build !linux

package helper

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// Daemon is only available on Linux.
type Daemon struct{}

func NewDaemon(dataDir, socket, serverUser string) (*Daemon, error) {
	return nil, errors.New("armageddon helper requires Linux")
}

func (d *Daemon) Serve(ctx context.Context) error {
	return errors.New("armageddon helper requires Linux")
}

func NNPMain(args []string) int {
	fmt.Fprintln(os.Stderr, "armageddon helper-nnp requires Linux")
	return 127
}
