import { useEffect, useRef, useState } from 'react';
import { useParams } from 'react-router-dom';
import { Alert, Button, Card, Input, Space, Spin, Typography } from 'antd';
import { consumePublicStream, publicAgentURL, publicInfo } from '../../api/public-agent';
import type { PublicAgentInfo } from '../../api/public-agent';
import { StreamHTTPError } from '../../api/public-stream';
import { publicRun } from '../../api/public-run';
import { clearPendingPublicRun, publicRunSessionStorage, readPendingPublicRun, writePendingPublicRun } from '../../api/public-run-recovery';

interface Submission { key: string; message: string; conversationId?: string; reply: number; runId?: string }
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
  const [cooldown, setCooldown] = useState(0);
  useEffect(() => { if (!cooldown) return; const timer = setTimeout(() => setCooldown(value => Math.max(0, value - 1)), 1000); return () => clearTimeout(timer); }, [cooldown]);
  const conversation = useRef<string | undefined>(undefined);
  const active = useRef<AbortController | null>(null);
  const visitor = useRef(crypto.randomUUID()); // Legacy NestJS namespace, never Go authorization.
  const restored = useRef(false);
  const remember = (request: Submission) => {
    const storage = publicRunSessionStorage();
    if (storage) writePendingPublicRun(storage, slug, { version: 1, ...request, createdAt: Date.now() });
  };
  const forget = () => {
    const storage = publicRunSessionStorage();
    if (storage) clearPendingPublicRun(storage, slug);
  };
  useEffect(() => {
    const controller = new AbortController();
    publicInfo(slug, controller.signal).then(setInfo).catch(e => { if (!controller.signal.aborted) setError(e.message); });
    return () => { controller.abort(); active.current?.abort(); };
  }, [slug]);
  useEffect(() => {
    if (!info?.persistedRuns || restored.current) return;
    restored.current = true;
    const storage = publicRunSessionStorage();
    const pending = storage && readPendingPublicRun(storage, slug);
    const request = pending && { ...pending, reply: 1 };
    if (!request) return;
    if (!request.runId) {
      setMessages([{ role: 'user', text: request.message }, { role: 'assistant', text: '' }]);
      setRetry(request);
      setError('上次请求结果尚未确认。可使用原请求标识重试，避免重复执行。');
      return;
    }
    const controller = new AbortController();
    setBusy(true);
    setMessages([{ role: 'user', text: request.message }, { role: 'assistant', text: '' }]);
    publicRun(publicAgentURL(slug), request.runId, info.sessionId, controller.signal).then(run => {
      if (run.conversationId) conversation.current = run.conversationId;
      if (run.status === 'COMPLETED') {
        setMessages([{ role: 'user', text: request.message }, { role: 'assistant', text: run.output ?? '' }]);
        forget();
      } else if (run.status === 'FAILED' || run.status === 'CANCELLED') {
        setText(request.message); forget();
        setError(run.status === 'CANCELLED' ? '上次运行已取消。可编辑消息重新发送。' : '上次运行已失败。可编辑消息重新发送。');
      } else {
        setRetry(request); setCooldown(1);
        setError('已恢复上次未完成运行。可查询结果或取消原运行。');
      }
    }).catch(error => {
      if (!controller.signal.aborted) {
        if (error instanceof StreamHTTPError && (error.status === 401 || error.status === 403 || error.status === 404)) forget();
        else setRetry(request);
        setError(error instanceof Error ? error.message : '无法恢复上次运行。');
      }
    }).finally(() => { if (!controller.signal.aborted) setBusy(false); });
    return () => controller.abort();
  }, [info, slug]);

  async function send(submission?: Submission, cancel = false) {
    if (active.current || (!cancel && cooldown) || !info || (!submission && !text.trim())) return;
    const request = submission ?? { key: crypto.randomUUID(), message: text.trim(), conversationId: conversation.current, reply: messages.length + 1 };
    if (info.persistedRuns) remember(request);
    if (!submission) { setMessages(previous => [...previous, { role: 'user', text: request.message }, { role: 'assistant', text: '' }]); setText(''); }
    const controller = new AbortController(); active.current = controller;
    setBusy(true); setError(''); setRetry(undefined);
    let failed = false;
    const sessionHeaders: Record<string, string> = info.sessionId ? { 'X-Visitor-Session': info.sessionId } : {};
    const recover = async () => {
      const run = await publicRun(publicAgentURL(slug), request.runId!, info.sessionId, controller.signal, cancel);
      if (run.conversationId) conversation.current = run.conversationId;
      if (run.status === 'COMPLETED') {
        if (typeof run.output !== 'string') throw new Error('运行结果不完整。');
        const output = run.output;
        setMessages(previous => previous.map((m, i) => i === request.reply ? { ...m, text: output } : m));
        forget(); setRetry(undefined); setError('');
      } else if (run.status === 'FAILED' || run.status === 'CANCELLED') {
        forget(); setRetry(undefined); setText(request.message);
        setError(run.status === 'CANCELLED' ? '原运行已取消。可编辑消息重新发送，新发送将创建新的运行。' : '原运行已失败。可编辑消息重新发送，新发送将创建新的运行。');
      } else { setRetry(request); setCooldown(1); setError('原运行仍在执行，请稍后查询结果。'); }
    };
    try {
      if (submission?.runId && info.persistedRuns) { await recover(); return; }
      if (submission) setMessages(previous => previous.map((m, i) => i === request.reply ? { ...m, text: '' } : m));
      const response = await fetch(`${publicAgentURL(slug)}/chat/stream`, {
        method: 'POST', credentials: 'include', signal: controller.signal,
        headers: { 'Content-Type': 'application/json', 'Idempotency-Key': request.key, ...sessionHeaders },
        body: JSON.stringify({ message: request.message, conversationId: request.conversationId, visitorId: visitor.current }),
      });
      if (info.persistedRuns) request.runId = response.headers.get('X-Run-Id') ?? undefined;
      await consumePublicStream(response, event => {
        if (controller.signal.aborted) return;
        if (event.type === 'run.created' && event.conversationId) { conversation.current = event.conversationId; request.runId = event.runId; if (info.persistedRuns) remember(request); }
        if (event.type === 'message.delta' || event.type === 'message.completed') setMessages(previous => previous.map((m, i) => i === request.reply ? { ...m, text: event.type === 'message.delta' ? m.text + (event.content ?? '') : (event.content ?? m.text) } : m));
        if (event.type === 'run.failed') { failed = true; setError(event.error || '运行失败，请开始新会话后重试。'); }
      }, info.persistedRuns ? request.runId : undefined);
      if (failed) { forget(); setRetry(undefined); setText(request.message); }
    } catch (e) {
      if (!controller.signal.aborted) {
        if (e instanceof StreamHTTPError && e.runId) request.runId = e.runId;
        if (info.persistedRuns && request.runId && !submission?.runId && (!(e instanceof StreamHTTPError) || e.status === 409)) {
          try { await recover(); return; } catch (recoveryError) { e = recoveryError; }
        }
        if (controller.signal.aborted) {
          if (request.runId) setRetry(request);
          setError('已停止接收，运行结果尚未确认。可查询结果或取消原运行。');
          return;
        }
        setError(e instanceof Error ? e.message : '请求失败');
        if (info.persistedRuns && (!(e instanceof StreamHTTPError) || e.status === 429 || e.status >= 500 || (e.status === 409 && request.runId))) setRetry(request);
        if (e instanceof StreamHTTPError && e.status === 429) setCooldown(e.retryAfter);
      } else if (info.persistedRuns) {
        setRetry(request);
        setError(request.runId ? '已停止接收，运行结果尚未确认。可查询结果或取消原运行。' : '已停止接收，尚未取得运行编号。重试本次请求将复用原请求标识，避免重复执行。');
      }
    } finally {
      if (active.current === controller) { active.current = null; setBusy(false); }
    }
  }

  function reset() { forget(); conversation.current = undefined; visitor.current = crypto.randomUUID(); setMessages([]); setRetry(undefined); setText(''); setError(''); }
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
          <Button type="primary" htmlType="submit" disabled={!text.trim() || busy || !!retry || cooldown > 0}>发送</Button>
          {busy && <Button onClick={() => { active.current?.abort(); setError('已停止接收回答。'); }}>停止</Button>}
          {retry && <Button disabled={busy || cooldown > 0} onClick={() => void send(retry)}>{cooldown > 0 ? `${cooldown} 秒后重试` : retry.runId ? '查询运行结果' : '重试本次请求'}</Button>}
          {retry?.runId && info.persistedRuns && <Button disabled={busy} onClick={() => void send(retry, true)}>取消原运行</Button>}
          <Button disabled={busy} onClick={reset}>新会话</Button>
        </Space>
      </form>
    </>}
  </main>;
}
