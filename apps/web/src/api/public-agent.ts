import { API_BASE_URL } from './http';

export interface PublicAgentInfo { name: string; description: string | null; openingMessage: string | null; persistedRuns?: boolean; sessionId?: string }
export interface PublicEvent { type: string; runId?: string; eventId?: string; conversationId?: string; content?: string; error?: string; code?: string }
export const publicAgentURL = (slug: string) => `${API_BASE_URL.replace(/\/$/, '')}/public/agents/${encodeURIComponent(slug)}`;

export async function publicInfo(slug: string, signal: AbortSignal): Promise<PublicAgentInfo> {
  const response = await fetch(publicAgentURL(slug), { credentials: 'include', signal });
  if (!response.ok) throw new Error('该应用暂不可用，请确认发布链接或稍后再试。');
  const result = await response.json();
  if (result.code !== 0 || typeof result.data?.name !== 'string') throw new Error('无法读取应用信息。');
  return result.data;
}

export { consumePublicStream } from './public-stream';
