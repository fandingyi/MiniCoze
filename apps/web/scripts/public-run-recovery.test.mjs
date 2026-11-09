import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import ts from 'typescript';

const source = readFileSync(new URL('../src/api/public-run-recovery.ts', import.meta.url), 'utf8');
const compiled = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2020, module: ts.ModuleKind.ESNext } }).outputText;
const { clearPendingPublicRun, publicRunStorageKey, readPendingPublicRun, writePendingPublicRun } = await import(`data:text/javascript;base64,${Buffer.from(compiled).toString('base64')}`);

function storage() {
  const values = new Map();
  return {
    getItem: key => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, value),
    removeItem: key => values.delete(key),
  };
}

const pending = { version: 1, key: 'retry-key-123', message: 'hello', reply: 1, conversationId: 'c1', runId: 'r1', createdAt: 1000 };

test('stores active recovery metadata by publication slug and clears it only on terminal cleanup', () => {
  const session = storage();
  writePendingPublicRun(session, 'a', pending);
  assert.deepEqual(readPendingPublicRun(session, 'a', 2000), pending);
  assert.equal(readPendingPublicRun(session, 'b', 2000), undefined);
  clearPendingPublicRun(session, 'a');
  assert.equal(session.getItem(publicRunStorageKey('a')), null);
});

test('rejects stale, malformed and cross-shape recovery entries', () => {
  const session = storage();
  session.setItem(publicRunStorageKey('a'), JSON.stringify({ ...pending, createdAt: 1000 }));
  assert.equal(readPendingPublicRun(session, 'a', 1000 + 10 * 60 * 1000 + 1), undefined);
  assert.equal(session.getItem(publicRunStorageKey('a')), null);
  for (const value of [
    { ...pending, version: 2 },
    { ...pending, key: 'bad key' },
    { ...pending, message: '' },
    { ...pending, reply: 0 },
    { ...pending, runId: 4 },
  ]) {
    session.setItem(publicRunStorageKey('a'), JSON.stringify(value));
    assert.equal(readPendingPublicRun(session, 'a', 2000), undefined);
    assert.equal(session.getItem(publicRunStorageKey('a')), null);
  }
});

test('does not retain invalid JSON if browser storage was partially written', () => {
  const session = storage();
  session.setItem(publicRunStorageKey('a'), '{');
  assert.equal(readPendingPublicRun(session, 'a', 2000), undefined);
  assert.equal(session.getItem(publicRunStorageKey('a')), null);
});
