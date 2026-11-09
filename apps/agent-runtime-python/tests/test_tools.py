import json
from dataclasses import replace

import httpx
import pytest
from fastapi.testclient import TestClient

from runtime.app import create_app
from test_runtime import SETTINGS, TOKEN, command, data, events, model_stream


def tool_command():
    c = command()
    c["mode"] = "draft"
    c["snapshot"]["tools"] = [{"type": "function", "function": {"name": "system_tools__echo_text", "description": "Echo", "parameters": {"type": "object"}}}]
    return c


def tool_stream(name="system_tools__echo_text", call_id="call_1", args='{"text":"hello"}'):
    return (data({"choices": [{"delta": {"tool_calls": [{"index": 0, "id": call_id, "type": "function", "function": {"name": name, "arguments": args[:5]}}]}}]})
            + data({"choices": [{"delta": {"tool_calls": [{"index": 0, "function": {"arguments": args[5:]}}]}, "finish_reason": "tool_calls"}]}) + data("[DONE]"))


@pytest.mark.parametrize("mode", ["draft", "published"])
def test_tool_loop_fragments_and_followup(mode):
    model_calls, gateway_calls = [], []

    def provider(request):
        body = json.loads(request.content)
        if request.url.host == "gateway.test":
            gateway_calls.append(body)
            assert request.headers["authorization"] == f"Bearer {TOKEN}"
            assert "test-model-secret" not in str(request.headers)
            assert body["arguments"] == {"text": "hello"}
            return httpx.Response(200, json={"call_id": "call_1", "name": body["name"], "status": "SUCCESS", "output": {"text": "hello"}})
        model_calls.append(body)
        if len(model_calls) == 1:
            return httpx.Response(200, text=tool_stream())
        assert body["messages"][-1]["role"] == "tool"
        assert json.loads(body["messages"][-1]["content"])["output"] == {"text": "hello"}
        return httpx.Response(200, text=model_stream("done"))

    with TestClient(create_app(replace(SETTINGS, tool_gateway_url="http://gateway.test"), httpx.MockTransport(provider))) as client:
        payload = tool_command()
        payload["mode"] = mode
        response = client.post("/internal/v1/runs:stream", json=payload, headers={"Authorization": f"Bearer {TOKEN}"})
    output = events(response)
    assert [e["type"] for e in output] == ["run.created", "run.in_progress", "model.round.completed", "tool.call.created", "tool.call.completed", "message.delta", "model.round.completed", "message.completed", "run.completed", "stream.done"]
    assert len(model_calls) == 2 and len(gateway_calls) == 1


@pytest.mark.parametrize("stream", [tool_stream("unbound"), tool_stream(args="[]"), tool_stream(call_id="../other"), tool_stream(args="bad-json")])
def test_invalid_tool_call_never_reaches_gateway(stream):
    def provider(request):
        assert request.url.host != "gateway.test"
        return httpx.Response(200, text=stream)
    with TestClient(create_app(replace(SETTINGS, tool_gateway_url="http://gateway.test"), httpx.MockTransport(provider))) as client:
        response = client.post("/internal/v1/runs:stream", json=tool_command(), headers={"Authorization": f"Bearer {TOKEN}"})
    kinds = [e["type"] for e in events(response)]
    assert kinds.count("run.failed") == 1 and "run.completed" not in kinds


def test_loop_budget_is_bounded():
    model_count, gateway_count = 0, 0
    def provider(request):
        nonlocal model_count, gateway_count
        if request.url.host == "gateway.test":
            gateway_count += 1
            body = json.loads(request.content)
            return httpx.Response(200, json={"call_id": body["call_id"], "name": body["name"], "status": "SUCCESS", "output": {}})
        model_count += 1
        return httpx.Response(200, text=tool_stream(call_id=f"call_{model_count}"))
    with TestClient(create_app(replace(SETTINGS, tool_gateway_url="http://gateway.test"), httpx.MockTransport(provider))) as client:
        response = client.post("/internal/v1/runs:stream", json=tool_command(), headers={"Authorization": f"Bearer {TOKEN}"})
    assert model_count == 5 and gateway_count == 4
    assert events(response)[-2]["payload"]["code"] == "TOOL_GATEWAY_ERROR"


def test_gateway_failure_has_no_success_terminal():
    def provider(request):
        if request.url.host == "gateway.test":
            return httpx.Response(403, text="private error must not escape")
        return httpx.Response(200, text=tool_stream())
    with TestClient(create_app(replace(SETTINGS, tool_gateway_url="http://gateway.test"), httpx.MockTransport(provider))) as client:
        response = client.post("/internal/v1/runs:stream", json=tool_command(), headers={"Authorization": f"Bearer {TOKEN}"})
    assert "private error" not in response.text
    assert "run.completed" not in [e["type"] for e in events(response)]
