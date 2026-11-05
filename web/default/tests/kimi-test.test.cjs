const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');
const { buildSync } = require('esbuild');
const entry = path.resolve(__dirname, '../src/pages/kimi-test-state.ts');
const build = buildSync({ entryPoints: [entry], bundle: true, platform: 'node', format: 'cjs', write: false });
const mod = { exports: {} };
new Function('module', 'exports', build.outputFiles[0].text)(mod, mod.exports);
const { readKimiEvents, finishKimiResults } = mod.exports;
const clientIdBuild = buildSync({ entryPoints: [path.resolve(__dirname, '../src/lib/client-id.ts')], bundle: true, platform: 'node', format: 'cjs', write: false });
const clientIdMod = { exports: {} };
new Function('module', 'exports', clientIdBuild.outputFiles[0].text)(clientIdMod, clientIdMod.exports);
test('chat UUID works on public HTTP without crypto.randomUUID', () => {
  const { createClientId } = clientIdMod.exports;
  assert.equal(createClientId({ getRandomValues: bytes => bytes.fill(255) }), 'ffffffff-ffff-4fff-bfff-ffffffffffff');
  assert.equal(createClientId({ randomUUID: () => 'native-uuid', getRandomValues: () => { throw Error('unexpected fallback'); } }), 'native-uuid');
});

const stream = (text) => new ReadableStream({ start(controller) { for (const byte of new TextEncoder().encode(text)) controller.enqueue(Uint8Array.of(byte)); controller.close(); } });
test('Kimi stream handles split UTF-8, CRLF, heartbeats and completion', async () => {
  const received = [];
  const events = [{ type: 'case_finished', time: '2026-11-05', message: '北京 · 通过' }, { type: 'completed', time: '2026-11-05', message: '完成' }];
  await readKimiEvents(stream(': heartbeat\r\n\r\n' + events.map(event => `data: ${JSON.stringify(event)}\r\n\r\n`).join('')), event => received.push(event));
  assert.deepEqual(received, events);
});
test('Kimi truncated stream is not marked completed', async () => {
  await assert.rejects(readKimiEvents(stream('data: {"type":"running","time":"now"}\n\n'), () => {}), /连接中断/);
  await assert.rejects(readKimiEvents(stream('data: {"type":"completed"'), () => {}), /连接中断/);
});
test('Kimi cancellation preserves finished results and marks every unfinished case', () => {
  const results = { a: { id: 'a', status: 'passed' }, b: { id: 'b', status: 'failed' }, c: { id: 'c', status: 'running' }, d: { id: 'd', status: 'queued' } };
  const cancelled = finishKimiResults(results, 'cancelled');
  assert.deepEqual(Object.values(cancelled).map(x => x.status), ['passed', 'failed', 'cancelled', 'cancelled']);
  assert.equal(results.c.status, 'running');
  assert.equal(finishKimiResults(results, 'interrupted').d.status, 'interrupted');
});
