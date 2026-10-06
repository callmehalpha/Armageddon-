//go:build linux

package server

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// socketPeer returns the pid and uid of the process that is listening on
// the other end of a connected unix socket (SO_PEERCRED records them at
// listen time).
func socketPeer(c net.Conn) (pid int, uid uint32, err error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, 0, fmt.Errorf("not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var cred *syscall.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, 0, err
	}
	if cerr != nil {
		return 0, 0, cerr
	}
	return int(cred.Pid), cred.Uid, nil
}

// parentPID reads a process's parent from /proc.
func parentPID(pid int) int {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	// pid (comm) state ppid ...; comm may contain spaces and parentheses.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return 0
	}
	ppid, _ := strconv.Atoi(f[1])
	return ppid
}

const peerCheckSupported = true
