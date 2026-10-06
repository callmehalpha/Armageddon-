package helper

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// maxFDs bounds the descriptors accepted with one frame (stdin, stdout,
// stderr).
const maxFDs = 3

// sendFrame writes v as one frame, attaching files as SCM_RIGHTS.
func sendFrame(c *net.UnixConn, v any, files ...*os.File) error {
	b, err := Frame(v)
	if err != nil {
		return err
	}
	var oob []byte
	if len(files) > 0 {
		fds := make([]int, len(files))
		for i, f := range files {
			fds[i] = int(f.Fd())
		}
		oob = unix.UnixRights(fds...)
	}
	n, oobn, err := c.WriteMsgUnix(b, oob, nil)
	if err != nil {
		return err
	}
	if oobn != len(oob) {
		return errors.New("short ancillary write")
	}
	if n < len(b) {
		_, err = c.Write(b[n:])
	}
	return err
}

// recvFrame reads one frame and any descriptors sent with it. Received
// descriptors are close-on-exec.
func recvFrame(c *net.UnixConn) ([]byte, []*os.File, error) {
	var hdr [4]byte
	oob := make([]byte, unix.CmsgSpace(maxFDs*4))
	var files []*os.File
	got := 0
	for got < 4 {
		n, oobn, flags, _, err := c.ReadMsgUnix(hdr[got:], oob)
		if oobn > 0 {
			fs, perr := parseRights(oob[:oobn])
			files = append(files, fs...)
			if perr != nil {
				closeAll(files)
				return nil, nil, perr
			}
		}
		if flags&unix.MSG_CTRUNC != 0 {
			closeAll(files)
			return nil, nil, errors.New("too many descriptors in one message")
		}
		if err != nil {
			closeAll(files)
			return nil, nil, err
		}
		if n == 0 {
			closeAll(files)
			return nil, nil, errors.New("connection closed")
		}
		got += n
	}
	body, err := readBody(c, hdr)
	if err != nil {
		closeAll(files)
		return nil, nil, err
	}
	return body, files, nil
}

func parseRights(oob []byte) ([]*os.File, error) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, err
	}
	var files []*os.File
	for _, m := range msgs {
		fds, err := unix.ParseUnixRights(&m)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			unix.CloseOnExec(fd)
			files = append(files, os.NewFile(uintptr(fd), "scm-rights"))
		}
	}
	if len(files) > maxFDs {
		closeAll(files)
		return nil, fmt.Errorf("%d descriptors in one message (max %d)", len(files), maxFDs)
	}
	return files, nil
}

func closeAll(files []*os.File) {
	for _, f := range files {
		if f != nil {
			f.Close()
		}
	}
}

func newHandle() string {
	var b [10]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
