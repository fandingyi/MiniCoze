"""Bounded model/tool loop. Only Go's gateway can execute a tool."""
import httpx
import json
import re
import time
from uuid import uuid4


class ModelProtocolError(Exception):
    pass


class ToolGatewayError(Exception):
    pass


class ToolTimeoutError(Exception):
    pass


async def bounded_model_lines(response):
    """Bound a provider line before its delimiter arrives; aiter_lines alone
    can accumulate an arbitrarily long unterminated SSE line."""
    pending = bytearray()
    async for chunk in response.aiter_bytes():
        if len(pending) + len(chunk) > 2 * 1024 * 1024:
            raise ModelProtocolError()
        pending.extend(chunk)
        while (end := pending.find(b"\n")) >= 0:
            if end > 1024 * 1024:
                raise ModelProtocolError()
            line = bytes(pending[:end]).rstrip(b"\r").decode("utf-8")
            del pending[:end + 1]
            yield line
        if len(pending) > 1024 * 1024:
            raise ModelProtocolError()
    if pending:
        yield bytes(pending).rstrip(b"\r").decode("utf-8")


async def execute(client, settings, command):
    messages = [{"role": "system", "content": command.snapshot.instructions},
                *[m.model_dump() for m in command.history],
                {"role": "user", "content": command.input.text}]
    tools = [t.model_dump() for t in command.snapshot.tools]
    allowed = {t["function"]["name"] for t in tools}
    if len(allowed) != len(tools):
        raise ModelProtocolError()
    message_id = uuid4().hex
    all_parts, used_ids, output_size = [], set(), 0
    totals = dict(inputTokens=0, outputTokens=0, totalTokens=0)
    complete_usage = True
    for round_index in range(5):
        round_started = time.monotonic()
        payload = {"model": command.snapshot.model.name, "messages": messages,
                   "temperature": command.snapshot.model.temperature,
                   "max_tokens": command.limits.max_output_tokens,
                   "stream": True, "stream_options": {"include_usage": True}}
        if tools:
            payload.update(tools=tools, tool_choice="auto", parallel_tool_calls=False)
        calls, parts, finish, ended, usage = {}, [], None, False, None
        async with client.stream("POST", settings.model_base_url.rstrip("/") + "/chat/completions",
                                 headers={"Authorization": f"Bearer {settings.model_api_key}"}, json=payload) as response:
            response.raise_for_status()
            async for line in bounded_model_lines(response):
                if not line.startswith("data:"):
                    continue
                data = line[5:].strip()
                if data == "[DONE]":
                    ended = True
                    break
                if len(data) > 1024 * 1024:
                    raise ModelProtocolError()
                chunk = json.loads(data)
                if not isinstance(chunk, dict) or chunk.get("error"):
                    raise ModelProtocolError()
                if chunk.get("usage") is not None:
                    counts = [chunk["usage"].get(k) for k in ("prompt_tokens", "completion_tokens", "total_tokens")]
                    if any(type(v) is not int or v < 0 for v in counts):
                        raise ModelProtocolError()
                    usage = dict(zip(totals, counts))
                for choice in chunk.get("choices", []):
                    if choice.get("index", 0) != 0:
                        continue
                    delta = choice.get("delta", {})
                    if delta.get("function_call"):
                        raise ModelProtocolError()
                    text = delta.get("content")
                    if text is not None:
                        if not isinstance(text, str) or finish is not None:
                            raise ModelProtocolError()
                        output_size += len(text.encode())
                        if output_size > 1024 * 1024:
                            raise ModelProtocolError()
                        parts.append(text)
                        all_parts.append(text)
                        if text:
                            yield "message.delta", {"messageId": message_id, "content": text}
                    for fragment in delta.get("tool_calls", []):
                        index = fragment.get("index")
                        if not tools or finish is not None or type(index) is not int or not 0 <= index < 8:
                            raise ModelProtocolError()
                        call = calls.setdefault(index, {"id": "", "type": "function", "function": {"name": "", "arguments": ""}})
                        if fragment.get("type", "function") != "function":
                            raise ModelProtocolError()
                        call["id"] += fragment.get("id", "")
                        for field in ("name", "arguments"):
                            call["function"][field] += fragment.get("function", {}).get(field, "")
                        if len(call["id"]) > 128 or len(call["function"]["name"]) > 64 or len(call["function"]["arguments"].encode()) > 8192:
                            raise ModelProtocolError()
                    if choice.get("finish_reason"):
                        if finish is not None:
                            raise ModelProtocolError()
                        finish = choice["finish_reason"]
        if not ended:
            raise ModelProtocolError()
        if finish not in ("stop", "length", "tool_calls"):
            raise ModelProtocolError()
        if command.mode != "preview":
            yield "model.round.completed", {"round": round_index + 1,
                "durationMs": min(120000, int((time.monotonic() - round_started) * 1000)),
                "finishReason": finish, "usage": usage}
        if usage is None:
            complete_usage = False
        else:
            for key in totals:
                totals[key] += usage[key]
        if not calls:
            if finish not in ("stop", "length"):
                raise ModelProtocolError()
            yield "message.completed", {"messageId": message_id, "content": "".join(all_parts)}
            result = {"finishReason": finish}
            if complete_usage:
                result["usage"] = totals
            yield "run.completed", result
            return
        if finish != "tool_calls" or round_index == 4 or len(used_ids) + len(calls) > 8 or not settings.tool_gateway_url:
            raise ToolGatewayError()
        prepared = []
        for index in sorted(calls):
            call = calls[index]
            name, call_id = call["function"]["name"], call["id"]
            if name not in allowed or not re.fullmatch(r"[a-zA-Z0-9_-]{1,128}", call_id) or call_id in used_ids:
                raise ToolGatewayError()
            arguments = json.loads(call["function"]["arguments"])
            if not isinstance(arguments, dict):
                raise ToolGatewayError()
            used_ids.add(call_id)
            prepared.append((call, arguments))
        messages.append({"role": "assistant", "content": "".join(parts) or None, "tool_calls": list(calls.values())})
        for call, arguments in prepared:
            name, call_id = call["function"]["name"], call["id"]
            yield "tool.call.created", {"toolCallId": call_id, "name": name, "args": arguments}
            try:
                async with client.stream("POST", settings.tool_gateway_url.rstrip("/") + f"/internal/v1/runs/{command.run_id}/tools",
                                         headers={"Authorization": f"Bearer {settings.service_token}"},
                                         json={"call_id": call_id, "name": name, "arguments": arguments}, timeout=6) as response:
                    if response.status_code != 200:
                        raise ToolGatewayError()
                    body = bytearray()
                    async for chunk in response.aiter_bytes():
                        body.extend(chunk)
                        if len(body) > 128 * 1024:
                            raise ToolGatewayError()
                    result = json.loads(body)
            except httpx.TimeoutException as exc:
                raise ToolTimeoutError() from exc
            if result.get("call_id") != call_id or result.get("name") != name or result.get("status") not in ("SUCCESS", "FAILED"):
                raise ToolGatewayError()
            event = {"toolCallId": call_id, "name": name, "result": result.get("output")}
            if result["status"] == "FAILED":
                event["error"] = "TOOL_EXECUTION_FAILED"
            yield "tool.call.completed", event
            messages.append({"role": "tool", "tool_call_id": call_id,
                             "content": json.dumps({"output": result.get("output"), "error": event.get("error")}, ensure_ascii=False)})
    raise ToolGatewayError()
