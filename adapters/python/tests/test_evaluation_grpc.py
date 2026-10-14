"""The Evaluation tool outside a run, over the Evaluation RPC.

Same substrate-dispatch shape as History: a JSON ``input`` dict serialised to
``SubstrateRequest.input_json``, dispatched to the ``Evaluation`` stub RPC,
decoded from ``SubstrateResponse``. A tool refusal (is_error=True) raises
SubstrateToolRefusedError. The server-side path, and the grant each token
gets, is covered by internal/api/evaluationtest.
"""

from __future__ import annotations

import json
from typing import Any

import grpc.aio
import pytest

from loomcycle import LoomcycleClient, SubstrateToolRefusedError
from loomcycle._generated import loomcycle_pb2 as pb


def _make_client() -> LoomcycleClient:
    channel = grpc.aio.insecure_channel("127.0.0.1:1")
    return LoomcycleClient(channel=channel)


def _capturing(result: Any):
    captured: dict = {}

    async def fn(req, metadata=None):
        captured["req"] = req
        return result

    return fn, captured


def _ok(payload: dict) -> pb.SubstrateResponse:
    return pb.SubstrateResponse(output_json=json.dumps(payload).encode(), is_error=False)


@pytest.mark.asyncio
async def test_evaluation_dispatches_the_input_verbatim_to_the_evaluation_rpc():
    client = _make_client()
    fake, captured = _capturing(_ok({"eval_id": "eval_1", "emitter_role": "unrelated"}))
    client._stub.Evaluation = fake  # type: ignore[attr-defined]

    out = await client.evaluation({"op": "submit", "run_id": "r_1", "score": 0.5})
    assert out["eval_id"] == "eval_1"
    assert json.loads(captured["req"].input_json) == {"op": "submit", "run_id": "r_1", "score": 0.5}


@pytest.mark.parametrize(
    "call,want",
    [
        (lambda c: c.submit_evaluation("r_1", 0), {"op": "submit", "run_id": "r_1", "score": 0}),
        (
            lambda c: c.submit_evaluation(
                "r_1", 1.0, dimensions={"speed": 0.2}, judgement={"v": 1}, rationale="ok"
            ),
            {
                "op": "submit",
                "run_id": "r_1",
                "score": 1.0,
                "dimensions": {"speed": 0.2},
                "judgement": {"v": 1},
                "rationale": "ok",
            },
        ),
        (lambda c: c.get_evaluation("eval_1"), {"op": "get", "eval_id": "eval_1"}),
        (lambda c: c.list_evaluations_for_run("r_1"), {"op": "list_for_run", "run_id": "r_1"}),
        (
            lambda c: c.list_evaluations_for_run("r_1", limit=5),
            {"op": "list_for_run", "run_id": "r_1", "limit": 5},
        ),
        (
            lambda c: c.list_evaluations_for_def("def_1", limit=2),
            {"op": "list_for_def", "def_id": "def_1", "limit": 2},
        ),
        (lambda c: c.aggregate_evaluations("def_1"), {"op": "aggregate", "def_id": "def_1"}),
        (
            lambda c: c.aggregate_evaluations("def_1", include_lineage=True),
            {"op": "aggregate", "def_id": "def_1", "include_lineage": True},
        ),
    ],
)
@pytest.mark.asyncio
async def test_evaluation_helpers_send_the_op_and_only_the_options_given(call, want):
    client = _make_client()
    fake, captured = _capturing(_ok({}))
    client._stub.Evaluation = fake  # type: ignore[attr-defined]

    await call(client)
    assert json.loads(captured["req"].input_json) == want


@pytest.mark.asyncio
async def test_evaluation_refusal_raises_with_the_tool_name():
    client = _make_client()
    fake, _ = _capturing(
        pb.SubstrateResponse(
            output_json=b'get: requires evaluation_scopes containing "read_any"', is_error=True
        )
    )
    client._stub.Evaluation = fake  # type: ignore[attr-defined]

    with pytest.raises(SubstrateToolRefusedError) as exc:
        await client.get_evaluation("eval_1")
    assert exc.value.tool == "Evaluation"
    assert "read_any" in str(exc.value)
