from typing import Literal

from pydantic import BaseModel, ConfigDict, Field


class StrictModel(BaseModel):
    model_config = ConfigDict(extra="forbid", strict=True, frozen=True)


class Actor(StrictModel):
    user_id: str = Field(min_length=1, max_length=128)
    workspace_id: str = Field(min_length=1, max_length=128)
    agent_id: str = Field(min_length=1, max_length=128)
    conversation_id: str | None = Field(default=None, min_length=1, max_length=128)


class Model(StrictModel):
    name: str = Field(min_length=1, max_length=128)
    temperature: float = Field(ge=0, le=2, allow_inf_nan=False)


class Function(StrictModel):
    name: str = Field(pattern=r"^[a-zA-Z0-9_-]{1,64}$")
    description: str = Field(max_length=2000)
    parameters: dict


class Tool(StrictModel):
    type: Literal["function"]
    function: Function


class Snapshot(StrictModel):
    snapshot_schema_version: Literal[1]
    model: Model
    instructions: str = Field(max_length=64000)
    # Capabilities are explicit: day 3 cannot silently ignore configured tools.
    tools: list[Tool] = Field(default_factory=list, max_length=8)
    knowledge: list = Field(default_factory=list, max_length=0)


class Input(StrictModel):
    text: str = Field(min_length=1, max_length=32000)


class Limits(StrictModel):
    deadline_ms: int = Field(ge=100, le=120000)
    max_output_tokens: int = Field(ge=1, le=8192)


class HistoryMessage(StrictModel):
    role: Literal["user", "assistant"]
    content: str = Field(max_length=32000)


class RunRequest(StrictModel):
    schema_version: Literal[1]
    run_id: str = Field(pattern=r"^[a-zA-Z0-9_-]{1,128}$")
    mode: Literal["preview", "draft", "published"]
    actor: Actor
    snapshot: Snapshot
    input: Input
    limits: Limits
    history: list[HistoryMessage] = Field(default_factory=list, max_length=50)
