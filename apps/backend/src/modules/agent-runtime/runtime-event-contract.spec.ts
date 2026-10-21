import type { RuntimeEvent } from '../../shared/types/agent';
import {
  RuntimeEventEncoder,
  serializeRuntimeSse,
  toBrowserRuntimeEvent,
} from './runtime-event-contract';

const samples: RuntimeEvent[] = [
  { type: 'run.created', runId: 'r1', conversationId: 'c1' },
  { type: 'run.in_progress', runId: 'r1' },
  { type: 'knowledge.status', runId: 'r1', knowledge: { bound: false } },
  {
    type: 'message.delta',
    runId: 'r1',
    messageId: 'm1',
    content: '你好🌏\n下一行',
  },
  {
    type: 'message.completed',
    runId: 'r1',
    messageId: 'm1',
    content: '你好🌏\n下一行',
  },
  {
    type: 'tool.call.created',
    runId: 'r1',
    toolCallId: 't1',
    name: 'lookup',
    args: { q: '文档' },
    pluginId: 'p1',
  },
  {
    type: 'tool.call.completed',
    runId: 'r1',
    toolCallId: 't1',
    name: 'lookup',
    result: { found: true },
  },
  {
    type: 'tool.call.failed',
    runId: 'r1',
    toolCallId: 't1',
    name: 'lookup',
    error: 'timeout',
  },
  {
    type: 'run.completed',
    runId: 'r1',
    usage: { inputTokens: 2, outputTokens: 3, totalTokens: 5 },
  },
  { type: 'run.failed', runId: 'r1', error: '模型不可用' },
  { type: 'stream.done', runId: 'r1' },
];

describe('runtime event v1 compatibility', () => {
  it.each(samples)(
    'preserves existing $type fields through SSE JSON',
    (event) => {
      const encoder = new RuntimeEventEncoder(
        () => new Date('2026-10-10T00:00:00Z'),
      );
      const envelope = encoder.encode(event);
      const frame = serializeRuntimeSse(envelope);
      const data = frame.split('\n').find((line) => line.startsWith('data: '))!;
      const decoded = JSON.parse(data.slice(6));
      expect(decoded).toEqual({
        ...event,
        schema_version: 1,
        event_id: 'r1:1',
        sequence: 1,
        timestamp: '2026-10-10T00:00:00.000Z',
      });
      expect(frame).toContain(`event: ${event.type}\n`);
      expect(frame.endsWith('\n\n')).toBe(true);
      expect(decoded.payload).toBeUndefined();
      expect(envelope.payload).not.toHaveProperty('runId');
      expect(envelope.payload).not.toHaveProperty('type');
    },
  );

  it('assigns ordered IDs and isolates concurrent streams', () => {
    const first = new RuntimeEventEncoder();
    const second = new RuntimeEventEncoder();
    expect(first.encode(samples[0]).event_id).toBe('r1:1');
    expect(first.encode(samples[1]).event_id).toBe('r1:2');
    expect(
      second.encode({ type: 'run.in_progress', runId: 'r2' }).event_id,
    ).toBe('r2:1');
  });

  it('rejects mixed runs and newline injection without advancing the sequence', () => {
    const encoder = new RuntimeEventEncoder();
    encoder.encode(samples[0]);
    expect(() =>
      encoder.encode({ type: 'run.in_progress', runId: 'r2' }),
    ).toThrow();
    expect(() =>
      encoder.encode({ type: 'run.in_progress', runId: 'r1\nevent: bad' }),
    ).toThrow();
    expect(encoder.encode(samples[1]).sequence).toBe(2);
  });

  it('does not mutate the source event', () => {
    const event = Object.freeze({ ...samples[0] });
    toBrowserRuntimeEvent(new RuntimeEventEncoder().encode(event));
    expect(event).not.toHaveProperty('sequence');
  });
});
