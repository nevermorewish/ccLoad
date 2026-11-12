const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');
const { createRequire } = require('node:module');
const { buildSync } = require('esbuild');

function loadPage(relative) {
  const entry = path.resolve(__dirname, '../src', relative);
  const result = buildSync({ entryPoints: [entry], bundle: true, platform: 'node', format: 'cjs', jsx: 'automatic', packages: 'external', write: false });
  const mod = { exports: {} };
  new Function('require', 'module', 'exports', result.outputFiles[0].text)(createRequire(entry), mod, mod.exports);
  return mod.exports;
}

const routes = loadPage('pages/channel-routes.ts');
const api = loadPage('pages/channels/api.ts');

test('token inputs accept plain numbers, separators and k/m suffixes', () => {
  assert.equal(routes.parseTokenInput(''), 0);
  assert.equal(routes.parseTokenInput(' 8000 '), 8000);
  assert.equal(routes.parseTokenInput('8,000'), 8000);
  assert.equal(routes.parseTokenInput('8k'), 8000);
  assert.equal(routes.parseTokenInput('8.5K'), 8500);
  assert.equal(routes.parseTokenInput('1.5m'), 1500000);
  assert.equal(routes.parseTokenInput('abc'), null);
  assert.equal(routes.parseTokenInput('-1'), null);
  assert.equal(routes.formatTokens(8000), '8k');
  assert.equal(routes.formatTokens(8500), '8.5k');
  assert.equal(routes.formatTokens(1500000), '1.5m');
  assert.equal(routes.formatTokens(1234), '1234');
  assert.equal(routes.formatTokens(0), '');
});

test('the kimi example serializes to the backend contract and round-trips', () => {
  const { config, error } = routes.draftsToConfig([routes.exampleRuleDraft()]);
  assert.equal(error, undefined);
  assert.deepEqual(config.rules[0], {
    id: '', name: 'kimi-k3 分流', enabled: true, models: ['kimi-k3'], default_pool: 0,
    token_tiers: [{ min_tokens: 0, max_tokens: 8000, pool: 1 }, { min_tokens: 50000, max_tokens: 0, pool: 2 }],
    overflow: [{ pool: 1, max_queue: 100, target_pool: 2 }], sticky: 'tier', strict: false, first_byte_timeout_seconds: 0,
  });
  const saved = { ...config.rules[0], id: 'abc123' };
  const draft = routes.ruleToDraft(saved);
  assert.deepEqual(draft.tiers, [{ min: '', max: '8k', pool: 1 }, { min: '50k', max: '', pool: 2 }]);
  assert.deepEqual(routes.draftsToConfig([draft]).config.rules[0], saved);
  assert.equal(routes.summarizeRule(saved), '< 8k → 渠道1；≥ 50k → 渠道2；其余 → 主渠道；渠道1 进行中 ≥ 100 时优先 渠道2');
});

test('draft validation mirrors the backend rules', () => {
  const base = routes.exampleRuleDraft();
  const cases = [
    [{ models: [] }, /至少填写一个模型/],
    [{ tiers: [{ min: '', max: '', pool: 1 }] }, /至少填写下限或上限/],
    [{ tiers: [{ min: '9k', max: '8k', pool: 1 }] }, /上限必须大于下限/],
    [{ tiers: [{ min: 'x', max: '', pool: 1 }] }, /格式无效/],
    [{ overflow: [{ pool: 1, maxQueue: '0', target: 2 }] }, /正整数/],
    [{ overflow: [{ pool: 1, maxQueue: '10', target: 1 }] }, /不能与来源相同/],
    [{ overflow: [{ pool: 1, maxQueue: '10', target: 2 }, { pool: 1, maxQueue: '20', target: 0 }] }, /多条队列溢出/],
    [{ firstByteTimeout: '1.5' }, /首字超时/],
    [{ firstByteTimeout: '-1' }, /首字超时/],
    [{ firstByteTimeout: '3601' }, /首字超时/],
  ];
  for (const [patch, pattern] of cases) {
    const { config, error } = routes.draftsToConfig([{ ...base, ...patch }]);
    assert.equal(config, undefined);
    assert.match(error, pattern);
  }
  assert.deepEqual(routes.addModels(['kimi-k3'], 'KIMI-K3, kimi-*，glm-5'), ['kimi-k3', 'kimi-*', 'glm-5']);
  assert.equal(routes.inflightFor([{ rule_id: 'r', pool: 1, inflight: 7 }], 'r', 1), 7);
  assert.equal(routes.inflightFor([], 'r', 1), 0);
  const pools = [{ pool: 0, inflight: 9 }, { pool: 1, inflight: 9 }];
  assert.deepEqual(routes.withPoolInflight(pools, [{ rule_id: '', pool: 0, inflight: 2 }, { rule_id: 'r', pool: 0, inflight: 3 }]).map((pool) => pool.inflight), [5, 0]);
});

test('first byte timeout round-trips and shows in the summary', () => {
  assert.equal(routes.parseSecondsInput(''), 0);
  assert.equal(routes.parseSecondsInput(' 20 '), 20);
  assert.equal(routes.parseSecondsInput('20s'), null);
  const draft = { ...routes.emptyRuleDraft(), models: ['kimi-k3.1-pro'], firstByteTimeout: '20' };
  const rule = routes.draftsToConfig([draft]).config.rules[0];
  assert.equal(rule.first_byte_timeout_seconds, 20);
  assert.equal(routes.summarizeRule(rule), '全部 → 主渠道；首字超过 20 秒切换渠道');
  assert.equal(routes.ruleToDraft(rule).firstByteTimeout, '20');
  assert.equal(routes.ruleToDraft({ ...rule, first_byte_timeout_seconds: 0 }).firstByteTimeout, '');
  assert.equal(routes.ruleToDraft({ ...rule, first_byte_timeout_seconds: undefined }).firstByteTimeout, '');
});

test('channel editor keeps the channel pool through load and save', () => {
  const channel = { id: 3, name: 'pool-two', auth_type: 'api_key', urls: [{ url: 'https://example.invalid' }], models: [{ model: 'kimi-k3' }], channel_pool: 2 };
  const form = api.formFromEditor({ channel, keys: [{ api_key: 'sk-pool' }] }, channel);
  assert.equal(form.channel_pool, 2);
  const payload = api.formToPayload(form, { customRules: null, cooldownRules: null, management: null });
  assert.equal(payload.channel_pool, 2);
  assert.equal(api.formFromEditor({ channel: { ...channel, channel_pool: undefined } }, channel).channel_pool, 0);
  assert.equal(api.emptyForm().channel_pool, 0);
  assert.deepEqual(api.CHANNEL_POOLS.map((pool) => [pool.value, pool.label, pool.path]), [[0, '主渠道', '/channels'], [1, '渠道1', '/channels-1'], [2, '渠道2', '/channels-2']]);
  assert.equal(api.channelPoolLabel(undefined), '主渠道');
  assert.equal(api.channelPoolLabel('1'), '渠道1');
});


test('explicit timeout destination round-trips and requires a timeout', () => {
  const draft = { ...routes.emptyRuleDraft(), models: ['kimi-k3.1-pro'], firstByteTimeout: '20', firstByteTimeoutChannel: '40' };
  const rule = routes.draftsToConfig([draft]).config.rules[0];
  assert.equal(rule.first_byte_timeout_channel_id, 40);
  assert.equal(routes.ruleToDraft(rule).firstByteTimeoutChannel, '40');
  assert.match(routes.summarizeRule(rule), /渠道 #40，返回最终结果/);
  assert.match(routes.draftsToConfig([{ ...draft, firstByteTimeout: '' }]).error, /指定超时渠道/);
});


test('first byte fail fast returns 503 without a destination channel', () => {
  const draft = { ...routes.emptyRuleDraft(), models: ['kimi-k3.1-pro'], firstByteTimeout: '20', firstByteTimeoutFailFast: true };
  const rule = routes.draftsToConfig([draft]).config.rules[0];
  assert.equal(rule.first_byte_timeout_seconds, 20);
  assert.equal(rule.first_byte_timeout_fail_fast, true);
  assert.equal(rule.first_byte_timeout_channel_id, undefined);
  assert.equal(routes.ruleToDraft(rule).firstByteTimeoutFailFast, true);
  assert.match(routes.summarizeRule(rule), /首字超过 20 秒直接返回 503/);
  assert.match(routes.draftsToConfig([{ ...draft, firstByteTimeout: '' }]).error, /503/);
  assert.match(routes.draftsToConfig([{ ...draft, firstByteTimeoutChannel: '40' }]).error, /不能同时设置/);
  assert.equal(routes.draftsToConfig([{ ...routes.emptyRuleDraft(), models: ['m'] }]).config.rules[0].first_byte_timeout_fail_fast, undefined);
});

test('non-stream destination round-trips independently of first byte timeout', () => {
  const draft = { ...routes.emptyRuleDraft(), models: ['kimi-k3.1-pro'], nonStreamChannel: '44' };
  const rule = routes.draftsToConfig([draft]).config.rules[0];
  assert.equal(rule.non_stream_channel_id, 44);
  assert.equal(rule.first_byte_timeout_seconds, 0);
  assert.equal(routes.ruleToDraft(rule).nonStreamChannel, '44');
  assert.match(routes.summarizeRule(rule), /非流式请求直接使用渠道 #44/);
  assert.equal(routes.ruleToDraft({ ...rule, non_stream_channel_id: undefined }).nonStreamChannel, '');
  assert.equal(routes.draftsToConfig([{ ...draft, nonStreamChannel: '' }]).config.rules[0].non_stream_channel_id, undefined);
  assert.match(routes.draftsToConfig([{ ...draft, nonStreamChannel: '-1' }]).error, /非流式/);
});
