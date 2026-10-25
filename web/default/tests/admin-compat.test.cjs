const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');
const { createRequire } = require('node:module');
const { buildSync } = require('esbuild');

// Exercise the actual React page's serializers, including their imported helpers.
function loadPage(relative) {
  const entry = path.resolve(__dirname, '../src', relative);
  const result = buildSync({ entryPoints: [entry], bundle: true, platform: 'node', format: 'cjs', jsx: 'automatic', packages: 'external', write: false });
  const mod = { exports: {} };
  new Function('require', 'module', 'exports', result.outputFiles[0].text)(createRequire(entry), mod, mod.exports);
  return mod.exports;
}
const api = loadPage('pages/channels/api.ts');
const { advancedFromChannel, advancedToPayload } = loadPage('pages/channels/advanced-settings-dialog.tsx');

test('channel force-upstream-stream switch persists without body rules and can be disabled', () => {
  const initial = advancedFromChannel({ custom_request_rules: { force_upstream_stream: true } });
  assert.equal(initial.force_upstream_stream, true);
  const payload = advancedToPayload(initial, false);
  assert.equal(payload.custom_request_rules.force_upstream_stream, true);
  assert.equal(advancedToPayload({ ...initial, force_upstream_stream: false }, false).custom_request_rules, null);
  const old = advancedFromChannel({});
  assert.equal(old.force_upstream_stream, false);
  assert.equal(advancedToPayload(old, false).custom_request_rules, null);
});
const tokens = loadPage('pages/tokens-page.tsx');
const reasoning = loadPage('pages/reasoning-settings-dialog.tsx');
const { debugLogUnavailableHint } = loadPage('lib/debug-log.ts');
const { DebugLogDialog } = loadPage('pages/logs-page.tsx');

test('debug unavailable settings use SystemSetting values rather than object truthiness', () => {
  const hint = debugLogUnavailableHint({
    debug_log_enabled: { key: 'debug_log_enabled', value: 'false' },
    debug_log_retention_minutes: { key: 'debug_log_retention_minutes', value: '2' },
  });
  assert.match(hint, /未开启，保留 2 分钟/);
  assert.doesNotMatch(hint, /已开启|\[object Object\]/);
  assert.match(hint, /无法补回/);
  assert.match(debugLogUnavailableHint({ debug_log_enabled: { value: 'true' }, debug_log_retention_minutes: { value: '15' } }), /已开启，保留 15 分钟/);
  for (const detail of [undefined, {}, { debug_log_enabled: {}, debug_log_retention_minutes: {} }]) {
    assert.match(debugLogUnavailableHint(detail), /无法读取/);
    assert.doesNotMatch(debugLogUnavailableHint(detail), /已开启|未开启|\[object Object\]|NaN/);
  }
});

test('log details display the complete persisted error before debug data loads', () => {
  const pageRequire = createRequire(path.resolve(__dirname, '../src/pages/logs-page.tsx'));
  const React = pageRequire('react');
  const { renderToStaticMarkup } = pageRequire('react-dom/server');
  const message = 'upstream status 400: ' + 'x'.repeat(200) + ' ValidationException: invalid beta flag';
  const html = renderToStaticMarkup(React.createElement(DebugLogDialog, { target: { logId: 79915, message }, onClose() {} }));
  assert.ok(html.includes(message));
  assert.match(html, /完整日志信息/);
  assert.match(html, /复制日志信息/);
});
const routing = loadPage('pages/route-preview-dialog.tsx');

test('System One edit and duplicate retain every key and its scope, priority and multiplier', () => {
  const channel = { id: 1, name: 'Decision', auth_type: 'system_one', urls: [{ url: 'https://example.invalid' }], models: [{ model: 'jev-latest' }], same_channel_retries: 3 };
  const keys = [
    { api_key: 'fixture-a', cost_multiplier: 1, priority: 20, allowed_models: ['jev-latest'], note: 'first' },
    { api_key: 'fixture-b', cost_multiplier: 2, priority: 10, allowed_models: [], model_scope_empty: true, note: 'second' },
  ];
  const form = api.formFromEditor({ channel, keys }, channel);
  const extra = advancedToPayload(advancedFromChannel(channel), form.auth_type !== 'api_key');
  const payload = api.formToPayload({ ...form, ...extra }, { customRules: extra.custom_request_rules, cooldownRules: extra.cooldown_detection_rules, management: extra.management_account });
  assert.equal(api.usesAPIKeyCredentials(form.auth_type), true);
  assert.deepEqual(payload.api_keys, keys);
  assert.equal(payload.same_channel_retries, 3);
  assert.equal(payload.auth_type, 'system_one');
  assert.equal('management_account' in payload, false);
  assert.equal(payload.websockets, false);
  assert.equal(payload.scheduled_check_enabled, false);
  assert.ok(payload.urls.every((url) => !url.protocols));
});

test('API key edits retain retry settings and explicit zero; OAuth keeps synthetic-key semantics', () => {
  const channel = { id: 1, name: 'Regular', auth_type: 'api_key', urls: [{ url: 'https://example.invalid', protocols: ['openai'] }], models: [{ model: 'gpt-test' }], same_channel_retries: 3 };
  const form = api.formFromEditor({ channel, keys: [{ api_key: 'fixture', priority: 9 }] }, channel);
  const extras = { customRules: null, cooldownRules: null, management: null };
  assert.equal(api.formToPayload(form, extras).same_channel_retries, 3);
  assert.equal(api.formToPayload({ ...form, same_channel_retries: 0 }, extras).same_channel_retries, 0);
  assert.deepEqual(api.formToPayload(form, extras).urls, channel.urls);
  const oauth = api.formToPayload({ ...form, auth_type: 'codex_oauth' }, extras);
  assert.equal(api.usesAPIKeyCredentials('codex_oauth'), false);
  assert.deepEqual(oauth.api_keys, [{ api_key: 'fixture', cost_multiplier: undefined }]);
  assert.equal('management_account' in oauth, false);
  assert.equal('key_strategy' in oauth, false);
});

test('Token RPM payload preserves configured limits and distinguishes disabling from invalid input', () => {
  for (const rpm of ['120', '0', '']) {
    const result = tokens.buildPayload({ ...tokens.emptyDraft(), description: 'test', rpm }, false);
    assert.equal(result.error, undefined);
    assert.equal(result.body.rpm_limit, Number(rpm));
    assert.equal('expires_at' in result.body, false);
  }
  for (const rpm of ['-1', '1.5', 'Infinity', 'NaN', '9007199254740992']) {
    assert.ok(tokens.buildPayload({ ...tokens.emptyDraft(), description: 'test', rpm }, false).error);
  }
});

test('reasoning settings round-trip enabled config and reject invalid model or timeout values', () => {
  const config = { enabled: true, decision_channel_id: 2, decision_model: 'jev-latest', business_models: ['gpt-6-astra'], timeout_ms: 2000 };
  assert.deepEqual(reasoning.parseReasoningConfig(JSON.stringify(config)), config);
  assert.deepEqual(reasoning.parseReasoningConfig('{}'), reasoning.reasoningDefaults);
  for (const patch of [{ timeout_ms: 99 }, { timeout_ms: 10001 }, { decision_channel_id: 0 }, { business_models: [] }, { business_models: ['gpt*'] }, { business_models: ['gpt(high)'] }, { business_models: ['gpt', ' gpt '] }, { enabled: null }]) {
    assert.throws(() => reasoning.parseReasoningConfig(JSON.stringify({ ...config, ...patch })));
  }
});

test('route preview sends optional token ID, protocol, model and multimodal flag without channel selection', () => {
  assert.deepEqual(routing.buildRoutePreviewPayload(' gpt-test ', 'codex', '', false), { model: 'gpt-test', client_protocol: 'codex', multimodal: false });
  assert.deepEqual(routing.buildRoutePreviewPayload('gpt-test', 'anthropic', '7', true), { model: 'gpt-test', client_protocol: 'anthropic', multimodal: true, auth_token_id: 7 });
  assert.throws(() => routing.buildRoutePreviewPayload('', 'codex', '', false));
  assert.throws(() => routing.buildRoutePreviewPayload('gpt-test', 'system_one', '', false));
  assert.throws(() => routing.buildRoutePreviewPayload('gpt-test', 'codex', '-1', false));
});

test('System One manual tests never send a chat protocol or request streaming', () => {
  const body = { model: 'jev-latest', content: 'hello', client_protocol: 'anthropic', stream: true, key_index: 1 };
  assert.deepEqual(api.channelTestPayload('system_one', body), { model: 'jev-latest', content: 'hello', stream: false, key_index: 1 });
  assert.deepEqual(api.channelTestPayload('api_key', body), body);
  assert.deepEqual(api.channelTestPayload('codex_oauth', body), body);
});


test('OpenCode saves omit unsupported management accounts and retain automatic model scopes', () => {
  const extras = { customRules: null, cooldownRules: null, management: { profile: '' } };
  for (const url of ['https://opencode.ai/zen', 'https://opencode.ai/zen/go', 'https://opencode.ai/inference']) {
    const form = { ...api.emptyForm(), urls: [{ url }], api_keys: [{ api_key: 'public', model_scope_auto: true, detected_models: ['gpt-test'] }] };
    const payload = api.formToPayload(form, extras);
    assert.equal('management_account' in payload, false);
    assert.equal(payload.api_keys[0].model_scope_auto, true);
    assert.deepEqual(payload.api_keys[0].detected_models, ['gpt-test']);
  }
  assert.deepEqual(api.formToPayload({ ...api.emptyForm(), urls: [{ url: 'https://opencode.ai.example/zen' }] }, extras).management_account, { profile: '' });
});
