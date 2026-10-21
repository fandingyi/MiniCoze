import type { Response } from 'express';
import type { RuntimeEvent } from '../../shared/types/agent';
import { writeRuntimeEventStream } from './runtime-event-stream.writer';

function response() {
  const chunks: string[] = [];
  const res = {
    headersSent: false,
    writableEnded: false,
    status: jest.fn().mockReturnThis(),
    json: jest.fn(),
    setHeader: jest.fn(),
    flushHeaders: jest.fn(() => {
      res.headersSent = true;
    }),
    write: jest.fn((chunk: string) => {
      chunks.push(chunk);
      return true;
    }),
    end: jest.fn(() => {
      res.writableEnded = true;
    }),
  };
  return { res, chunks, express: res as unknown as Response };
}

describe('runtime SSE writer', () => {
  it('writes compatible ordered frames including the existing end marker', async () => {
    const { res, chunks, express } = response();
    async function* events(): AsyncIterable<RuntimeEvent> {
      yield { type: 'run.created', runId: 'r1', conversationId: 'c1' };
      yield {
        type: 'message.delta',
        runId: 'r1',
        messageId: 'm1',
        content: '你好🌏',
      };
      yield { type: 'run.completed', runId: 'r1' };
      yield { type: 'stream.done', runId: 'r1' };
    }
    await writeRuntimeEventStream(express, events());
    const parsed = chunks.map((frame) =>
      JSON.parse(
        frame
          .split('\n')
          .find((line) => line.startsWith('data: '))!
          .slice(6),
      ),
    );
    expect(parsed.map((event) => event.sequence)).toEqual([1, 2, 3, 4]);
    expect(parsed[1].content).toBe('你好🌏');
    expect(parsed[3].type).toBe('stream.done');
    expect(res.end).toHaveBeenCalledTimes(1);
  });

  it('keeps empty responses as HTTP 204', async () => {
    const { res, chunks, express } = response();
    async function* events(): AsyncIterable<RuntimeEvent> {
      return;
    }
    await writeRuntimeEventStream(express, events());
    expect(res.status).toHaveBeenCalledWith(204);
    expect(chunks).toEqual([]);
  });

  it('keeps pre-stream failures as HTTP errors', async () => {
    const { res, express } = response();
    async function* events(): AsyncIterable<RuntimeEvent> {
      throw new Error('unavailable');
    }
    await writeRuntimeEventStream(express, events());
    expect(res.status).toHaveBeenCalledWith(500);
    expect(res.json).toHaveBeenCalledWith({
      code: 500,
      message: 'unavailable',
      data: null,
    });
    expect(res.flushHeaders).not.toHaveBeenCalled();
  });

  it('preserves the existing transport error behavior after headers', async () => {
    const { res, chunks, express } = response();
    async function* events(): AsyncIterable<RuntimeEvent> {
      yield { type: 'run.created', runId: 'r1', conversationId: 'c1' };
      throw new Error('transport failed');
    }
    await writeRuntimeEventStream(express, events());
    expect(chunks.join('')).toContain(
      'event: error\ndata: {"message":"transport failed"}\n\n',
    );
    expect(res.status).not.toHaveBeenCalled();
    expect(res.end).toHaveBeenCalledTimes(1);
  });
});
