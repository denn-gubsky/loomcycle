"""Study D driver: every step strictly in sequence, the tenant restored on any exit.

    nohup setsid python3 -I bench/docs/kbrag/docs/study.py --out /home/denn/workspace/kbrag-work/docs/study --key-prefix kbrag-docs-study2 > <log> 2>&1 &

Steps (each a child process run in the foreground of this one; nothing is inferred from a
PID file — the 2026-11-23 incident, Amendment 3):
  1. route kb/walker to Haiku (forks.py --apply) and create the rag-lc helper (register.py create)
  2. preflight; scored runs of `p7` at 3 runs per arm
  3. preflight; `waves` and `known` at 1 run per arm
  4. preflight; one retry pass per step 2 and 3
  5. restore the tenant (forks.py --restore, register.py retire) and verify it (forks.py --check)
  6. judge; analysis
Step 5 runs in a `finally` that SIGTERM and SIGHUP also reach (they are turned into SystemExit),
so a crash or a kill still restores; only SIGKILL skips it (then run forks.py --restore and
register.py retire by hand). A spend-cap or deadline stop (exit 2) ends the scored runs and
goes on to 5 and 6; a routing violation or failed preflight (3, 4) ends the scored runs too,
and the study is reported as stopped. Every step's start, end and exit code go to
<out>/driver.jsonl.
"""
import argparse, datetime, json, os, signal, subprocess, sys

HERE = os.path.dirname(os.path.abspath(__file__))
PY = [sys.executable, "-I"]


def iso():
    return datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


class Driver:
    def __init__(self, out, key_prefix, deadline, run=None, log=print):
        self.out, self.key_prefix, self.deadline, self.log = out, key_prefix, deadline, log
        self._run = run or self._popen
        self.child = None
        self.stopped = None    # why the scored runs ended early, if they did

    def _popen(self, cmd):
        self.child = subprocess.Popen(cmd)
        try:
            return self.child.wait()
        finally:
            self.child = None

    def stop_child(self):
        """A runner still going when the driver is told to exit is stopped BEFORE the tenant is
        restored: a run must never outlive the routing it was started under."""
        c = self.child
        if c is not None and c.poll() is None:
            self.note(step="driver", event="stopping child", pid=c.pid)
            c.terminate()
            try:
                c.wait(timeout=120)
            except subprocess.TimeoutExpired:
                c.kill()
                c.wait()

    def note(self, **kw):
        os.makedirs(self.out, exist_ok=True)
        with open(os.path.join(self.out, "driver.jsonl"), "a") as f:
            f.write(json.dumps(dict(kw, ts=iso())) + "\n")

    def step(self, name, script, *args):
        self.note(step=name, event="start")
        self.log("== %s %s" % (name, iso()))
        rc = self._run(PY + [os.path.join(HERE, script)] + list(args))
        self.note(step=name, event="end", rc=rc)
        self.log("== %s rc=%s %s" % (name, rc, iso()))
        return rc

    def scored(self, name, *args):
        """A darms invocation, skipped once the scored runs have been stopped."""
        if self.stopped:
            self.note(step=name, event="skipped", why=self.stopped)
            return None
        rc = self.step("preflight " + name, "preflight.py")
        if rc != 0:
            self.stopped = "preflight failed before %s" % name
            return rc
        rc = self.step(name, "darms.py", "--out", self.out, "--workers", "4", "--deadline", self.deadline,
                       "--key-prefix", self.key_prefix, *args)
        if rc != 0:
            self.stopped = "%s exited %s (2 cap/deadline, 3 violation, 4 preflight)" % (name, rc)
        return rc

    def restore(self):
        a = self.step("restore forks", "forks.py", "--restore")
        b = self.step("retire helper", "register.py", "retire")
        c = self.step("verify restore", "forks.py", "--verify-restored")
        ok = a == 0 and b == 0 and c == 0
        self.note(step="restore", event="verified" if ok else "NOT VERIFIED", rc=[a, b, c])
        return ok

    def run(self):
        restored = False
        try:
            if self.step("fork kb/walker", "forks.py", "--apply") != 0 or self.step("create helper", "register.py", "create") != 0:
                self.stopped = "setup failed"
            self.scored("p7", "--sets", "p7", "--reps", "3")
            self.scored("waves+known", "--sets", "waves,known", "--reps", "1")
            self.scored("retry p7", "--sets", "p7", "--reps", "3", "--retry-failed")
            self.scored("retry waves+known", "--sets", "waves,known", "--reps", "1", "--retry-failed")
        finally:
            self.stop_child()
            restored = self.restore()
        if not restored:
            self.log("RESTORE NOT VERIFIED: judge and analysis skipped; see driver.jsonl")
            return 5
        self.step("judge", "djudge.py", "--out", self.out, "--workers", "4")
        self.step("judge retry", "djudge.py", "--out", self.out, "--workers", "4")
        self.step("analyse", "danalyze.py", "--out", self.out)
        self.note(step="driver", event="done", stopped=self.stopped)
        self.log("== done %s; scored runs %s" % (iso(), "stopped: " + self.stopped if self.stopped else "complete"))
        return 0


def _exit_on(signum, frame):
    raise SystemExit(128 + signum)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--key-prefix", required=True)
    ap.add_argument("--deadline", default="2026-11-24T06:00:00Z")
    a = ap.parse_args()
    signal.signal(signal.SIGTERM, _exit_on)
    signal.signal(signal.SIGHUP, _exit_on)
    log = lambda s: print(s, flush=True)  # noqa: E731
    sys.exit(Driver(a.out, a.key_prefix, a.deadline, log=log).run())


if __name__ == "__main__":
    main()
