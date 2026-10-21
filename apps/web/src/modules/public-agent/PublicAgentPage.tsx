import { useEffect, useRef, useState } from 'react';
import { useParams } from 'react-router-dom';
import { Alert, Button, Card, Input, Space, Spin, Typography } from 'antd';
import { consumePublicStream, publicAgentURL, publicInfo } from '../../api/public-agent';
import type { PublicAgentInfo } from '../../api/public-agent';

interface Submission { key: string; message: string; conversationId?: string; reply: number }
interface Message { role: 'user' | 'assistant'; text: string }

export default function PublicAgentPage() {
  const { slug = '' } = useParams();
  // A changed publication URL gets a fresh state and cancels its old stream.
  return <PublicChat key={slug} slug={slug} />;
}

function PublicChat({ slug }: { slug: string }) {
  const [info, setInfo] = useState<PublicAgentInfo>();
  const [messages, setMessages] = useState<Message[]>([]);
  const [text, setText] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [retry, setRetry] = useState<Submission>();
  const conversation = useRef<string | undefined>(undefined);
  const active = useRef<AbortController | null>(null);
  const visitor = useRef(crypto.randomUUID()); // Legacy NestJS namespace, never Go authorization.
  useEffect(() => {
    const controller = new AbortController();
    publicInfo(slug, controller.signal).then(setInfo).catch(e => { if (!controller.signal.aborted) setError(e.message); });
    return () => { controller.abort(); active.current?.abort(); };
  }, [slug]);

  async function send(submission?: Submission) {
    if (active.current || !info || (!submission && !text.trim())) return;
    const request = submission ?? { key: crypto.randomUUID(), message: text.trim(), conversationId: conversation.current, reply: messages.length + 1 };
    if (!submission) { setMessages(previous => [...previous, { role: 'user', text: request.message }, { role: 'assistant', text: '' }]); setText(''); }
    else setMessages(previous => previous.map((m, i) => i === request.reply ? { ...m, text: '' } : m));
    const controller = new AbortController(); active.current = controller;
    setBusy(true); setError(''); setRetry(undefined);
    let failed = false;
    try {
      const response = await fetch(`${publicAgentURL(slug)}/chat/stream`, {
        method: 'POST', credentials: 'include', signal: controller.signal,
        headers: { 'Content-Type': 'application/json', 'Idempotency-Key': request.key },
        body: JSON.stringify({ message: request.message, conversationId: request.conversationId, visitorId: visitor.current }),
      });
      await consumePublicStream(response, event => {
        if (controller.signal.aborted) return;
        if (event.type === 'run.created' && event.conversationId) conversation.current = event.conversationId;
        if (event.type === 'message.delta' || event.type === 'message.completed') setMessages(previous => previous.map((m, i) => i === request.reply ? { ...m, text: event.type === 'message.delta' ? m.text + (event.content ?? '') : (event.content ?? m.text) } : m));
        if (event.type === 'run.failed') { failed = true; setError(event.error || '运行失败，请开始新会话后重试。'); }
      });
      if (failed) setRetry(undefined);
    } catch (e) {
      if (!controller.signal.aborted) { setError(e instanceof Error ? e.message : '请求失败'); if (info.persistedRuns) setRetry(request); }
    } finally {
      if (active.current === controller) { active.current = null; setBusy(false); }
    }
  }

  function reset() { conversation.current = undefined; visitor.current = crypto.randomUUID(); setMessages([]); setRetry(undefined); setError(''); }
  return <main style={{ maxWidth: 840, margin: '0 auto', padding: 24 }}>
    <Typography.Title level={2}>{info?.name ?? '公开应用'}</Typography.Title>
    {info?.description && <Typography.Paragraph>{info.description}</Typography.Paragraph>}
    {!info && !error && <Spin aria-label="加载应用" />}
    {error && <Alert type="error" title={error} showIcon style={{ marginBottom: 16 }} />}
    {info && <>
      {!messages.length && <Typography.Paragraph>{info.openingMessage || '你好，有什么可以帮你？'}</Typography.Paragraph>}
      <section aria-label="聊天记录" aria-live="polite">
        {messages.map((m, i) => <Card key={i} size="small" title={m.role === 'user' ? '你' : info.name} style={{ marginBottom: 12 }}><div style={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>{m.text || (busy ? '正在思考…' : '未收到完整回答')}</div></Card>)}
      </section>
      <form onSubmit={e => { e.preventDefault(); void send(); }}>
        <Input.TextArea aria-label="消息" value={text} onChange={e => setText(e.target.value)} disabled={busy || !!retry} maxLength={8000} autoSize={{ minRows: 3, maxRows: 8 }} placeholder="输入消息" />
        <Space wrap style={{ marginTop: 12 }}>
          <Button type="primary" htmlType="submit" disabled={!text.trim() || busy || !!retry}>发送</Button>
          {busy && <Button onClick={() => { active.current?.abort(); setError('已停止接收回答。'); }}>停止</Button>}
          {retry && <Button disabled={busy} onClick={() => void send(retry)}>重试本次请求</Button>}
          <Button disabled={busy} onClick={reset}>新会话</Button>
        </Space>
      </form>
    </>}
  </main>;
}
