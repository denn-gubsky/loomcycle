# The update relay (RFC GW P9 plan §3.4, owner ruling D-P9-66): started by
# the worker inside a workspace's container session, it listens on the
# session's own loopback, which nothing outside the session reaches, and
# hands each update a server posts to the worker.
#
# It writes "P<port>\n" once listening. For each POST to "/" it writes
# "U", the body's length (4 bytes, big endian) and the body, then reads one
# byte from the worker: "A" (queued: 202) or "F" (the queue is full: 429).
# A body over the bound (argv[1]) is 413; another path 404, another method
# 405, all with no body. One request at a time.
import socket
import struct
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer


def main():
    if len(sys.argv) != 2 or not sys.argv[1].isdigit():
        sys.stderr.write("update relay: takes one argument, the size bound\n")
        return 2
    limit = int(sys.argv[1])
    out = sys.stdout.buffer
    ack = sys.stdin.buffer

    class H(BaseHTTPRequestHandler):
        timeout = 30

        def _end(self, code):
            self.send_response(code)
            self.send_header("Content-Length", "0")
            self.send_header("Connection", "close")
            self.end_headers()

        def _refuse(self):
            if self.path != "/":
                self._end(404)
            else:
                self._end(405)

        do_GET = do_PUT = do_DELETE = do_PATCH = do_HEAD = do_OPTIONS = _refuse

        def do_POST(self):
            if self.path != "/":
                self._end(404)
                return
            try:
                n = int(self.headers.get("Content-Length") or "-1")
            except ValueError:
                n = -1
            if n < 0:
                self._end(411)
                return
            if n > limit:
                self._end(413)
                return
            body = self.rfile.read(n)
            if len(body) != n:
                self._end(400)
                return
            out.write(b"U" + struct.pack(">I", n) + body)
            out.flush()
            a = ack.read(1)
            if not a:
                sys.exit(0)
            self._end(202 if a == b"A" else 429)

        def log_message(self, *a):
            pass

    srv = HTTPServer(("127.0.0.1", 0), H)
    srv.socket.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    out.write(b"P%d\n" % srv.server_address[1])
    out.flush()
    srv.serve_forever()


if __name__ == "__main__":
    sys.exit(main())
