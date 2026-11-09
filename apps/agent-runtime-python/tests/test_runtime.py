import json

import httpx
import pytest
from fastapi.testclient import TestClient

from runtime.app import Settings, create_app

TOKEN = "test-runtime-service-token-32-characters"
SETTINGS = Settings(TOKEN, "http://model.test/v1", "test-model-secret", frozenset({"test-model"}))


def command():
    return {
        "schema_version": 1, "run_id": "run-1", "mode": "preview",
        "actor": {"user_id": "u1", "workspace_id": "w1", "agent_id": "a1"},
        "snapshot": {"snapshot_schema_version": 1, "model": {"name": "test-model", "temperature": 0.7}, "instructions": "test instructions", "tools": [], "knowledge": []},
        "input": {"text": "你好"}, "limits": {"deadline_ms": 1000, "max_output_tokens": 128},
    }


def data(value):
    return "data: " + (value if isinstance(value, str) else json.dumps(value, ensure_ascii=False)) + "\n\n"


def model_stream(text="你好🌏\n下一行"):
    return (
        data({"choices": [{"index": 0, "delta": {"content": text}, "finish_reason": None}]})
        + data({"choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]})
        + data({"choices": [], "usage": {"prompt_tokens": 4, "completion_tokens": 3, "total_tokens": 7}})
        + data("[DONE]")
    )


def events(response):
    assert response.status_code == 200
    return [json.loads(line[5:]) for line in response.text.splitlines() if line.startswith("data:")]


def test_authenticated_stream_and_snapshot():
    def provider(request):
        assert request.headers["authorization"] == "Bearer test-model-secret"
        payload = json.loads(request.content)
        assert payload["messages"][0]["content"] == "test instructions"
        assert payload["messages"][1]["content"] == "你好"
        assert payload["max_tokens"] == 128
        return httpx.Response(200, text=model_stream())

    with TestClient(create_app(SETTINGS, httpx.MockTransport(provider))) as client:
        response = client.post("/internal/v1/runs:stream", json=command(), headers={"Authorization": f"Bearer {TOKEN}"})
    output = events(response)
    assert [e["type"] for e in output] == ["run.created", "run.in_progress", "message.delta", "message.completed", "run.completed", "stream.done"]
    assert [e["sequence"] for e in output] == list(range(1, 7))
    assert output[2]["payload"]["content"] == "你好🌏\n下一行"
    assert output[4]["payload"]["usage"]["totalTokens"] == 7
    assert all(e["event_id"] == f"run-1:{e['sequence']}" for e in output)
    assert "test-model-secret" not in response.text


@pytest.mark.parametrize("mode", ["draft", "published"])
def test_persisted_history_and_conversation(mode):
    payload = command()
    payload["mode"] = mode
    payload["actor"]["conversation_id"] = "saved-conversation"
    payload["history"] = [{"role": "user", "content": "previous question"}, {"role": "assistant", "content": "previous answer"}]

    def provider(request):
        messages = json.loads(request.content)["messages"]
        assert [m["role"] for m in messages] == ["system", "user", "assistant", "user"]
        assert messages[1:3] == payload["history"]
        return httpx.Response(200, text=model_stream())

    with TestClient(create_app(SETTINGS, httpx.MockTransport(provider))) as client:
        response = client.post("/internal/v1/runs:stream", json=payload, headers={"Authorization": f"Bearer {TOKEN}"})
    assert events(response)[0]["payload"]["conversationId"] == "saved-conversation"


@pytest.mark.parametrize("authorization", ["", "Bearer invalid", "Basic test"])
def test_authentication(authorization):
    with TestClient(create_app(SETTINGS)) as client:
        response = client.post("/internal/v1/runs:stream", json=command(), headers={"Authorization": authorization})
    assert response.status_code == 401


@pytest.mark.parametrize("mutation", [
    lambda c: c.update(mode="invalid-mode"),
    lambda c: c.update(user_id="forged"),
    lambda c: c["snapshot"].update(tools=[{"name": "delete"}]),
    lambda c: c["snapshot"].update(api_key="must-not-echo"),
    lambda c: c["limits"].update(deadline_ms=0),
    lambda c: c["limits"].update(max_output_tokens=100000),
    lambda c: c["snapshot"]["model"].update(name="not-allowed"),
])
def test_invalid_contract(mutation):
    payload = command()
    mutation(payload)
    with TestClient(create_app(SETTINGS)) as client:
        response = client.post("/internal/v1/runs:stream", json=payload, headers={"Authorization": f"Bearer {TOKEN}"})
    assert response.status_code == 422
    assert "must-not-echo" not in response.text


@pytest.mark.parametrize("stream", [
    data({"choices": [{"delta": {"content": "partial"}}]}),
    data("[DONE]"),
    data({"choices": [{"delta": {"tool_calls": [{}]}}]}),
    data({"error": {"message": "secret upstream text"}}),
    data("broken-json"),
])
def test_bad_model_stream_is_failed_not_completed(stream):
    with TestClient(create_app(SETTINGS, httpx.MockTransport(lambda _: httpx.Response(200, text=stream)))) as client:
        response = client.post("/internal/v1/runs:stream", json=command(), headers={"Authorization": f"Bearer {TOKEN}"})
    kinds = [e["type"] for e in events(response)]
    assert kinds.count("run.failed") == 1
    assert "run.completed" not in kinds
    assert kinds[-1] == "stream.done"
    assert "secret upstream text" not in response.text


def test_timeout_and_provider_error():
    def timeout(_):
        raise httpx.ReadTimeout("secret exception")

    for transport in [httpx.MockTransport(timeout), httpx.MockTransport(lambda _: httpx.Response(500, text="secret exception"))]:
        with TestClient(create_app(SETTINGS, transport)) as client:
            response = client.post("/internal/v1/runs:stream", json=command(), headers={"Authorization": f"Bearer {TOKEN}"})
        assert events(response)[-2]["type"] == "run.failed"
        assert "secret exception" not in response.text


def test_request_size_limit():
    with TestClient(create_app(SETTINGS)) as client:
        response = client.post("/internal/v1/runs:stream", content=b"x" * (256 * 1024 + 1), headers={"Authorization": f"Bearer {TOKEN}"})
    assert response.status_code == 413
