import { checkStreamResponse } from './public-stream';

export interface PublicRun {
  id: string;
  status: 'CREATED' | 'RUNNING' | 'COMPLETED' | 'FAILED' | 'CANCELLED';
  conversationId?: string;
  output?: string;
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
