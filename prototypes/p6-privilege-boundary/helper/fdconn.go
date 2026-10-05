package helper

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"

	"armageddon/prototypes/p6-privilege-boundary/proto"
)

// writeMsg sends a framed JSON value plus optional file descriptors as
// SCM_RIGHTS ancillary data over a SOCK_STREAM unix socket.
func writeMsg(c *net.UnixConn, v any, fds []int) error {
	var buf []byte
	w := byteSink{&buf}
	if err := proto.WriteMsg(w, v); err != nil {
		return err
	}
	var oob []byte
	if len(fds) > 0 {
		oob = unix.UnixRights(fds...)
	}
	_, _, err := c.WriteMsgUnix(buf, oob, nil)
	return err
}

// readMsg reads one framed JSON message and any accompanying descriptors.
// maxFDs bounds how many descriptors are accepted; extras are closed.
func readMsg(c *net.UnixConn, maxFDs int) ([]byte, []int, error) {
	buf := make([]byte, 1<<20)
	oob := make([]byte, unix.CmsgSpace(maxFDs*4)+64)
	n, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, nil, err
	}
	fds := parseFDs(oob[:oobn])
	if len(buf[:n]) < 4 {
		closeAll(fds)
		return nil, nil, fmt.Errorf("short frame")
	}
	// The frame is [4-byte length][body]; the body is what the caller wants.
	length := int(buf[0])<<24 | int(buf[1])<<16 | int(buf[2])<<8 | int(buf[3])
	if length < 0 || 4+length > n {
		closeAll(fds)
		return nil, nil, fmt.Errorf("frame length %d exceeds message %d", length, n-4)
	}
	return buf[4 : 4+length], fds, nil
}

func parseFDs(oob []byte) []int {
	var fds []int
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil
	}
	for _, m := range msgs {
		if m.Header.Level == unix.SOL_SOCKET && m.Header.Type == unix.SCM_RIGHTS {
			got, err := unix.ParseUnixRights(&m)
			if err == nil {
				fds = append(fds, got...)
			}
		}
	}
	return fds
}

func closeAll(fds []int) {
	for _, fd := range fds {
		unix.Close(fd)
	}
}

type byteSink struct{ b *[]byte }

func (s byteSink) Write(p []byte) (int, error) { *s.b = append(*s.b, p...); return len(p), nil }

// PeerUID returns the connecting process's uid via SO_PEERCRED. This is the
// boundary enforced in E4: the helper serves only the server uid, regardless
// of the socket's file mode.
func PeerUID(c *net.UnixConn) (uid, gid, pid int, err error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, 0, 0, err
	}
	var ucred *unix.Ucred
	var e2 error
	err = raw.Control(func(fd uintptr) {
		ucred, e2 = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if err != nil {
		return 0, 0, 0, err
	}
	if e2 != nil {
		return 0, 0, 0, e2
	}
	return int(ucred.Uid), int(ucred.Gid), int(ucred.Pid), nil
}
