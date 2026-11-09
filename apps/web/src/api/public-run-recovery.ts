export interface PendingPublicRun {
  version: 1;
  key: string;
  message: string;
  reply: number;
  conversationId?: string;
  runId?: string;
  lastEventId?: string;
  createdAt: number;
}

const prefix = 'minicoze:public-run:v1:';
const maxAge = 10 * 60 * 1000;

export const publicRunStorageKey = (slug: string) => `${prefix}${slug}`;

export function publicRunSessionStorage(): Storage | undefined {
  try { return window.sessionStorage; } catch { return undefined; }
}

function valid(value: unknown, now: number): value is PendingPublicRun {
  if (!value || typeof value !== 'object') return false;
  const run = value as Partial<PendingPublicRun>;
  return run.version === 1
    && typeof run.key === 'string' && /^[A-Za-z0-9_.:-]{8,128}$/.test(run.key)
    && typeof run.message === 'string' && run.message.length > 0 && run.message.length <= 8000
    && typeof run.reply === 'number' && Number.isInteger(run.reply) && run.reply >= 1 && run.reply <= 10000
    && (run.conversationId == null || (typeof run.conversationId === 'string' && run.conversationId.length <= 128))
    && (run.runId == null || (typeof run.runId === 'string' && run.runId.length > 0 && run.runId.length <= 128))
    && (run.lastEventId == null || (typeof run.lastEventId === 'string' && /^[^:\s]{1,128}:\d+$/.test(run.lastEventId)))
    && typeof run.createdAt === 'number' && Number.isFinite(run.createdAt)
    && run.createdAt <= now + 60 * 1000 && now - run.createdAt <= maxAge;
}

export function readPendingPublicRun(storage: Storage, slug: string, now = Date.now()): PendingPublicRun | undefined {
  const key = publicRunStorageKey(slug);
  try {
    const raw = storage.getItem(key);
    if (!raw) return undefined;
    const value: unknown = JSON.parse(raw);
    if (valid(value, now)) return value;
  } catch {
    // A broken or unavailable browser store must not block public chat.
  }
  try { storage.removeItem(key); } catch { /* ignore */ }
  return undefined;
}

export function writePendingPublicRun(storage: Storage, slug: string, run: PendingPublicRun) {
  try { storage.setItem(publicRunStorageKey(slug), JSON.stringify(run)); } catch { /* best effort */ }
}

export function clearPendingPublicRun(storage: Storage, slug: string) {
  try { storage.removeItem(publicRunStorageKey(slug)); } catch { /* best effort */ }
}
