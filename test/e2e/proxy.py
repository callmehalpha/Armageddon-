#!/usr/bin/env python3
"""Minimal TCP forwarder used by write.sh to take a "laptop" offline.

    proxy.py LISTEN_PORT TARGET_PORT

Killing the process drops every connection (the laptop is offline);
starting it again brings it back.
"""
import socket
import sys
import threading

listen_port, target_port = int(sys.argv[1]), int(sys.argv[2])


def pipe(a, b):
    try:
        while True:
            data = a.recv(65536)
            if not data:
                break
            b.sendall(data)
    except OSError:
        pass
    finally:
        for s in (a, b):
            try:
                s.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass


srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", listen_port))
srv.listen(64)
while True:
    client, _ = srv.accept()
    try:
        upstream = socket.create_connection(("127.0.0.1", target_port))
    except OSError:
        client.close()
        continue
    threading.Thread(target=pipe, args=(client, upstream), daemon=True).start()
    threading.Thread(target=pipe, args=(upstream, client), daemon=True).start()
