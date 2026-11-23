# loomcycle workspace kernel driver for Python (RFC GW §5.7).
#
# The workspace worker starts this file with `python3 -I` in a workspace and
# speaks to it in JSON lines: cells come in on stdin, frames go out on stdout.
# A cell's own output never reaches those two streams: file descriptors 1
# and 2 are a pipe this driver reads, so `print`, a traceback and a
# subprocess's output arrive in the order they were written, and fd 0 is
# /dev/null. The supervisor treats every frame as untrusted: a cell can write
# to the protocol descriptor and forge a frame, and that changes only its
# own result.
#
# Frames in:  {"type":"cell","id":ID,"code":CODE,"n":N,"max_output":BYTES,"max_value":BYTES}
# Frames out: {"type":"ready","protocol":1}
#             {"type":"out","id":ID,"text":TEXT}
#             {"type":"done","id":ID,"value":TEXT?,"error":TEXT?,
#              "interrupted":BOOL?,"pictures":[PATH...]?,"dropped":BYTES?}
#
# The supervisor interrupts a cell with SIGINT and kills the process group
# when that does not end it. The driver exits when its stdin closes.

import ast
import builtins
import codecs
import json
import os
import secrets
import signal
import sys
import threading
import traceback
import warnings

PROTOCOL = 1
# How long the end of a cell waits for its output to drain from the pipe.
DRAIN_SECONDS = 5.0
CHUNK = 64 * 1024


def main():
    proto_fd = os.dup(1)
    in_fd = os.dup(0)
    null = os.open(os.devnull, os.O_RDONLY)
    os.dup2(null, 0)
    os.close(null)
    r, w = os.pipe()
    os.dup2(w, 1)
    os.dup2(w, 2)
    os.close(w)
    os.set_inheritable(proto_fd, False)
    os.set_inheritable(in_fd, False)
    os.set_inheritable(r, False)

    write_lock = threading.Lock()

    def send(frame):
        data = (json.dumps(frame, ensure_ascii=False) + "\n").encode("utf-8", "replace")
        with write_lock:
            view = memoryview(data)
            while view:
                n = os.write(proto_fd, view)
                view = view[n:]

    sys.stdin = open(os.devnull, "r")
    sys.stdout = open(1, "w", encoding="utf-8", errors="replace", buffering=1, closefd=False)
    sys.stderr = open(2, "w", encoding="utf-8", errors="replace", buffering=1, closefd=False)
    sys.__stdout__, sys.__stderr__, sys.__stdin__ = sys.stdout, sys.stderr, sys.stdin

    def no_input(prompt=""):
        raise RuntimeError("input() is not available in a workspace cell: nobody is there to answer; put the value in the code")

    builtins.input = no_input
    os.environ.setdefault("MPLBACKEND", "Agg")
    warnings.filterwarnings("ignore", message=".*(non-GUI backend|non-interactive).*")
    root = os.getcwd()
    if root not in sys.path:
        sys.path.insert(0, root)

    pump = Pump(r, send)
    threading.Thread(target=pump.run, name="loom-output", daemon=True).start()

    ns = {"__name__": "__main__", "__builtins__": builtins}
    reader = os.fdopen(in_fd, "rb", buffering=0)
    signal.signal(signal.SIGINT, signal.SIG_IGN)
    send({"type": "ready", "protocol": PROTOCOL})
    buf = b""
    while True:
        line, buf = read_line(reader, buf)
        if line is None:
            os._exit(0)
        try:
            frame = json.loads(line)
        except ValueError:
            continue
        if not isinstance(frame, dict) or frame.get("type") != "cell":
            continue
        run_cell(frame, ns, root, pump, send)


def read_line(reader, buf):
    while b"\n" not in buf:
        chunk = reader.read(CHUNK)
        if not chunk:
            return None, b""
        buf += chunk
    line, _, rest = buf.partition(b"\n")
    return line, rest


class Pump:
    """Reads the cell's output pipe and sends it as out frames of the
    current cell, at most max bytes of it; the rest is counted. A cell's end
    is a random mark written into the same pipe, so everything written before
    it has been sent when the cell's done frame goes."""

    def __init__(self, fd, send):
        self.fd = fd
        self.send = send
        self.lock = threading.Lock()
        self.cell = None
        self.mark = None
        self.seen = threading.Event()
        self.max = 0
        self.sent = 0
        self.dropped = 0
        self.decoder = codecs.getincrementaldecoder("utf-8")("replace")

    def begin(self, cell_id, max_bytes):
        with self.lock:
            self.cell, self.max, self.sent, self.dropped = cell_id, max_bytes, 0, 0
            self.decoder.reset()

    def end(self):
        mark = b"\x00loom-end-" + secrets.token_hex(16).encode() + b"\x00"
        self.seen.clear()
        with self.lock:
            self.mark = mark
        try:
            sys.stdout.flush()
            sys.stderr.flush()
        except Exception:
            pass
        os.write(1, mark)
        self.seen.wait(DRAIN_SECONDS)
        with self.lock:
            dropped = self.dropped
            self.cell, self.mark = None, None
        return dropped

    def emit(self, data):
        # Called with the lock held.
        if self.cell is None or not data:
            return
        room = self.max - self.sent
        if room <= 0:
            self.dropped += len(data)
            return
        if len(data) > room:
            self.dropped += len(data) - room
            data = data[:room]
        self.sent += len(data)
        text = self.decoder.decode(data)
        if text:
            self.send({"type": "out", "id": self.cell, "text": text})

    def run(self):
        pending = b""
        while True:
            try:
                chunk = os.read(self.fd, CHUNK)
            except InterruptedError:
                continue
            if not chunk:
                return
            with self.lock:
                pending += chunk
                mark = self.mark
                if mark is not None:
                    i = pending.find(mark)
                    if i >= 0:
                        self.emit(pending[:i])
                        # What follows the mark was written after the cell
                        # ended (a thread it left running): no cell's.
                        pending = b""
                        self.seen.set()
                        continue
                # Hold back what could be the start of a mark split across
                # two reads; a mark always starts with a NUL.
                keep = pending.rfind(b"\x00")
                if keep >= 0 and len(pending) - keep < 64:
                    self.emit(pending[:keep])
                    pending = pending[keep:]
                else:
                    self.emit(pending)
                    pending = b""


def cut(text, limit):
    data = text.encode("utf-8", "replace")
    if len(data) <= limit:
        return text, False
    return data[:limit].decode("utf-8", "ignore"), True


def cell_traceback(exc):
    tb = exc.__traceback__
    # Drop this driver's own frames: the cell's start at its first frame
    # whose file is the cell.
    while tb is not None and not tb.tb_frame.f_code.co_filename.startswith("<cell"):
        tb = tb.tb_next
    te = traceback.TracebackException(type(exc), exc, tb)
    # And this driver's frames the cell called into (input()).
    te.stack = traceback.StackSummary.from_list([f for f in te.stack if f.filename != __file__])
    return "".join(te.format())


def save_pictures(root, n):
    plt = sys.modules.get("matplotlib.pyplot")
    if plt is None:
        return []
    out = []
    try:
        nums = plt.get_fignums()
        if nums:
            folder = os.path.join(root, ".loom", "cells")
            os.makedirs(folder, exist_ok=True)
        for i, num in enumerate(nums, 1):
            rel = ".loom/cells/cell-%d-%d.png" % (n, i)
            plt.figure(num).savefig(os.path.join(root, rel))
            out.append(rel)
        plt.close("all")
    except Exception as exc:
        print("saving the cell's figures failed: %s" % exc, file=sys.stderr)
    return out


def run_cell(frame, ns, root, pump, send):
    cell_id = str(frame.get("id", ""))
    code = str(frame.get("code", ""))
    n = int(frame.get("n", 0) or 0)
    max_output = int(frame.get("max_output", 1 << 20) or 0)
    max_value = int(frame.get("max_value", 1 << 20) or 0)
    done = {"type": "done", "id": cell_id}
    pump.begin(cell_id, max_output)
    filename = "<cell %d>" % n
    try:
        signal.signal(signal.SIGINT, signal.default_int_handler)
        try:
            tree = ast.parse(code, filename=filename, mode="exec")
            last = None
            if tree.body and isinstance(tree.body[-1], ast.Expr):
                last = ast.Expression(tree.body.pop().value)
            # Running the agent's code is this driver's purpose: it runs in the
            # agent's own workspace session, under the engine's bounds.
            exec(compile(tree, filename, "exec"), ns)
            if last is not None:
                value = eval(compile(last, filename, "eval"), ns)
                if value is not None:
                    ns["_"] = value
                    text, _ = cut(repr(value), max_value)
                    done["value"] = text
        finally:
            signal.signal(signal.SIGINT, signal.SIG_IGN)
    except KeyboardInterrupt:
        done["interrupted"] = True
        done["error"] = "KeyboardInterrupt: the cell was interrupted; the kernel's variables are kept\n"
    except SystemExit as exc:
        done["error"] = "SystemExit(%r): a cell cannot end the kernel; use reset_kernel\n" % (exc.code,)
    except BaseException as exc:
        if isinstance(exc, SyntaxError):
            text = "".join(traceback.format_exception_only(type(exc), exc))
        else:
            text = cell_traceback(exc)
        done["error"], _ = cut(text, 64 * 1024)
    pictures = save_pictures(root, n)
    if pictures:
        done["pictures"] = pictures
    dropped = pump.end()
    if dropped:
        done["dropped"] = dropped
    send(done)


if __name__ == "__main__":
    main()
