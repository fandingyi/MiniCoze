import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import ts from 'typescript';
const compile = name => ts.transpileModule(readFileSync(new URL(`../src/api/${name}.ts`, import.meta.url), 'utf8'), { compilerOptions: { target: ts.ScriptTarget.ES2020, module: ts.ModuleKind.ESNext } }).outputText;
const url = source => `data:text/javascript;base64,${Buffer.from(source).toString('base64')}`;
const { publicRun } = await import(url(compile('public-run').replace("'./public-stream'", JSON.stringify(url(compile('public-stream'))))));
const result = data => new Response(JSON.stringify({ code: 0, data }));

test('query and cancellation preserve visitor scope and encode run identity', async t => {
  const requests = [];
  t.mock.method(globalThis, 'fetch', async (url, options) => { requests.push({url, ...options}); return result({id:'run/1',status:'CANCELLED'}); });
  for (const cancel of [false,true]) await publicRun('/public/agent', 'run/1', 'visitor-session', new AbortController().signal, cancel);
  assert.equal(requests[0].url, '/public/agent/runs/run%2F1');
  assert.equal(requests[0].method, 'GET');
  assert.equal(requests[1].url, '/public/agent/runs/run%2F1/cancel');
  assert.equal(requests[1].method, 'POST');
  assert.equal(requests[1].credentials, 'include');
  assert.equal(requests[1].headers['X-Visitor-Session'], 'visitor-session');
});

test('completion racing cancellation preserves completed answer', async t => {
  t.mock.method(globalThis, 'fetch', async () => result({id:'r',status:'COMPLETED',output:'saved answer'}));
  const run = await publicRun('/p','r','s',new AbortController().signal,true);
  assert.equal(run.status,'COMPLETED'); assert.equal(run.output,'saved answer');
});

test('invalid query identities, states and incomplete answers are rejected', async t => {
  for (const data of [{id:'other',status:'CANCELLED'},{id:'r',status:'UNKNOWN'},{id:'r',status:'COMPLETED'},{id:'r',status:'RUNNING',conversationId:42}]) {
    t.mock.method(globalThis,'fetch',async()=>result(data));
    await assert.rejects(publicRun('/p','r','s',new AbortController().signal),/无效状态/);
    t.mock.restoreAll();
  }
});

test('timeout aborts transport without claiming cancellation succeeded', async t => {
  let transport;
  t.mock.method(globalThis,'fetch',(_url,{signal})=>new Promise((_resolve,reject)=>{transport=signal;signal.addEventListener('abort',()=>reject(signal.reason),{once:true});}));
  await assert.rejects(publicRun('/p','r','s',new AbortController().signal,true,10),/结果尚未确认/);
  assert.equal(transport.aborted,true);
});

test('caller abort propagates and authentication failures remain non-retryable', async t => {
  t.mock.method(globalThis,'fetch',async(_url,{signal})=>{signal.throwIfAborted();return new Response('{}',{status:401});});
  const controller = new AbortController(); controller.abort();
  await assert.rejects(publicRun('/p','r','s',controller.signal),e=>e.name==='AbortError');
  await assert.rejects(publicRun('/p','r','s',new AbortController().signal),e=>e.status===401);
});
