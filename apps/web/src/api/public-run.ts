import { checkStreamResponse, consumePublicStream } from './public-stream';
import type { PublicEvent } from './public-agent';

export interface PublicRun {
  id: string;
  status: 'CREATED' | 'RUNNING' | 'COMPLETED' | 'FAILED' | 'CANCELLED';
  conversationId?: string;
  output?: string;
}

// A reconnect is a bounded catch-up request: the server writes durable frames
// after Last-Event-ID and closes. A normal stream remains responsible for live
// delivery, while the status query below remains the final source of truth.
export async function replayPublicRunEvents(baseURL: string, runId: string, sessionId: string | undefined, lastEventId: string | undefined, signal: AbortSignal, onEvent: (event: PublicEvent) => void) {
  const response = await fetch(`${baseURL}/runs/${encodeURIComponent(runId)}/events`, {
    credentials: 'include', signal,
    headers: { ...(sessionId ? { 'X-Visitor-Session': sessionId } : {}), ...(lastEventId ? { 'Last-Event-ID': lastEventId } : {}) },
  });
  await consumePublicStream(response, onEvent, runId, true);
}

// Bound both headers and body reads; cancellation outcomes must come from the server.
export async function publicRun(baseURL: string, runId: string, sessionId: string | undefined, signal: AbortSignal, cancel = false, timeoutMs = 10000): Promise<PublicRun> {
  const controller = new AbortController();
  const abort = () => controller.abort(signal.reason);
  signal.addEventListener('abort', abort, { once: true });
  if (signal.aborted) abort();
  let timedOut = false;
  const timer = setTimeout(() => { timedOut = true; controller.abort(); }, timeoutMs);
  try {
    const response = await fetch(`${baseURL}/runs/${encodeURIComponent(runId)}${cancel ? '/cancel' : ''}`, {
      method: cancel ? 'POST' : 'GET', credentials: 'include', signal: controller.signal,
      headers: sessionId ? { 'X-Visitor-Session': sessionId } : {},
    });
    await checkStreamResponse(response);
    const { code, data: run } = await response.json();
    if (code !== 0 || run?.id !== runId || !['CREATED', 'RUNNING', 'COMPLETED', 'FAILED', 'CANCELLED'].includes(run.status)
      || (run.conversationId != null && typeof run.conversationId !== 'string')
      || (run.status === 'COMPLETED' && typeof run.output !== 'string')) throw new Error('运行查询返回了无效状态。');
    return run;
  } catch (error) {
    if (timedOut && !signal.aborted) throw new Error('运行状态请求超时，结果尚未确认，请查询原运行。');
    throw error;
  } finally {
    clearTimeout(timer);
    signal.removeEventListener('abort', abort);
  }
}
