import type { PublicEvent } from './public-agent';

export class StreamHTTPError extends Error {
  constructor(public status: number, public runId?: string, public retryAfter = 1) {
    super(status === 409 ? '运行仍在执行，请查询结果。' : status === 429 ? '请求繁忙，请稍后重试。' : status === 401 || status === 403 ? '访问身份或发布渠道已失效，请重新打开应用。' : `请求失败（${status}）`);
  }
}

export async function checkStreamResponse(response: Response) {
  if (response.ok) return;
  const payload = await response.json().catch(() => null);
  const id = payload?.data?.id;
  throw new StreamHTTPError(response.status, typeof id === 'string' ? id : undefined, retryDelay(response.headers.get('Retry-After')));
}

export function retryDelay(value: string | null, now = Date.now()): number {
  const raw = value?.trim() ?? '';
  const seconds = /^\d+$/.test(raw) ? Number(raw) : /^[A-Za-z]{3},/.test(raw) ? Math.ceil((Date.parse(raw) - now) / 1000) : NaN;
  return Number.isSafeInteger(seconds) && seconds > 0 ? seconds : 1;
}

// Bound buffering and reject truncated success: a closed socket is not completion.
export async function consumePublicStream(response: Response, onEvent: (event: PublicEvent) => void, expectedRunId = response.headers.get('X-Run-Id') ?? undefined) {
  await checkStreamResponse(response);
  if (!response.headers.get('content-type')?.includes('text/event-stream') || !response.body) throw new Error('服务返回了无效的响应。');
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = '', data: string[] = [], size = 0, terminal = false, done = false, runId = '';
  const types = new Set(['run.created', 'run.in_progress', 'message.delta', 'message.completed', 'tool.call.created', 'tool.call.completed', 'tool.call.failed', 'knowledge.status', 'run.completed', 'run.failed', 'stream.done']);
  try {
    while (!done) {
      const chunk = await reader.read();
      if (chunk.done) break;
      buffer += decoder.decode(chunk.value, { stream: true });
      let end: number;
      while ((end = buffer.indexOf('\n')) !== -1) {
        const line = buffer.slice(0, end).replace(/\r$/, '');
        buffer = buffer.slice(end + 1);
        size += line.length;
        if (size > 2 * 1024 * 1024) throw new Error('响应事件过大。');
        if (line === '') {
          if (data.length) {
            const event: PublicEvent = JSON.parse(data.join('\n'));
            if (!event || !types.has(event.type) || typeof event.runId !== 'string' || !event.runId) throw new Error('响应事件格式错误。');
            if (expectedRunId && event.runId !== expectedRunId) throw new Error('响应头与事件的运行身份不一致。');
            if (!runId) {
              if (event.type !== 'run.created' && event.type !== 'run.failed') throw new Error('缺少运行开始事件。');
              runId = event.runId;
            } else if (event.runId !== runId || event.type === 'run.created') throw new Error('运行身份或事件顺序错误。');
            if (terminal && event.type !== 'stream.done') throw new Error('终态之后出现额外事件。');
            if (event.type === 'run.created' && typeof event.conversationId !== 'string') throw new Error('缺少会话信息。');
            if ((event.type === 'message.delta' || event.type === 'message.completed') && typeof event.content !== 'string') throw new Error('消息内容格式错误。');
            if (event.type === 'run.failed' && typeof event.error !== 'string') throw new Error('运行错误格式错误。');
            if (event.type === 'run.completed' || event.type === 'run.failed') terminal = true;
            if (event.type === 'stream.done') { if (!terminal) throw new Error('运行未返回最终状态。'); done = true; }
            onEvent(event);
          }
          data = []; size = 0;
          if (done) break;
        } else if (line.startsWith('data:')) data.push(line.slice(5).replace(/^ /, ''));
      }
      if (buffer.length + size > 2 * 1024 * 1024) throw new Error('响应事件过大。');
    }
    if (!done) throw new Error('连接中断，结果尚未确认。');
  } finally { await reader.cancel().catch(() => {}); reader.releaseLock(); }
}
