// Package client is the server half of P6. It runs as the unprivileged
// armageddon user and reaches the helper only through the typed socket API.
// It holds no privileged capability of its own.
package client

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"

	"armageddon/prototypes/p6-privilege-boundary/proto"
)

// Client is a connection to the helper.
type Client struct{ c *net.UnixConn }

// Dial connects to the helper socket.
func Dial(socket string) (*Client, error) {
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return nil, err
	}
	return &Client{c: c}, nil
}

func (c *Client) Close() error { return c.c.Close() }

func (c *Client) call(req proto.Request, fds []int) (proto.Response, []int, error) {
	if err := writeReq(c.c, req, fds); err != nil {
		return proto.Response{}, nil, err
	}
	return readResp(c.c)
}

// Do issues a request with no file descriptors and returns the response.
func (c *Client) Do(req proto.Request) (proto.Response, error) {
	resp, fds, err := c.call(req, nil)
	closeAll(fds)
	if err != nil {
		return resp, err
	}
	if !resp.OK {
		return resp, fmt.Errorf("%s: %s", req.Op, resp.Error)
	}
	return resp, nil
}

// Spawn issues a spawn request, passing stdio/PTY descriptors.
func (c *Client) Spawn(req proto.Request, fds []int) (proto.Response, error) {
	req.Op = proto.OpSpawnInWorkspace
	req.NumFDs = len(fds)
	resp, respFDs, err := c.call(req, fds)
	closeAll(respFDs)
	if err != nil {
		return resp, err
	}
	if !resp.OK {
		return resp, fmt.Errorf("spawn: %s", resp.Error)
	}
	return resp, nil
}

func writeReq(c *net.UnixConn, v any, fds []int) error {
	var buf []byte
	if err := proto.WriteMsg(sink{&buf}, v); err != nil {
		return err
	}
	var oob []byte
	if len(fds) > 0 {
		oob = unix.UnixRights(fds...)
	}
	_, _, err := c.WriteMsgUnix(buf, oob, nil)
	return err
}

func readResp(c *net.UnixConn) (proto.Response, []int, error) {
	buf := make([]byte, 1<<20)
	oob := make([]byte, 4096)
	n, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
	if err != nil {
		return proto.Response{}, nil, err
	}
	var fds []int
	if msgs, e := unix.ParseSocketControlMessage(oob[:oobn]); e == nil {
		for _, m := range msgs {
			if got, e := unix.ParseUnixRights(&m); e == nil {
				fds = append(fds, got...)
			}
		}
	}
	if n < 4 {
		closeAll(fds)
		return proto.Response{}, nil, fmt.Errorf("short response")
	}
	length := int(buf[0])<<24 | int(buf[1])<<16 | int(buf[2])<<8 | int(buf[3])
	if 4+length > n {
		closeAll(fds)
		return proto.Response{}, nil, fmt.Errorf("bad response frame")
	}
	resp, err := proto.DecodeResponse(buf[4 : 4+length])
	return resp, fds, err
}

type sink struct{ b *[]byte }

func (s sink) Write(p []byte) (int, error) { *s.b = append(*s.b, p...); return len(p), nil }

func closeAll(fds []int) {
	for _, fd := range fds {
		unix.Close(fd)
	}
}
