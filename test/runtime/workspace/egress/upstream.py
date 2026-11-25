# The egress suite's API upstream (egress.sh): an HTTPS server on ADDR:PORT
# that answers every GET with "<name> <path>" and appends one JSON line per
# request (path and Authorization header) to LOG, so the suite can check what
# the core sent. Run with python3 -I.
import http.server, json, ssl, sys

name, addr, port, cert, key, log = sys.argv[1:7]


class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        with open(log, "a") as f:
            f.write(json.dumps({"path": self.path, "authorization": self.headers.get("Authorization", "")}) + "\n")
        body = (name + " " + self.path).encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass


srv = http.server.ThreadingHTTPServer((addr, int(port)), H)
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(cert, key)
srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
print("ready", flush=True)
srv.serve_forever()
