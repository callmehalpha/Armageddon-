#!/usr/bin/env python3
"""A Git smart-HTTP remote that requires a token, for test/e2e/ide.sh.

    gitremote.py <token> <project-root> <port>

Serves git http-backend on 127.0.0.1:<port>; every request must carry HTTP
Basic auth whose password is <token>.
"""
import base64
import http.server
import os
import subprocess
import sys

TOKEN, ROOT, PORT = sys.argv[1], sys.argv[2], int(sys.argv[3])


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.serve()

    def do_POST(self):
        self.serve()

    def body(self):
        if self.headers.get("Transfer-Encoding", "").lower() == "chunked":
            out = b""
            while True:
                size = int(self.rfile.readline().strip() or b"0", 16)
                if size == 0:
                    self.rfile.readline()
                    return out
                out += self.rfile.read(size)
                self.rfile.readline()
        return self.rfile.read(int(self.headers.get("Content-Length") or 0))

    def serve(self):
        auth = self.headers.get("Authorization", "")
        password = ""
        if auth.startswith("Basic "):
            password = base64.b64decode(auth[6:]).decode(errors="replace").split(":", 1)[-1]
        if password != TOKEN:
            self.send_response(401)
            self.send_header("WWW-Authenticate", 'Basic realm="test"')
            self.end_headers()
            return
        path, _, query = self.path.partition("?")
        env = {
            "PATH": os.environ["PATH"],
            "GIT_PROJECT_ROOT": ROOT,
            "GIT_HTTP_EXPORT_ALL": "1",
            "REMOTE_USER": "test",
            "PATH_INFO": path,
            "QUERY_STRING": query,
            "REQUEST_METHOD": self.command,
            "CONTENT_TYPE": self.headers.get("Content-Type", ""),
            "HTTP_CONTENT_ENCODING": self.headers.get("Content-Encoding", ""),
            "GIT_PROTOCOL": self.headers.get("Git-Protocol", ""),
        }
        out = subprocess.run(["git", "http-backend"], input=self.body(), env=env, capture_output=True).stdout
        sep = b"\r\n\r\n" if b"\r\n\r\n" in out else b"\n\n"
        head, _, rest = out.partition(sep)
        status, headers = 200, []
        for line in head.decode().splitlines():
            k, _, v = line.partition(":")
            if k.lower() == "status":
                status = int(v.strip().split()[0])
            elif k:
                headers.append((k, v.strip()))
        self.send_response(status)
        for k, v in headers:
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(rest)))
        self.end_headers()
        self.wfile.write(rest)

    def log_message(self, *args):
        pass


http.server.ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
