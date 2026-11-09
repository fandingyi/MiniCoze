import type { Response } from 'express';
import type { RuntimeEvent } from '../../shared/types/agent';
import {
  RuntimeEventEncoder,
  serializeRuntimeSse,
} from './runtime-event-contract';

export async function writeRuntimeEventStream(
  res: Response,
  events: AsyncIterable<RuntimeEvent>,
): Promise<void> {
  const encoder = new RuntimeEventEncoder();
  try {
    const iterator = events[Symbol.asyncIterator]();
    const first = await iterator.next();

    if (first.done) {
      res.status(204).end();
      return;
    }

    res.setHeader('Content-Type', 'text/event-stream; charset=utf-8');
    res.setHeader('Cache-Control', 'no-cache, no-transform');
    res.setHeader('Connection', 'keep-alive');
    res.flushHeaders?.();

    res.write(serializeRuntimeSse(encoder.encode(first.value)));

    for (;;) {
      const next = await iterator.next();
      if (next.done) {
        break;
      }

      res.write(serializeRuntimeSse(encoder.encode(next.value)));
    }
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);

    if (!res.headersSent) {
      res.status(500).json({
        code: 500,
        message,
        data: null,
      });
      return;
    }

    res.write(`event: error\n`);
    res.write(`data: ${JSON.stringify({ message })}\n\n`);
  } finally {
    if (!res.writableEnded) {
      res.end();
    }
  }
}
