import type { PublicEvent } from './public-agent';

// Bound buffering and reject truncated success: a closed socket is not completion.
export async function consumePublicStream(response: Response, onEvent: (event: PublicEvent) => void) {
  if (!response.ok) throw new Error(`请求失败（${response.status}），请稍后重试或开始新会话。`);
  if (!response.headers.get('content-type')?.includes('text/event-stream') || !response.body) throw new Error('服务返回了无效的响应。');
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = '', data: string[] = [], size = 0, terminal = false, done = false;
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
            if (!event || typeof event.type !== 'string') throw new Error('响应事件格式错误。');
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
