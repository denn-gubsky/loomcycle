# The tunnel's relay (RFC GW P9 plan §3.2): started by the worker inside a
# workspace's session, it joins its own stdin and stdout to 127.0.0.1:<port>
# in the session's network namespace. It takes a port and nothing else: no
# host can be named, so it reaches only the session's own loopback. It
# writes one byte, "O", once connected; then the port's answer, as it comes.
import os
import socket
import sys
import threading


def main():
    if len(sys.argv) != 2 or not (sys.argv[1].isascii() and sys.argv[1].isdigit()):
        sys.stderr.write("relay: takes one argument, a port\n")
        return 2
    port = int(sys.argv[1])
    if not 0 < port < 65536:
        sys.stderr.write("relay: port %d is not from 1 to 65535\n" % port)
        return 2
    try:
        s = socket.create_connection(("127.0.0.1", port), timeout=10)
    except OSError as e:
        sys.stderr.write("relay: connect: %s\n" % (e.strerror or e))
        return 3
    s.settimeout(None)
    out = sys.stdout.buffer
    out.write(b"O")
    out.flush()

    def upstream():
        try:
            while True:
                b = os.read(0, 65536)
                if not b:
                    break
                s.sendall(b)
        except OSError:
            pass
        try:
            s.shutdown(socket.SHUT_WR)
        except OSError:
            pass

    threading.Thread(target=upstream, daemon=True).start()
    try:
        while True:
            b = s.recv(65536)
            if not b:
                break
            out.write(b)
            out.flush()
    except OSError:
        pass
    return 0


if __name__ == "__main__":
    sys.exit(main())
