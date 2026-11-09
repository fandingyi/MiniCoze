import type { RuntimeEvent } from '../../shared/types/agent';

export const RUNTIME_EVENT_SCHEMA_VERSION = 1 as const;

export interface RuntimeEventMetadata {
  schema_version: typeof RUNTIME_EVENT_SCHEMA_VERSION;
  event_id: string;
  sequence: number;
  timestamp: string;
}

// Keep payload keys compatible with the existing browser contract in v1.
export type RuntimeEventEnvelope = {
  [K in RuntimeEvent['type']]: RuntimeEventMetadata & {
    run_id: string;
    type: K;
    payload: Omit<Extract<RuntimeEvent, { type: K }>, 'type' | 'runId'>;
  };
}[RuntimeEvent['type']];

export type VersionedRuntimeEvent = RuntimeEvent & RuntimeEventMetadata;

/** One instance per run stream; this is event identity, not durable replay. */
export class RuntimeEventEncoder {
  private sequence = 0;
  private runId?: string;

  constructor(private readonly now: () => Date = () => new Date()) {}

  encode(event: RuntimeEvent): RuntimeEventEnvelope {
    if (!event.runId || /[\r\n]/.test(event.runId)) {
      throw new Error('Invalid runtime run ID');
    }
    if (this.runId !== undefined && this.runId !== event.runId) {
      throw new Error('A runtime stream must belong to one run');
    }
    this.runId = event.runId;
    const { type, runId, ...payload } = event;
    const sequence = ++this.sequence;
    return {
      schema_version: RUNTIME_EVENT_SCHEMA_VERSION,
      event_id: `${runId}:${sequence}`,
      run_id: runId,
      sequence,
      timestamp: this.now().toISOString(),
      type,
      payload,
    } as RuntimeEventEnvelope;
  }
}

/** Internal envelopes must never replace the browser's flat event shape. */
export function toBrowserRuntimeEvent(
  event: RuntimeEventEnvelope,
): VersionedRuntimeEvent {
  return {
    ...event.payload,
    type: event.type,
    runId: event.run_id,
    schema_version: event.schema_version,
    event_id: event.event_id,
    sequence: event.sequence,
    timestamp: event.timestamp,
  } as VersionedRuntimeEvent;
}

export function serializeRuntimeSse(event: RuntimeEventEnvelope): string {
  return `id: ${event.event_id}\nevent: ${event.type}\ndata: ${JSON.stringify(toBrowserRuntimeEvent(event))}\n\n`;
}
