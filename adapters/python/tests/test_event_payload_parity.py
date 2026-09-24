"""Every structured payload on the wire Event reaches the public AgentEvent.

Derived from the generated descriptor rather than listed: a payload added to
the proto (awaiting_review was one) is otherwise decoded by nobody, and a
Python caller sees the frame's type with its content silently missing."""

from __future__ import annotations

import dataclasses

from loomcycle._generated import loomcycle_pb2 as pb
from loomcycle.events import AgentEvent


def test_every_event_payload_is_a_field_of_agent_event():
    payloads = [f.name for f in pb.Event.DESCRIPTOR.fields if f.message_type is not None]
    assert payloads, "no message-typed fields on Event — the check has stopped checking"
    fields = {f.name for f in dataclasses.fields(AgentEvent)}
    missing = sorted(set(payloads) - fields)
    assert not missing, f"AgentEvent does not carry: {missing}"


def test_the_awaiting_review_payload_is_decoded():
    ev = pb.Event(type="awaiting_review", awaiting_review=pb.AwaitingReview(since_turn=3, round=2))
    got = AgentEvent._from_proto(ev)
    assert got.awaiting_review is not None
    assert (got.awaiting_review.since_turn, got.awaiting_review.round) == (3, 2)
    assert AgentEvent._from_proto(pb.Event(type="text", text="x")).awaiting_review is None


def test_the_hook_decision_payload_is_decoded():
    ev = pb.Event(type="hook_decision", hook_decision=pb.HookDecision(
        hook="sec/pin", phase="pre", tool_use_id="c1", tool_name="WebFetch",
        decision="rewrite_input", updated_input=b'{"url":"https://safe/"}'))
    got = AgentEvent._from_proto(ev).hook_decision
    assert got is not None
    assert (got.hook, got.decision, got.updated_input) == ("sec/pin", "rewrite_input", b'{"url":"https://safe/"}')


def test_a_hold_names_the_hook_that_took_it():
    ev = pb.Event(type="awaiting_review", awaiting_review=pb.AwaitingReview(
        since_turn=1, round=1, held_by="ops/hold"))
    got = AgentEvent._from_proto(ev).awaiting_review
    assert got is not None and got.held_by == "ops/hold"
