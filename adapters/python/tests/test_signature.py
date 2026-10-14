"""verify_loomcycle_signature against the Go signer's vector.

The vector is written by internal/sigv1 (pinned by
TestSign_MatchesTheCrossLanguageVector); reading it means this test never
restates the algorithm it checks.
"""

import json
import pathlib

import pytest

from loomcycle import verify_loomcycle_signature

VECTOR = json.loads(
    (pathlib.Path(__file__).resolve().parents[3] / "internal" / "sigv1" / "testdata" / "vector.json").read_text("utf-8")
)
AT = float(VECTOR["t"])


def verify(body=VECTOR["body"], header=VECTOR["header"], secret=VECTOR["secret"], **kw):
    kw.setdefault("now", AT)
    return verify_loomcycle_signature(body, header, secret, **kw)


def test_accepts_the_go_signers_vector_as_str_and_bytes():
    assert verify()
    assert verify(body=VECTOR["body"].encode("utf-8"))
    assert verify(secret=VECTOR["secret"].encode("utf-8"))
    assert verify(header=VECTOR["header"].replace(",", ", "))


def test_refuses_a_tampered_body_a_wrong_secret_and_a_reserialized_body():
    assert not verify(body=VECTOR["body"] + " ")
    assert not verify(secret="another-secret")
    assert not verify(body=json.dumps(json.loads(VECTOR["body"])))


def test_refuses_a_stale_or_future_t_and_keeps_the_window_inclusive():
    assert verify(now=AT + 300)
    assert verify(now=AT - 300)
    assert not verify(now=AT + 301)
    assert not verify(now=AT - 301)
    assert verify(now=AT + 301, tolerance_sec=600)


@pytest.mark.parametrize(
    "header",
    [
        "",
        VECTOR["header"].split(",")[0],
        VECTOR["header"].split(",")[1],
        "t=abc," + VECTOR["header"].split(",")[1],
        VECTOR["header"][:-1],
        VECTOR["header"][:-2] + "zz",
        VECTOR["header"].split(",")[0] + ",v1=",
        VECTOR["header"].split("v1=")[1],
        "t=%d,%s" % (VECTOR["t"] + 1, VECTOR["header"].split(",")[1]),
    ],
)
def test_fails_closed_on_every_malformed_header(header):
    assert not verify(header=header)


def test_an_empty_secret_never_verifies():
    assert not verify(secret="")
