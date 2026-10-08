#!/usr/bin/env python3
"""Minimal TCP forwarder used by write.sh to take a "laptop" offline.

    proxy.py LISTEN_PORT TARGET_PORT [UPLOAD_BYTES_PER_SECOND]

With a rate, uploads (client to server) are throttled, so a test can cut
the connection in the middle of one (disaster.sh, F5); "upload in
progress" is printed once a connection has sent 200 KB. Killing the process drops every connection (the laptop is offline);
starting it again brings it back.
"""
import socket
import sys
import threading
import time

listen_port, target_port = int(sys.argv[1]), int(sys.argv[2])
rate = int(sys.argv[3]) if len(sys.argv) > 3 else 0


def pipe(a, b, throttle=False):
    sent = 0
    try:
        while True:
            data = a.recv(16384 if throttle else 65536)
            if not data:
                break
            b.sendall(data)
            if throttle:
                if sent < 200000 <= sent + len(data):
                    print("upload in progress", flush=True)  # disaster.sh waits for this
                sent += len(data)
                time.sleep(len(data) / rate)
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
    threading.Thread(target=pipe, args=(client, upstream, rate > 0), daemon=True).start()
    threading.Thread(target=pipe, args=(upstream, client), daemon=True).start()
