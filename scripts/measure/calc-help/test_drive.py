"""Unit tests for drive.py and for tasks.yaml's answers: stdlib only, no network.

  python3 -I -m unittest discover -s scripts/measure/calc-help -p 'test_*.py'

The oracle below recomputes every task's answer from its `check` expression
with Python's own fractions, decimal, datetime, math and statistics. It never
calls loomcycle, so an answer is never Calc's word for itself.
"""

import calendar
import cmath
import contextlib
import importlib.util
import io
import json
import math
import os
import statistics
import tempfile
import unittest
from datetime import date, timedelta
from decimal import ROUND_HALF_UP, Decimal, localcontext
from fractions import Fraction

_HERE = os.path.dirname(os.path.abspath(__file__))
_spec = importlib.util.spec_from_file_location("calc_help_drive", os.path.join(_HERE, "drive.py"))
drive = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(drive)


# --- the oracle --------------------------------------------------------------

def rhu(x, n):
    """x rounded half up to n decimals, as a Fraction."""
    with localcontext() as ctx:
        ctx.prec = 80
        if isinstance(x, Fraction):
            d = Decimal(x.numerator) / Decimal(x.denominator)
        elif isinstance(x, float):
            d = Decimal(repr(x))
        else:
            d = Decimal(x)
        return Fraction(str(d.quantize(Decimal(1).scaleb(-n), rounding=ROUND_HALF_UP)))


def isprime(n):
    if n < 2:
        return False
    i = 2
    while i * i <= n:
        if n % i == 0:
            return False
        i += 1
    return True


def fib(n):
    a, b = 0, 1
    for _ in range(n):
        a, b = b, a + b
    return a


def workdays(a, b, holidays):
    """Monday-to-Friday days from a to b, both included, less the holidays."""
    return sum(1 for k in range((b - a).days + 1)
               if (a + timedelta(k)).weekday() < 5 and (a + timedelta(k)) not in holidays)


def addworkdays(d, n, holidays):
    while n > 0:
        d += timedelta(1)
        if d.weekday() < 5 and d not in holidays:
            n -= 1
    return d


def addmonths(d, n):
    m = d.month - 1 + n
    y, m = d.year + m // 12, m % 12 + 1
    return date(y, m, min(d.day, calendar.monthrange(y, m)[1]))


def irr(flows):
    """The rate where the NPV of flows (one a period, the first at t=0) is 0,
    by bisection."""
    def npv(r):
        return sum(cf / (1 + r) ** t for t, cf in enumerate(flows))
    lo, hi = -0.99, 1.0
    for _ in range(200):
        mid = (lo + hi) / 2
        if npv(lo) * npv(mid) <= 0:
            hi = mid
        else:
            lo = mid
    return (lo + hi) / 2


ORACLE = {
    "__builtins__": {}, "F": Fraction, "D": Decimal, "math": math, "st": statistics,
    "date": date, "timedelta": timedelta, "rhu": rhu, "isprime": isprime, "fib": fib,
    "workdays": workdays, "addworkdays": addworkdays, "addmonths": addmonths, "irr": irr,
    "sum": sum, "range": range, "len": len, "set": set, "pow": pow, "abs": abs, "enumerate": enumerate,
}


# --- the oracle for tasks2.yaml: linear algebra, distributions, calculus -----
#
# Exact where it can be (matrices over Fractions), closed form or a textbook
# series where it cannot. Never NumPy (not in CI) and never Calc.

def _frac_matrix(a):
    return [[Fraction(x) for x in row] for row in a]


def rref(a):
    """The reduced row echelon form of a over Fractions, and its rank."""
    m = _frac_matrix(a)
    rows, cols, r = len(m), len(m[0]), 0
    for c in range(cols):
        p = next((i for i in range(r, rows) if m[i][c] != 0), None)
        if p is None:
            continue
        m[r], m[p] = m[p], m[r]
        m[r] = [x / m[r][c] for x in m[r]]
        for i in range(rows):
            if i != r and m[i][c] != 0:
                m[i] = [x - m[i][c] * y for x, y in zip(m[i], m[r])]
        r += 1
        if r == rows:
            break
    return m, r


def rank(a):
    return rref(a)[1]


def solve(a, b):
    """x with a*x = b for a square, non-singular a, as Fractions."""
    m, r = rref([list(row) + [bi] for row, bi in zip(a, b)])
    if r < len(a) or any(m[i][i] != 1 for i in range(len(a))):
        raise ValueError("singular")
    return [row[-1] for row in m]


def det(a):
    """The determinant over Fractions, by elimination."""
    m, n, d = _frac_matrix(a), len(a), Fraction(1)
    for c in range(n):
        p = next((i for i in range(c, n) if m[i][c] != 0), None)
        if p is None:
            return Fraction(0)
        if p != c:
            m[c], m[p] = m[p], m[c]
            d = -d
        d *= m[c][c]
        for i in range(c + 1, n):
            f = m[i][c] / m[c][c]
            m[i] = [x - f * y for x, y in zip(m[i], m[c])]
    return d


def matmul(a, b):
    return [[sum(Fraction(x) * Fraction(y) for x, y in zip(row, col)) for col in zip(*b)] for row in a]


def inv(a):
    n = len(a)
    cols = [solve(a, [1 if i == j else 0 for i in range(n)]) for j in range(n)]
    return [[cols[j][i] for j in range(n)] for i in range(n)]


def lstsq(a, b):
    """The least-squares x for a full-column-rank a: the normal equations
    a'a x = a'b, solved exactly."""
    at = [list(c) for c in zip(*a)]
    return solve(matmul(at, a), [r[0] for r in matmul(at, [[y] for y in b])])


def polyfit(xs, ys, deg):
    """Least-squares coefficients, highest power first, as Fractions."""
    return lstsq([[Fraction(x) ** k for k in range(deg, -1, -1)] for x in xs], ys)


def eig2sym(a, b, c):
    """The eigenvalues of the symmetric [[a, b], [b, c]] in closed form,
    ascending."""
    m, h = (a + c) / 2, math.sqrt(((a - c) / 2) ** 2 + b * b)
    return (m - h, m + h)


def normcdf(x, mu=0.0, sigma=1.0):
    return 0.5 * (1 + math.erf((x - mu) / (sigma * math.sqrt(2))))


def gammainc_lower(s, x):
    """The regularized lower incomplete gamma P(s, x), by its power series
    (converges for every x >= 0; the terms fall once k passes x)."""
    if x <= 0:
        return 0.0
    term = total = 1.0 / s
    k = 0
    while abs(term) > 1e-17 * abs(total):
        k += 1
        term *= x / (s + k)
        total += term
    return total * math.exp(-x + s * math.log(x) - math.lgamma(s))


def betainc(a, b, x):
    """The regularized incomplete beta I_x(a, b), by its continued fraction
    (Lentz), using the symmetry I_x(a, b) = 1 - I_{1-x}(b, a) where the
    fraction converges slowly."""
    if x <= 0:
        return 0.0
    if x >= 1:
        return 1.0
    if x > (a + 1) / (a + b + 2):
        return 1.0 - betainc(b, a, 1 - x)
    front = math.exp(math.lgamma(a + b) - math.lgamma(a) - math.lgamma(b)
                     + a * math.log(x) + b * math.log(1 - x)) / a
    tiny, f, c, d = 1e-300, 1.0, 1.0, 0.0
    for i in range(400):
        m = i // 2
        if i == 0:
            num = 1.0
        elif i % 2 == 0:
            num = m * (b - m) * x / ((a + 2 * m - 1) * (a + 2 * m))
        else:
            num = -(a + m) * (a + b + m) * x / ((a + 2 * m) * (a + 2 * m + 1))
        d = 1.0 + num * d
        d = tiny if abs(d) < tiny else d
        d = 1.0 / d
        c = 1.0 + num / c
        c = tiny if abs(c) < tiny else c
        f *= c * d
        if abs(1.0 - c * d) < 1e-16:
            break
    return front * (f - 1.0)


def tcdf(t, df):
    p = 0.5 * betainc(df / 2, 0.5, df / (df + t * t))
    return 1 - p if t > 0 else p


def chi2cdf(x, df):
    return gammainc_lower(df / 2, x / 2)


def bisect(f, lo, hi, n=200):
    """A root of f in [lo, hi], where f(lo) and f(hi) differ in sign."""
    flo = f(lo)
    for _ in range(n):
        mid = (lo + hi) / 2
        fm = f(mid)
        if (fm < 0) == (flo < 0):
            lo, flo = mid, fm
        else:
            hi = mid
    return (lo + hi) / 2


def quantile(cdf, p, lo, hi):
    """The x in [lo, hi] with cdf(x) = p, by bisection."""
    return bisect(lambda x: cdf(x) - p, lo, hi)


def simpson(f, a, b, n=2000):
    """Composite Simpson's rule over n (even) panels."""
    h = (b - a) / n
    return h / 3 * (f(a) + f(b) + 4 * sum(f(a + k * h) for k in range(1, n, 2))
                    + 2 * sum(f(a + k * h) for k in range(2, n, 2)))


ORACLE2 = dict(ORACLE, cmath=cmath, rank=rank, solve=solve, det=det, matmul=matmul, inv=inv,
               lstsq=lstsq, polyfit=polyfit, eig2sym=eig2sym, normcdf=normcdf, tcdf=tcdf,
               chi2cdf=chi2cdf, bisect=bisect, quantile=quantile, simpson=simpson,
               min=min, max=max, zip=zip, sorted=sorted, comb=math.comb, complex=complex, float=float)


def as_exact(v):
    """A task answer or a check value as something comparable exactly."""
    if isinstance(v, bool):
        return v
    if isinstance(v, str):
        n = drive.to_number(v)
        return v if n is None or drive.ISO_DATE.fullmatch(v) else n
    if isinstance(v, float):
        return Fraction(repr(v))
    return Fraction(v)


def raw_text(v):
    """A value written as a model might write it in an ANSWER line."""
    if isinstance(v, bool):
        return "true" if v else "false"
    if isinstance(v, float):
        return repr(v)
    return str(v)


class TasksTest(unittest.TestCase):
    def setUp(self):
        self.tasks = drive.load_tasks(os.path.join(_HERE, "tasks.yaml"))

    def test_every_answer_equals_its_independent_check(self):
        for t in self.tasks:
            with self.subTest(task=t["id"]):
                # check is an expression from tasks.yaml, a file in this
                # directory under review like the test itself — not outside
                # input. It runs with no builtins beyond the oracle's names.
                value = eval(t["check"], dict(ORACLE))
                self.assertEqual(as_exact(t["answer"]), as_exact(value),
                                 f"{t['id']}: answer {t['answer']!r}, check gives {value!r}")

    def test_every_answer_scores_as_correct_against_itself(self):
        for t in self.tasks:
            with self.subTest(task=t["id"]):
                self.assertTrue(drive.is_correct(t, raw_text(t["answer"])))

    def test_the_set_spreads_over_the_sections(self):
        per = {}
        for t in self.tasks:
            per[t["section"]] = per.get(t["section"], 0) + 1
        self.assertEqual(set(per), {"elementary", "integers", "sequences", "units", "statistics",
                                    "finance", "dates", "word"})
        self.assertTrue(all(n >= 6 for n in per.values()), per)
        self.assertGreaterEqual(len(self.tasks), 55)

    def test_no_prompt_is_a_calc_expression(self):
        # A prompt is a question a user asks, not a call: no Calc function
        # spelled with its parentheses.
        for t in self.tasks:
            for fn in ("pmt(", "convert(", "workdays(", "stdev(", "round(", "sqrt("):
                self.assertNotIn(fn, t["prompt"], t["id"])

    def test_load_refuses_an_unknown_key_and_a_duplicate_id(self):
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "t.yaml")
            base = ('tasks:\n  - id: a\n    section: s\n    prompt: "p"\n    answer: 1\n'
                    '    tolerance: "exact"\n    check: "1"\n')
            with open(p, "w") as f:
                f.write(base + "    colour: \"red\"\n")
            with self.assertRaises(ValueError):
                drive.load_tasks(p)
            with open(p, "w") as f:
                f.write(base + base.replace("tasks:\n", ""))
            with self.assertRaises(ValueError):
                drive.load_tasks(p)
            with open(p, "w") as f:
                f.write(base.replace('"exact"', '"about:1"'))
            with self.assertRaises(ValueError):
                drive.load_tasks(p)


class Tasks2Test(unittest.TestCase):
    """tasks2.yaml: the sections RFC GY P2–P3 added, checked by ORACLE2."""

    def setUp(self):
        self.tasks = drive.load_tasks(os.path.join(_HERE, "tasks2.yaml"))

    def test_every_answer_equals_its_independent_check(self):
        for t in self.tasks:
            with self.subTest(task=t["id"]):
                # As in TasksTest: check comes from a reviewed file in this
                # directory and runs with only the oracle's names.
                value = eval(t["check"], dict(ORACLE2))
                self.assertEqual(as_exact(t["answer"]), as_exact(value),
                                 f"{t['id']}: answer {t['answer']!r}, check gives {value!r}")

    def test_every_answer_scores_as_correct_against_itself(self):
        for t in self.tasks:
            with self.subTest(task=t["id"]):
                self.assertTrue(drive.is_correct(t, raw_text(t["answer"])))

    def test_the_set_spreads_over_the_new_sections(self):
        per = {}
        for t in self.tasks:
            per[t["section"]] = per.get(t["section"], 0) + 1
        self.assertEqual(set(per), {"matrices", "distributions", "calculus", "complex", "tables", "word"})
        self.assertTrue(all(n >= 8 for s, n in per.items() if s != "word"), per)
        self.assertGreaterEqual(per["word"], 3)
        self.assertGreaterEqual(len(self.tasks), 40)

    def test_no_id_collides_with_the_first_set(self):
        first = {t["id"] for t in drive.load_tasks(os.path.join(_HERE, "tasks.yaml"))}
        self.assertEqual(first & {t["id"] for t in self.tasks}, set())

    def test_no_prompt_is_a_calc_expression(self):
        for t in self.tasks:
            for fn in ("solve(", "det(", "inv(", "eig(", "matrank(", "lstsq(", "normcdf(", "tinv(",
                       "chi2inv(", "binompdf(", "poisscdf(", "expcdf(", "integrate(", "deriv(", "root(",
                       "minimize(", "polyfit(", "polyval(", "interp(", "polyroots(", "complex(", "arg(",
                       "csv(", "where(", "groupby(", "pivot(", "join(", "describe(", "round("):
                self.assertNotIn(fn, t["prompt"], t["id"])

    def test_the_oracle_helpers_agree_with_closed_forms(self):
        # The series and fractions behind the checks, against values with a
        # closed form: t with 1 df is Cauchy, chi-squared with 2 df is an
        # exponential, a triangular matrix's determinant is its diagonal.
        for x in (-3.0, -0.4, 0.7, 12.0):
            self.assertAlmostEqual(tcdf(x, 1), 0.5 + math.atan(x) / math.pi, places=12)
            self.assertAlmostEqual(chi2cdf(abs(x), 2), 1 - math.exp(-abs(x) / 2), places=12)
        self.assertAlmostEqual(normcdf(1.959963984540054), 0.975, places=12)
        self.assertAlmostEqual(quantile(lambda t: tcdf(t, 10), 0.975, 0, 50), 2.228138852, places=8)
        self.assertEqual(det([[2, 7, 1], [0, 3, 5], [0, 0, Fraction(1, 2)]]), 3)
        self.assertEqual(solve([[2, 1], [1, 3]], [3, 5]), [Fraction(4, 5), Fraction(7, 5)])
        self.assertEqual(inv([[4, 7], [2, 6]]), [[Fraction(3, 5), Fraction(-7, 10)], [Fraction(-1, 5), Fraction(2, 5)]])
        self.assertEqual(rank([[1, 2], [2, 4]]), 1)
        self.assertEqual(polyfit([0, 1, 2, 3], [1, 3, 7, 13], 2), [1, 1, 1])
        self.assertEqual(eig2sym(2, 1, 2), (1.0, 3.0))
        self.assertAlmostEqual(simpson(math.exp, 0, 1), math.e - 1, places=12)
        self.assertAlmostEqual(bisect(lambda x: x * x - 2, 0, 2), math.sqrt(2), places=14)


class AnswerTest(unittest.TestCase):
    def test_extract_takes_the_last_answer_line_without_decoration(self):
        text = "First ANSWER: 1\nworking...\n**ANSWER:** `42.5`."
        self.assertEqual(drive.extract_answer(text), "42.5")
        self.assertEqual(drive.extract_answer("answer: Sunday"), "Sunday")
        self.assertIsNone(drive.extract_answer("The result is 4."))
        self.assertIsNone(drive.extract_answer(""))

    def test_to_number_reads_money_percent_grouping_and_fractions(self):
        cases = {"$1,234.50": Fraction("1234.5"), "15.5%": Fraction("15.5"), "11/21": Fraction(11, 21),
                 "-15959.95": Fraction("-15959.95"), "about 3 km": Fraction(3), "1e-3": Fraction(1, 1000),
                 "2,432,902,008,176,640,000": Fraction(2432902008176640000), ".5": Fraction(1, 2),
                 "-$1,234.50": Fraction("-1234.5"), "€ 12.40": Fraction("12.4")}
        for raw, want in cases.items():
            self.assertEqual(drive.to_number(raw), want, raw)
        self.assertIsNone(drive.to_number("none"))
        self.assertIsNone(drive.to_number(None))

    def test_is_correct_applies_each_kind_of_tolerance(self):
        exact = {"answer": 409.5, "tolerance": "exact"}
        self.assertTrue(drive.is_correct(exact, "409.50"))
        self.assertTrue(drive.is_correct(exact, "$409.5"))
        self.assertFalse(drive.is_correct(exact, "409.51"))
        absol = {"answer": 5.706, "tolerance": "abs:1e-3"}
        self.assertTrue(drive.is_correct(absol, "5.707"))
        self.assertFalse(drive.is_correct(absol, "5.708"))
        rel = {"answer": 273493.79, "tolerance": "rel:1e-6"}
        self.assertTrue(drive.is_correct(rel, "273494.0"))
        self.assertFalse(drive.is_correct(rel, "273495"))
        frac = {"answer": "11/21", "tolerance": "exact"}
        self.assertTrue(drive.is_correct(frac, "11/21"))
        self.assertFalse(drive.is_correct(frac, "0.5238095238"))
        self.assertFalse(drive.is_correct(exact, None))

    def test_is_correct_reads_dates_words_and_booleans(self):
        self.assertTrue(drive.is_correct({"answer": "2026-02-28", "tolerance": "exact"}, "2026-02-28 (a Saturday)"))
        self.assertFalse(drive.is_correct({"answer": "2026-02-28", "tolerance": "exact"}, "2026-03-03"))
        self.assertTrue(drive.is_correct({"answer": "Sunday", "tolerance": "exact"}, "sunday."))
        self.assertFalse(drive.is_correct({"answer": "Sunday", "tolerance": "exact"}, "Monday"))
        self.assertTrue(drive.is_correct({"answer": True, "tolerance": "exact"}, "Yes"))
        self.assertTrue(drive.is_correct({"answer": True, "tolerance": "exact"}, "Yes, it is prime"))
        self.assertFalse(drive.is_correct({"answer": True, "tolerance": "exact"}, "false"))
        self.assertFalse(drive.is_correct({"answer": True, "tolerance": "exact"}, "not prime"))


def sse(*events):
    out = []
    for name, data in events:
        out += [f"event: {name}", "data: " + json.dumps(data), ""]
    return out


def call(i, name, inp):
    return ("tool_call", {"type": "tool_call", "tool_use": {"id": i, "name": name, "input": inp}})


def result(i, name, inp, text, is_error=False):
    d = {"type": "tool_result", "text": text, "tool_use": {"id": i, "name": name, "input": inp}}
    if is_error:
        d["is_error"] = True
    return ("tool_result", d)


def refused(i, inp, code):
    """A refused Calc call as the stream carries it: the result text wraps
    Calc's own refusal JSON (shape copied from a captured run)."""
    inner = json.dumps({"detail": "…", "error": code, "hint": "…", "pos": 0})
    return result(i, "Calc", inp, json.dumps({"isError": True, "error": inner, "errorCategory": "validation",
                                              "isRetryable": False, "description": "…"}))


def usage(i, o, cr=0, cw=0):
    return ("usage", {"type": "usage", "usage": {"input_tokens": i, "output_tokens": o,
                                                  "cache_read_input_tokens": cr, "cache_creation_input_tokens": cw,
                                                  "provider": "anthropic", "model": "claude-haiku-5-5"}})


class SSETest(unittest.TestCase):
    def test_a_refused_first_call_is_recorded_and_help_reads_counted(self):
        help_in = {"op": "help", "topic": "Calc/finance"}
        bad, good = {"expr": "pmt(0.05/12, 60)"}, {"expr": "round(-pmt(0.05/12, 60, 1000), 2)"}
        lines = sse(
            ("agent", {"run_id": "r_1"}),
            usage(1000, 50, 0, 900),
            call("t1", "Calc", bad),
            refused("t1", bad, "arity"),
            usage(1200, 40, 900, 0),
            call("t2", "Context", help_in),
            result("t2", "Context", help_in, "…"),
            call("t3", "Calc", good),
            result("t3", "Calc", good, json.dumps({"result": 18.87, "text": "18.87", "exact": False})),
            ("text", {"type": "text", "text": "The payment is 18.87.\nANSWER: 18.87"}),
            ("done", {"type": "done", "stop_reason": "end_turn"}),
        )
        rec = drive.parse_sse(lines, drive.new_record())
        self.assertEqual(rec["run_id"], "r_1")
        self.assertEqual(rec["calc_calls"], 2)
        self.assertIs(rec["first_calc_parsed"], False)
        self.assertEqual(rec["first_calc_error"], "arity")
        self.assertEqual(rec["calc_errors"], ["arity"])
        self.assertEqual(rec["help_reads"], 1)
        self.assertEqual(rec["help_topics"], ["Calc/finance"])
        self.assertEqual(rec["tool_calls"], {"Calc": 2, "Context/help": 1})
        self.assertEqual((rec["input"], rec["output"], rec["cache_read"], rec["cache_write"]), (2200, 90, 900, 900))
        self.assertEqual(rec["model"], "claude-haiku-5-5")
        self.assertEqual(rec["stop"], "end_turn")
        drive.score({"answer": 18.87, "tolerance": "abs:0.01"}, rec)
        self.assertEqual(rec["answer"], "18.87")
        self.assertTrue(rec["correct"])

    def test_a_good_first_call_parses_and_a_numeric_refusal_still_counts_as_parsed(self):
        a, b = {"expr": "1/0"}, {"expr": "2+2"}
        rec = drive.parse_sse(sse(
            call("x", "Calc", a), refused("x", a, "division_by_zero"),
            call("y", "Calc", b), result("y", "Calc", b, json.dumps({"result": 4, "text": "4"})),
        ), drive.new_record())
        self.assertIs(rec["first_calc_parsed"], True)
        self.assertEqual(rec["first_calc_error"], "division_by_zero")
        self.assertEqual(rec["calc_errors"], ["division_by_zero"])

    def test_an_unwrapped_refusal_is_read_too(self):
        a = {"expr": "2x"}
        rec = drive.parse_sse(sse(
            call("x", "Calc", a), result("x", "Calc", a, json.dumps({"error": "syntax", "detail": "…"})),
        ), drive.new_record())
        self.assertEqual(rec["first_calc_error"], "syntax")
        self.assertIs(rec["first_calc_parsed"], False)

    def test_a_dispatcher_refusal_counts_as_a_parse_failure(self):
        # The argument check runs before Calc: its message is not Calc's JSON.
        a = {"expr": "2+2", "precision": 4}
        wrapped = json.dumps({"isError": True, "error": 'unknown argument "precision"'})
        for text, flag in ((wrapped, False), ('unknown argument "precision"', True)):
            rec = drive.parse_sse(sse(call("x", "Calc", a), result("x", "Calc", a, text, flag)),
                                  drive.new_record())
            self.assertIs(rec["first_calc_parsed"], False, text)
            self.assertEqual(rec["first_calc_error"], "call_refused", text)

    def test_a_result_is_not_an_error(self):
        a = {"expr": "2+2"}
        rec = drive.parse_sse(sse(call("x", "Calc", a), result("x", "Calc", a, json.dumps({"result": 4, "text": "4"}))),
                              drive.new_record())
        self.assertIs(rec["first_calc_parsed"], True)
        self.assertIsNone(rec["first_calc_error"])
        self.assertEqual(rec["calc_errors"], [])

    def test_model_mismatch_allows_a_dated_id_and_flags_another_model(self):
        self.assertFalse(drive.model_mismatch("claude-haiku-5-5", "claude-haiku-5-5-20261001"))
        self.assertTrue(drive.model_mismatch("claude-haiku-5-5", "qwen3.6:latest"))
        self.assertFalse(drive.model_mismatch(None, "anything"))
        self.assertFalse(drive.model_mismatch("m", None))

    def test_unknown_name_codes_are_parse_failures(self):
        self.assertTrue(drive.is_parse_error("unknown_function"))
        self.assertTrue(drive.is_parse_error("syntax"))
        self.assertFalse(drive.is_parse_error("domain"))
        self.assertFalse(drive.is_parse_error(None))

    def test_no_calc_call_leaves_parsed_unset(self):
        rec = drive.parse_sse(sse(("text", {"text": "ANSWER: 4"})), drive.new_record())
        self.assertIsNone(rec["first_calc_parsed"])
        self.assertEqual(rec["calc_calls"], 0)


class SummaryTest(unittest.TestCase):
    def rec(self, section, correct, calls, parsed, helps, cost=0.001, error=None):
        return {"section": section, "correct": correct, "calc_calls": calls, "first_calc_parsed": parsed,
                "help_reads": helps, "help_topics": ["Calc"] * helps, "cost_usd": cost, "error": error}

    def test_rates_per_section_and_overall(self):
        rs = [self.rec("units", True, 1, True, 0), self.rec("units", False, 2, False, 2),
              self.rec("dates", True, 0, None, 1, error=None), self.rec("dates", False, 1, True, 0, error="timeout")]
        rows = {r["section"]: r for r in drive.summarise(rs)}
        self.assertEqual(rows["units"]["first_call_parsed"], 0.5)
        self.assertEqual(rows["units"]["accuracy"], 0.5)
        self.assertEqual(rows["units"]["mean_help_reads"], 1.0)
        self.assertEqual(rows["dates"]["called_calc"], 0.5)
        self.assertEqual(rows["dates"]["first_call_parsed"], 1.0)  # of the runs that called Calc
        self.assertEqual(rows["dates"]["failed"], 1)
        self.assertEqual(rows["all"]["runs"], 4)
        self.assertEqual(rows["all"]["accuracy"], 0.5)
        self.assertEqual(rows["all"]["cost_usd"], 0.004)
        table = drive.markdown(drive.summarise(rs))
        self.assertIn("| units | 2 | 100% | 50% | 50% | 1.5 | 1.0 | 0 | $0.0020 |", table)
        self.assertEqual(drive.help_topic_counts(rs), {"Calc": 3})

    def test_cost_uses_the_haiku_price_model(self):
        rec = {"input": 1_000_000, "output": 1_000_000, "cache_read": 1_000_000, "cache_write": 1_000_000}
        self.assertAlmostEqual(drive.cost(rec, "haiku"), 0.10 + 0.50 + 0.01 + 0.125)
        self.assertEqual(drive.cost(rec, "free"), 0)

    def test_the_spend_stop_ends_the_run_before_the_ceiling_could_pass_max_usd(self):
        calls = []

        def fake_run_once(base, token, agent, prompt, provider, model, timeout):
            calls.append(prompt)
            rec = drive.new_record()
            rec.update({"input": 70_000, "output": 10_000, "model": model,
                        "text": "ANSWER: 1", "seconds": 0})  # $0.007 + $0.005 = $0.012 a run
            return rec

        real = drive.run_once
        drive.run_once = fake_run_once
        try:
            with tempfile.TemporaryDirectory() as d, contextlib.redirect_stderr(io.StringIO()) as err, \
                    contextlib.redirect_stdout(io.StringIO()):
                drive.main(["--model", "m", "--max-usd", "0.05", "--out", d])
                lines = drive.read_results(os.path.join(d, "results.jsonl"))
        finally:
            drive.run_once = real
        # After 2 runs ($0.024) a 3rd could reach $0.044: it runs. After 3
        # ($0.036) a 4th could reach $0.056, past $0.05: it stops at 3.
        self.assertEqual(len(calls), 3)
        self.assertEqual(len(lines), 3)
        self.assertIn("stopping", err.getvalue())

    def test_plan_sends_nothing_and_prints_the_ceiling(self):
        err = io.StringIO()
        with contextlib.redirect_stderr(err):
            self.assertEqual(drive.main(["--plan", "--runs", "2"]), 0)
        self.assertIn("x 2 run(s)", err.getvalue())
        self.assertIn("ceiling $", err.getvalue())


if __name__ == "__main__":
    unittest.main()
