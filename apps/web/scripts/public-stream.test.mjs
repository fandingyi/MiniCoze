import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import ts from 'typescript';
const compiled = ts.transpileModule(readFileSync(new URL('../src/api/public-stream.ts', import.meta.url), 'utf8'), { compilerOptions: { target: ts.ScriptTarget.ES2020, module: ts.ModuleKind.ESNext } }).outputText;
const { consumePublicStream, StreamHTTPError } = await import(`data:text/javascript;base64,${Buffer.from(compiled).toString('base64')}`);
const frame = value => `data: ${JSON.stringify({runId:"test-run",...(value.type === "run.created" ? {conversationId:"conversation"} : {}),...value})}\r\n\r\n`;
function response(text, step = 7) {
  const bytes = new TextEncoder().encode(text);
  return new Response(new ReadableStream({ start(controller) { for(let i=0;i<bytes.length;i+=step) controller.enqueue(bytes.slice(i,i+step)); controller.close(); } }), { headers: { 'content-type': 'text/event-stream' } });
}
test('fragmented UTF-8 and CRLF preserve content and require terminal/done', async () => {
  const events = [];
  await consumePublicStream(response(frame({type:'run.created'})+frame({type:'message.delta',content:'你好🌏\n回答'})+frame({type:'run.completed'})+frame({type:'stream.done'}),1), event => events.push(event));
  assert.equal(events[1].content,'你好🌏\n回答'); assert.equal(events.length,4);
});
test('transport EOF never pretends to be successful completion', async () => {
  await assert.rejects(consumePublicStream(response(frame({type:'run.created'})+frame({type:'run.completed'})),()=>{}),/连接中断/);
  await assert.rejects(consumePublicStream(response(frame({type:'run.created'})+frame({type:'stream.done'})),()=>{}),/最终状态/);
});
test('failed runs can close normally without becoming successes', async () => {
  const events=[];
  await consumePublicStream(response(frame({type:'run.failed',error:'cancelled'})+frame({type:'stream.done'})),e=>events.push(e.type));
  assert.deepEqual(events,['run.failed','stream.done']);
});
test('invalid JSON, HTTP failure, HTML and oversized frames are rejected', async () => {
  await assert.rejects(consumePublicStream(response('data: nope\n\n'),()=>{}));
  await assert.rejects(consumePublicStream(new Response('',{status:403}),()=>{}),e=>e instanceof StreamHTTPError && e.status===403);
  await assert.rejects(consumePublicStream(new Response('<html>'),()=>{}),/无效/);
  await assert.rejects(consumePublicStream(response('data: '+ 'x'.repeat(2*1024*1024+1),65536),()=>{}),/过大/);
});

test('rejects mixed runs, duplicate terminal, late delta and missing content', async () => {
  for (const middle of [
    frame({type:'message.delta',runId:'other',content:'bad'}),
    frame({type:'run.completed'})+frame({type:'run.failed',error:'bad'}),
    frame({type:'run.completed'})+frame({type:'message.delta',content:'late'}),
    frame({type:'message.delta'}),
    frame({type:'run.created'}),
  ]) await assert.rejects(consumePublicStream(response(frame({type:'run.created'})+middle+frame({type:'stream.done'})),()=>{}));
});
test('409 preserves run identity, 429 preserves bounded retry delay', async () => {
  await assert.rejects(consumePublicStream(new Response(JSON.stringify({data:{id:'existing'}}),{status:409}),()=>{}),e=>e instanceof StreamHTTPError && e.status===409 && e.runId==='existing');
  await assert.rejects(consumePublicStream(new Response('{}',{status:429,headers:{'Retry-After':'3'}}),()=>{}),e=>e.retryAfter===3);
});
