import { consumePublicStream } from '../public-stream';


import { API_BASE_URL, getAuthToken } from '../http';
import { runAgentStreamMock } from './mock-stream';

const useMock = import.meta.env.VITE_USE_AUTH_MOCK === 'true';



export interface TokenUsage {
  inputTokens: number;
  outputTokens: number;
  totalTokens: number;
}

export type RuntimeEventType =
  | 'run.created'
  | 'run.in_progress'
  | 'message.delta'
  | 'message.completed'
  | 'tool.call.created'
  | 'tool.call.completed'
  | 'tool.call.failed'
  | 'knowledge.status'
  | 'run.completed'
  | 'run.failed'
  | 'stream.done';

export interface RunCreatedEvent {
  type: 'run.created';
  runId: string;
  conversationId: string;
}

export interface RunInProgressEvent {
  type: 'run.in_progress';
  runId: string;
}

export type KnowledgeBoundStatus =
  | { bound: false }
  | { bound: true; knowledgeName: string; retrievedCount?: number };

export interface KnowledgeStatusEvent {
  type: 'knowledge.status';
  runId: string;
  knowledge: KnowledgeBoundStatus;
}

export interface MessageDeltaEvent {
  type: 'message.delta';
  runId: string;
  messageId: string;
  content: string;
}

export interface MessageCompletedEvent {
  type: 'message.completed';
  runId: string;
  messageId: string;
  content: string;
}

export interface ToolCallCreatedEvent {
  type: 'tool.call.created';
  runId: string;
  toolCallId: string;
  name: string;
  args: unknown;
}

export interface ToolCallCompletedEvent {
  type: 'tool.call.completed' | 'tool.call.failed';
  runId: string;
  toolCallId: string;
  name: string;
  result: unknown;
  error?: string;
}

export interface RunCompletedEvent {
  type: 'run.completed';
  runId: string;
  usage?: TokenUsage;
}

export interface RunFailedEvent {
  type: 'run.failed';
  runId: string;
  error: string;
}

export interface StreamDoneEvent {
  type: 'stream.done';
  runId: string;
}

export type RuntimeEvent =
  | RunCreatedEvent
  | RunInProgressEvent
  | KnowledgeStatusEvent
  | MessageDeltaEvent
  | MessageCompletedEvent
  | ToolCallCreatedEvent
  | ToolCallCompletedEvent
  | RunCompletedEvent
  | RunFailedEvent
  | StreamDoneEvent;



export interface RunAgentParams {
  /** Reuse only for the same logical request. */
  idempotencyKey?: string;
  agentId: string;
  message: string;
  conversationId?: string;
  preview?: boolean;
  model?: string;
  systemPrompt?: string;
  temperature?: number;
  maxTokens?: number;
  knowledgeBaseId?: string;
  tools?: Array<{ type: 'function'; function: { name: string; description: string; parameters: Record<string, unknown> } }>;
}



export interface RunAgentCallbacks {
  onEvent: (event: RuntimeEvent) => void;
  onError: (error: Error) => void;
}



export async function runAgentStream(
  params: RunAgentParams,
  callbacks: RunAgentCallbacks,
): Promise<AbortController> {
  // Mock 模式下使用模拟流
  if (useMock) {
    return runAgentStreamMock(params, callbacks);
  }

  const token = getAuthToken();
  const controller = new AbortController();

  const url = `${API_BASE_URL.replace(/\/$/, '')}/agent-runs/stream`;

  fetch(url, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'Idempotency-Key': params.idempotencyKey ?? crypto.randomUUID(),
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
    },
    body: JSON.stringify({
      agentId: params.agentId,
      message: params.message,
      conversationId: params.conversationId,
      preview: params.preview,
      model: params.model,
      systemPrompt: params.systemPrompt,
      temperature: params.temperature,
      maxTokens: params.maxTokens,
      knowledgeBaseId: params.knowledgeBaseId,
      tools: params.tools,
    }),
    signal: controller.signal,
  })
    .then(response => consumePublicStream(response, event => callbacks.onEvent(event as RuntimeEvent)))
    .catch((err) => {
      if (err instanceof DOMException && err.name === 'AbortError') {
        return;
      }
      callbacks.onError(err instanceof Error ? err : new Error('网络请求失败'));
    });

  return controller;
}
