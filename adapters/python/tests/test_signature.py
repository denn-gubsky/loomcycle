"""verify_loomcycle_signature against the Go signer's vector.

The vector is written by internal/sigv1 (pinned by
TestSign_MatchesTheCrossLanguageVector); reading it means this test never
restates the algorithm it checks.
"""

import json
import pathlib

import pytest

from loomcycle import verify_loomcycle_delivery, verify_loomcycle_signature

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


def test_delivery_returns_the_signed_bodys_id_and_hook():
    body = verify_loomcycle_delivery(VECTOR["body"], VECTOR["header"], VECTOR["secret"], now=AT)
    assert body is not None
    assert body["delivery_id"] == "7c0e2b6a-1d2f-4a3b-8c4d-5e6f7a8b9c0d"
    assert body["hook_name"] == "gate" and body["owner"] == "agent:w"
    as_bytes = verify_loomcycle_delivery(VECTOR["body"].encode("utf-8"), VECTOR["header"], VECTOR["secret"], now=AT)
    assert as_bytes == body


def test_delivery_is_none_when_the_signature_fails():
    assert verify_loomcycle_delivery(VECTOR["body"], VECTOR["header"], "another-secret", now=AT) is None
    swapped = VECTOR["body"].replace("7c0e2b6a-1d2f", "00000000-0000")
    assert verify_loomcycle_delivery(swapped, VECTOR["header"], VECTOR["secret"], now=AT) is None


@pytest.mark.parametrize("case", VECTOR["unusable"], ids=lambda c: c["name"])
def test_delivery_is_none_for_a_signed_body_that_is_not_a_delivery(case):
    # The signature is valid; it is the body that is refused.
    assert verify_loomcycle_signature(case["body"], case["header"], VECTOR["secret"], now=AT)
    assert verify_loomcycle_delivery(case["body"], case["header"], VECTOR["secret"], now=AT) is None
