const test = require('node:test');
const assert = require('node:assert/strict');
const { spawnSync } = require('node:child_process');
const { existsSync } = require('node:fs');
const path = require('node:path');
const { buildClientConfigSnippets } = require('./tokens.js');

function parseToml(value) {
  // Python's standard TOML parser avoids adding a frontend runtime dependency.
  for (const command of (process.platform === 'win32' ? ['python', 'python3'] : ['python3', 'python3.14', 'python3.13', 'python3.12', 'python3.11'])) {
    const result = spawnSync(command, ['-c', 'import json, sys, tomllib; print(json.dumps(tomllib.loads(sys.stdin.read())))'], {
      input: value, encoding: 'utf8'
    });
    if (result.error?.code === 'ENOENT' || result.stderr?.includes("No module named 'tomllib'")) continue;
    assert.equal(result.status, 0, result.stderr);
    return JSON.parse(result.stdout);
  }
  assert.fail('Python 3.11+ is required to parse the generated TOML');
}

test('client configuration preserves gateway URLs and token values', () => {
  const token = 'sk-test';
  const snippets = buildClientConfigSnippets('https://gateway.example:8443/', token);
  assert.deepEqual(JSON.parse(snippets.claudeCode), { env: {
    ANTHROPIC_BASE_URL: 'https://gateway.example:8443', ANTHROPIC_AUTH_TOKEN: token
  } });
  const config = parseToml(snippets.codexToml);
  assert.equal(config.model_provider, 'ccload');
  assert.deepEqual(config.model_providers.ccload, {
    name: 'OpenAI', base_url: 'https://gateway.example:8443/v1', wire_api: 'responses',
    requires_openai_auth: true, supports_websockets: true
  });
});

test('JSON, TOML and shell escaping preserve special characters without executing token content', () => {
  const origin = 'https://gateway.example/"\\\t\n\x7f';
  const token = "sk-'\"\\\n$HOME `printf unsafe` $(printf unsafe)";
  const snippets = buildClientConfigSnippets(origin, token);
  assert.equal(JSON.parse(snippets.claudeCode).env.ANTHROPIC_AUTH_TOKEN, token);
  assert.equal(JSON.parse(snippets.claudeCode).env.ANTHROPIC_BASE_URL, origin);
  assert.equal(parseToml(snippets.codexToml).model_providers.ccload.base_url, origin + '/v1');
  // A shell function stands in for Codex and returns exactly the login command's stdin.
  const candidates = process.platform === 'win32'
    ? ['sh', path.join(process.env.ProgramFiles || 'C:/Program Files', 'Git/bin/sh.exe'), path.join(process.env.LOCALAPPDATA || '', 'Programs/Git/bin/sh.exe')]
    : ['/bin/sh'];
  const shell = candidates.find(command => command.includes('/') || command.includes('\\') ? existsSync(command) : !spawnSync(command, ['-c', 'exit 0']).error);
  assert.ok(shell, 'A POSIX shell (Git for Windows on Windows) is required to verify login command escaping');
  const result = spawnSync(shell, ['-c', 'codex() { cat; }; ' + snippets.codexLogin], { encoding: 'utf8' });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout, token + '\n');
});
