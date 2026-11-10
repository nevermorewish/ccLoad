const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');
const { buildSync } = require('esbuild');
const result = buildSync({ entryPoints: [path.resolve(__dirname, '../src/lib/log-status.ts')], bundle: true, platform: 'node', format: 'cjs', write: false });
const mod = { exports: {} };
new Function('module', 'exports', result.outputFiles[0].text)(mod, mod.exports);
const { logStatusOptions, RATE_LIMIT_LABELS } = mod.exports;

test('429 categories remain selectable with no recent errors and use distinct query values', () => {
  const options = logStatusOptions([200, 200, 503]);
  assert.equal(options.filter(option => option.value === '200').length, 1);
  assert.ok(options.some(option => option.value === '429'));
  for (const [category, label] of Object.entries(RATE_LIMIT_LABELS)) {
    assert.deepEqual(options.find(option => option.value === `429:${category}`), { value: `429:${category}`, label: `429 · ${label}` });
  }
  assert.equal(new Set(options.map(option => option.value)).size, options.length);
});
