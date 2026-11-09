import asyncio
import hmac
import json
import logging
import time
import os
from contextlib import asynccontextmanager
from dataclasses import dataclass
from datetime import datetime, timezone

import httpx
import anyio
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse, StreamingResponse
from pydantic import ValidationError

from .contracts import RunRequest
from .execution import execute, ModelProtocolError, ToolGatewayError, ToolTimeoutError


@dataclass(frozen=True)
class Settings:
    service_token: str
    model_base_url: str
    model_api_key: str
    allowed_models: frozenset[str]
    tool_gateway_url: str = ""

    @classmethod
    def from_env(cls):
        return cls(
            os.environ.get("RUNTIME_SERVICE_TOKEN", ""),
            os.environ.get("MODEL_BASE_URL", ""),
            os.environ.get("MODEL_API_KEY", ""),
            frozenset(x.strip() for x in os.environ.get("MODEL_ALLOWLIST", "").split(",") if x.strip()),
            os.environ.get("GO_TOOL_GATEWAY_URL", ""),
        )


class DisconnectAwareStream(StreamingResponse):
    """Cancel the upstream read even when the model hasn't emitted another token."""

    def __init__(self, *args, release, **kwargs):
        super().__init__(*args, **kwargs)
        self.release = release

    async def __call__(self, scope, receive, send):
        try:
            async with anyio.create_task_group() as group:
                async def stream():
                    try:
                        await self.stream_response(send)
                    except OSError:
                        pass  # Client disconnected during a send.
                    finally:
                        group.cancel_scope.cancel()

                group.start_soon(stream)
                await self.listen_for_disconnect(receive)
                group.cancel_scope.cancel()
        finally:
            try:
                await self.body_iterator.aclose()
            finally:
                self.release()


def create_app(settings: Settings | None = None, transport=None):
    settings = settings or Settings.from_env()
    if len(settings.service_token) < 32 or not settings.model_api_key or not settings.allowed_models:
        raise ValueError("Configure service token (32+ characters), model key and allowlist")
    base_url = httpx.URL(settings.model_base_url)
    if base_url.scheme not in ("http", "https") or not base_url.host or base_url.userinfo or base_url.query or base_url.fragment:
        raise ValueError("Configure a trusted MODEL_BASE_URL")

    if settings.tool_gateway_url:
        gateway = httpx.URL(settings.tool_gateway_url)
        if gateway.scheme not in ("http", "https") or not gateway.host or gateway.userinfo or gateway.query or gateway.fragment or gateway.path not in ("", "/"):
            raise ValueError("Configure a trusted GO_TOOL_GATEWAY_URL origin")

    @asynccontextmanager
    async def lifespan(app):
        async with httpx.AsyncClient(
            transport=transport, trust_env=False, follow_redirects=False,
            timeout=httpx.Timeout(30, connect=5),
            limits=httpx.Limits(max_connections=32, max_keepalive_connections=16),
        ) as client:
            app.state.client = client
            yield

    app = FastAPI(lifespan=lifespan, docs_url=None, redoc_url=None, openapi_url=None)
    slots = asyncio.Semaphore(16)

    @app.get("/healthz")
    async def health():
        return {"status": "ok"}

    @app.get("/internal/v1/ready")
    async def readiness(request: Request):
        expected = f"Bearer {settings.service_token}"
        if not hmac.compare_digest(request.headers.get("authorization", "").encode(), expected.encode()):
            return JSONResponse({"code": "UNAUTHORIZED"}, status_code=401)
        client = getattr(app.state, "client", None)
        if client is None or client.is_closed:
            return JSONResponse({"status": "not_ready"}, status_code=503)
        # No model call and no callback to Go: avoid cost and cyclic probes.
        return {"status": "ready", "tools_ready": bool(settings.tool_gateway_url)}

    @app.post("/internal/v1/runs:stream")
    async def run(request: Request):
        expected = f"Bearer {settings.service_token}"
        if not hmac.compare_digest(request.headers.get("authorization", "").encode(), expected.encode()):
            return JSONResponse({"code": "UNAUTHORIZED"}, status_code=401)
        body = bytearray()
        async for chunk in request.stream():
            body.extend(chunk)
            if len(body) > 256 * 1024:
                return JSONResponse({"code": "REQUEST_TOO_LARGE"}, status_code=413)
        try:
            command = RunRequest.model_validate_json(body)
        except ValidationError:
            # Do not return Pydantic's input echo (prompts and user data).
            return JSONResponse({"code": "INVALID_RUNTIME_REQUEST"}, status_code=422)
        if command.snapshot.model.name not in settings.allowed_models:
            return JSONResponse({"code": "MODEL_NOT_ALLOWED"}, status_code=422)
        if command.snapshot.tools and (command.mode not in ("draft", "published") or not settings.tool_gateway_url):
            return JSONResponse({"code": "TOOLS_NOT_CONFIGURED"}, status_code=422)
        if slots.locked():
            return JSONResponse({"code": "RUNTIME_BUSY"}, status_code=429)
        await slots.acquire()

        async def events():
            sequence = 0
            started = time.monotonic()
            terminal = "interrupted"
            logger = logging.getLogger("uvicorn.error")
            logger.info(json.dumps({"event": "runtime.started", "run_id": command.run_id}))

            def event(kind, payload):
                nonlocal sequence, terminal
                sequence += 1
                if kind in ("run.completed", "run.failed"):
                    terminal = kind
                    logger.info(json.dumps({"event": "runtime.terminal", "run_id": command.run_id,
                                           "status": kind, "duration_ms": int((time.monotonic() - started) * 1000)}))
                envelope = {
                    "schema_version": 1, "run_id": command.run_id,
                    "event_id": f"{command.run_id}:{sequence}", "sequence": sequence,
                    "timestamp": datetime.now(timezone.utc).isoformat(),
                    "type": kind, "payload": payload,
                }
                return f"event: {kind}\ndata: {json.dumps(envelope, ensure_ascii=False)}\n\n"

            try:
                yield event("run.created", {"conversationId": command.actor.conversation_id or f"preview-{command.run_id}"})
                yield event("run.in_progress", {})
                async with asyncio.timeout(command.limits.deadline_ms / 1000):
                    async for kind, data in execute(request.app.state.client, settings, command):
                        yield event(kind, data)
            except ToolTimeoutError:
                yield event("run.failed", {"error": "工具请求超时", "code": "TOOL_TIMEOUT"})
            except ToolGatewayError:
                yield event("run.failed", {"error": "工具调用失败或超过执行预算", "code": "TOOL_GATEWAY_ERROR"})
            except (TimeoutError, httpx.TimeoutException):
                yield event("run.failed", {"error": "模型请求超时", "code": "MODEL_TIMEOUT"})
            except (httpx.HTTPError, ModelProtocolError, ValueError, TypeError, KeyError, AttributeError):
                yield event("run.failed", {"error": "模型服务异常", "code": "MODEL_ERROR"})
            finally:
                if terminal == "interrupted":
                    logger.info(json.dumps({"event": "runtime.interrupted", "run_id": command.run_id,
                                           "duration_ms": int((time.monotonic() - started) * 1000)}))
            yield event("stream.done", {})

        return DisconnectAwareStream(events(), release=slots.release, media_type="text/event-stream", headers={"Cache-Control": "no-cache", "X-Accel-Buffering": "no"})

    return app
