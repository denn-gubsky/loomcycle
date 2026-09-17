"""Per-SURFACE override parity for the Python client.

There is a Go-side guard (internal/api/http/override_parity_test.go) that owns
the cross-transport list. Its Python row used to grep client.py as a whole FILE
— and every override name appears TWICE there, once in run_streaming's call and
once in continue_session's, so the row stayed green while:

  * `_build_run_request` had no override parameters at all, meaning every
    fresh-run call raised TypeError before reaching the wire;
  * `_run_request_from_dict` — the spawn_run_batch child builder — carried none
    of them; and
  * the committed pb2 stubs had no override fields whatsoever.

A file is not a surface. This checks each request-building surface on its own,
from Python, where the signatures and the generated descriptors can be
introspected instead of pattern-matched.
"""

from __future__ import annotations

import inspect

import pytest

from loomcycle import client as client_mod
from loomcycle import LoomcycleClient
from loomcycle._generated import loomcycle_pb2 as pb

# The override wire names. Mirrors overrideWireNames in the Go guard, which is
# what cross-checks this list against the proto and the other transports; the
# property THIS file owns is that every Python surface carries all of them.
OVERRIDES = [
    "model", "provider", "tier", "effort",
    "max_tokens", "max_iterations", "unbounded_iterations", "max_concurrent_children",
    "retry_attempts", "memory_inject_max_tokens", "memory_index_max_bytes", "inject_tool_guide",
]


def test_the_list_matches_the_generated_proto():
    """Non-vacuity, and the reason a list here is safe: every name must be a real
    field on BOTH request messages, so a typo or a stale entry fails loudly
    instead of being silently un-checkable."""
    for msg in (pb.RunRequest, pb.ContinueRequest):
        fields = {f.name for f in msg.DESCRIPTOR.fields}
        missing = sorted(set(OVERRIDES) - fields)
        assert not missing, f"{msg.DESCRIPTOR.name} has no such field(s): {missing}"


@pytest.mark.parametrize(
    "surface",
    [
        client_mod._build_run_request,
        LoomcycleClient.run_streaming,
        LoomcycleClient.continue_session,
    ],
    ids=lambda f: f.__name__,
)
def test_every_request_building_signature_accepts_every_override(surface):
    """The TypeError class of failure: a caller passes an override the callee
    never declared. Checked per function, because the ones that had it and the
    one that did not all lived in the same file."""
    params = set(inspect.signature(surface).parameters)
    missing = sorted(set(OVERRIDES) - params)
    assert not missing, f"{surface.__name__} does not accept: {missing}"


def test_spawn_dict_builder_forwards_every_override():
    """`_run_request_from_dict` is a THIRD enumeration — it re-lists the keys by
    hand rather than sharing a signature — so it gets a behavioural check, not a
    signature one. It builds a fan-out child, and it is the surface that had none
    of these."""
    spawn = {
        "agent": "reviewer",
        "segments": [],
        "model": "some-model", "provider": "some-provider", "tier": "middle", "effort": "high",
        "max_tokens": 4096, "max_iterations": 12, "max_concurrent_children": 2,
        # Every tuning value is its MEANINGFUL ZERO: these are proto3 `optional`
        # precisely so "off" is expressible, and a builder that treated them as
        # falsy-absent would pass a test that used non-zero values.
        "unbounded_iterations": False, "retry_attempts": 0,
        "memory_inject_max_tokens": 0, "memory_index_max_bytes": 0, "inject_tool_guide": False,
    }
    req = client_mod._run_request_from_dict(spawn)
    dropped = []
    for name, want in spawn.items():
        if name in ("agent", "segments"):
            continue
        got = getattr(req, name)
        if got != want:
            dropped.append(f"{name}: got {got!r}, want {want!r}")
        if isinstance(want, bool) or want == 0:
            if not req.HasField(name):
                dropped.append(f"{name}: unset — its meaningful zero was dropped as falsy")
    assert not dropped, "spawn dict keys dropped: " + "; ".join(dropped)


def test_an_omitted_override_stays_unset_on_a_spawn_child():
    """The other half of the contract, and the regression the builder's own
    comment warns about: `.get` on an absent key must stay None and reach the
    wire UNSET, never as the zero that means "no retries" / "inject nothing".
    Giving those `.get` calls a 0/False default would pass every test above."""
    req = client_mod._run_request_from_dict({"agent": "reviewer", "segments": []})
    for name in ("unbounded_iterations", "retry_attempts",
                 "memory_inject_max_tokens", "memory_index_max_bytes", "inject_tool_guide"):
        assert not req.HasField(name), f"{name} was set without the spawn dict asking for it"


def test_a_none_keyword_reaches_the_proto_as_unset():
    """Pins the behaviour `_optional_overrides`' docstring used to describe
    backwards. It claimed passing None through 'would set them to their zero and
    silently turn the feature off'; the runtime actually treats a None keyword as
    absent. The filter is kept for independence from that detail, not because the
    claim was true — and if a protobuf upgrade ever changes it, this fails here
    rather than as a mystery 'retries are off' report from a user."""
    req = pb.RunRequest(retry_attempts=None, inject_tool_guide=None, model=None)
    assert not req.HasField("retry_attempts")
    assert not req.HasField("inject_tool_guide")
    assert req.model == ""
