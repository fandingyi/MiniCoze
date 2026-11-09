import json
import logging
from dataclasses import replace

import httpx
from fastapi.testclient import TestClient

from runtime.app import create_app
from test_runtime import SETTINGS, TOKEN, data, events, model_stream
from test_tools import tool_command, tool_stream
from test_runtime import command


def first_round():
    return tool_stream().replace(data("[DONE]"), data({"choices": [], "usage": {"prompt_tokens": 4, "completion_tokens": 3, "total_tokens": 7}}) + data("[DONE]"))


def test_completed_round_survives_later_failure_and_logs_are_redacted(caplog):
    count = 0
    def provider(request):
        nonlocal count
        if request.url.host == "gateway.test":
            return httpx.Response(200, json={"call_id": "call_1", "name": "system_tools__echo_text", "status": "SUCCESS", "output": {"text": "private tool output"}})
        count += 1
        return httpx.Response(200, text=first_round()) if count == 1 else httpx.Response(500, text="private provider error")
    payload = tool_command()
    payload["input"]["text"] = "private user prompt"
    with caplog.at_level(logging.INFO, logger="uvicorn.error"):
        with TestClient(create_app(replace(SETTINGS, tool_gateway_url="http://gateway.test"), httpx.MockTransport(provider))) as client:
            response = client.post("/internal/v1/runs:stream", json=payload, headers={"Authorization": f"Bearer {TOKEN}"})
    output = events(response)
    rounds = [e["payload"] for e in output if e["type"] == "model.round.completed"]
    assert len(rounds) == 1 and rounds[0]["usage"]["totalTokens"] == 7
    assert output[-2]["type"] == "run.failed"
    text = "\n".join(record.getMessage() for record in caplog.records if record.name == "uvicorn.error")
    assert "run-1" in text and "runtime.terminal" in text
    assert all(secret not in text for secret in [TOKEN, "test-model-secret", "private user prompt", "private tool output", "private provider error"])


def test_multiround_usage_sum_and_missing_usage():
    for missing in (False, True):
        count = 0
        def provider(request):
            nonlocal count
            if request.url.host == "gateway.test":
                return httpx.Response(200, json={"call_id": "call_1", "name": "system_tools__echo_text", "status": "SUCCESS", "output": {}})
            count += 1
            if count == 1:
                return httpx.Response(200, text=tool_stream() if missing else first_round())
            return httpx.Response(200, text=model_stream())
        with TestClient(create_app(replace(SETTINGS, tool_gateway_url="http://gateway.test"), httpx.MockTransport(provider))) as client:
            response = client.post("/internal/v1/runs:stream", json=tool_command(), headers={"Authorization": f"Bearer {TOKEN}"})
        output = events(response)
        rounds = [e["payload"] for e in output if e["type"] == "model.round.completed"]
        assert [r["round"] for r in rounds] == [1, 2]
        final = output[-2]["payload"]
        if missing:
            assert rounds[0]["usage"] is None and "usage" not in final
        else:
            assert final["usage"]["totalTokens"] == 14


def test_tool_timeout_has_distinct_code():
    def provider(request):
        if request.url.host == "gateway.test":
            raise httpx.ReadTimeout("private timeout details")
        return httpx.Response(200, text=first_round())
    with TestClient(create_app(replace(SETTINGS, tool_gateway_url="http://gateway.test"), httpx.MockTransport(provider))) as client:
        response = client.post("/internal/v1/runs:stream", json=tool_command(), headers={"Authorization": f"Bearer {TOKEN}"})
    assert events(response)[-2]["payload"]["code"] == "TOOL_TIMEOUT"
    assert "private timeout" not in response.text


def test_unterminated_model_line_is_bounded():
    with TestClient(create_app(SETTINGS, httpx.MockTransport(lambda _: httpx.Response(200, content=b"data: " + b"x" * (1024 * 1024))))) as client:
        response = client.post("/internal/v1/runs:stream", json=command(), headers={"Authorization": f"Bearer {TOKEN}"})
    assert events(response)[-2]["payload"]["code"] == "MODEL_ERROR"
