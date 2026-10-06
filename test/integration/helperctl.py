#!/usr/bin/env python3
"""Raw client for the armageddon helper socket, for integration tests.

    helperctl.py SOCKET JSON            send one request, print the response
    helperctl.py SOCKET JSON --stdio    also pass stdin/stdout/stderr (SCM_RIGHTS)
                                        and print the exit frame too

It speaks the wire format directly (4-byte big-endian length + JSON), so the
tests exercise the helper's decoder and checks, not the Go client. Exit
status: 0 if the first response has ok=true, 1 otherwise, 2 if the helper
closed the connection without answering, 3 if it could not connect.
"""
import json
import socket
import struct
import sys


def recv_exact(s, n):
    buf = b""
    while len(buf) < n:
        chunk = s.recv(n - len(buf))
        if not chunk:
            return None
        buf += chunk
    return buf


def recv_frame(s):
    hdr = recv_exact(s, 4)
    if hdr is None:
        return None
    (n,) = struct.unpack(">I", hdr)
    body = recv_exact(s, n)
    return None if body is None else json.loads(body)


def main():
    path, req = sys.argv[1], sys.argv[2].encode()
    stdio = "--stdio" in sys.argv[3:]
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    try:
        s.connect(path)
    except OSError as e:
        print("connect: %s" % e, file=sys.stderr)
        sys.exit(3)
    frame = struct.pack(">I", len(req)) + req
    try:
        if stdio:
            socket.send_fds(s, [frame], [0, 1, 2])
        else:
            s.sendall(frame)
        resp = recv_frame(s)
    except OSError:  # the helper hung up on us
        resp = None
    if resp is None:
        print("closed", file=sys.stderr)
        sys.exit(2)
    print(json.dumps(resp), file=sys.stderr)
    if resp.get("ok") and stdio:
        exit_frame = recv_frame(s)
        print(json.dumps(exit_frame), file=sys.stderr)
    sys.exit(0 if resp.get("ok") else 1)


if __name__ == "__main__":
    main()
