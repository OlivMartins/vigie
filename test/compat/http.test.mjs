// Vigie's HTTP gateway between the official MCP TypeScript SDKs over
// Streamable HTTP: clients v1 and v2, servers v1 (2025 sessions, SSE, GET
// stream) and v2 (2026-07-28 with its stateless fallback). Run with
// "make compat" (builds bin/vigie first).
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdtempSync, readFileSync, writeFileSync, existsSync } from 'node:fs';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { join, dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { Client as ClientV1 } from 'sdk-v1/client/index.js';
import { StreamableHTTPClientTransport as HttpV1 } from 'sdk-v1/client/streamableHttp.js';
import { Client as ClientV2, StreamableHTTPClientTransport as HttpV2 } from '@modelcontextprotocol/client';

const here = dirname(fileURLToPath(import.meta.url));
const vigie = process.env.VIGIE_BIN ?? resolve(here, '../../bin/vigie');
assert.ok(existsSync(vigie), `${vigie} not found: run "make build" first`);

// A key assembled at run time: the repository holds no literal credential.
const awsKey = 'AKIA' + 'Z7QJ4XWVN2L5KT3M';
const token = 'compat-' + 'f00d'.repeat(8);

const children = [];
let gatewayURL;
let traces;

function start(command, args, env = {}) {
  const child = spawn(command, args, { stdio: ['ignore', 'pipe', 'inherit'], env: { ...process.env, ...env } });
  children.push(child);
  return child;
}

async function upstream(flavor) {
  const child = start(process.execPath, [join(here, 'http-server.mjs'), flavor]);
  const [data] = await new Promise((ok) => child.stdout.once('data', (...a) => ok(a)));
  return `http://127.0.0.1:${String(data).trim()}/mcp`;
}

function freePort() {
  return new Promise((ok) => {
    const s = createServer();
    s.listen(0, '127.0.0.1', () => { const { port } = s.address(); s.close(() => ok(port)); });
  });
}

before(async () => {
  const dir = mkdtempSync(join(tmpdir(), 'vigie-compat-http-'));
  const [v1, v2, port] = await Promise.all([upstream('v1'), upstream('v2'), freePort()]);
  const config = join(dir, 'vigie.yaml');
  traces = join(dir, 'traces.jsonl');
  writeFileSync(config, [
    'version: 1',
    `pins_dir: ${join(dir, 'pins')}`,
    'gateway:',
    '  tokens:',
    '    compat: ${VIGIE_COMPAT_TOKEN}',
    'defaults:',
    '  tools:',
    '    deny: ["drop_*"]',
    'servers:',
    `  v1:\n    url: ${v1}`,
    `  v2:\n    url: ${v2}`,
    '',
  ].join('\n'));
  start(vigie, ['gateway', '--config', config, '--listen', `127.0.0.1:${port}`, '--trace-file', traces, '--log-level', 'error'],
    { VIGIE_COMPAT_TOKEN: token, OTEL_EXPORTER_OTLP_ENDPOINT: '' });
  gatewayURL = `http://127.0.0.1:${port}`;
  for (let i = 0; ; i++) {
    try {
      if ((await fetch(`${gatewayURL}/healthz`)).ok) break;
    } catch (err) {
      if (i > 100) throw err;
    }
    await new Promise((ok) => setTimeout(ok, 50));
  }
});

after(() => {
  for (const child of children) child.kill();
});

function spans() {
  return readFileSync(traces, 'utf8').trim().split('\n').map((line) => {
    const span = JSON.parse(line);
    span.attrs = Object.fromEntries((span.Attributes ?? []).map((kv) => [kv.Key, kv.Value.Value]));
    return span;
  });
}

const auth = { headers: { Authorization: `Bearer ${token}` } };
const clients = {
  v1: (url) => ({ client: new ClientV1({ name: 'compat-http-client-v1', version: '1.0.0' }), transport: new HttpV1(url, { requestInit: auth }) }),
  v2: (url) => ({
    client: new ClientV2({ name: 'compat-http-client-v2', version: '2.0.0' }, { versionNegotiation: { mode: 'auto' } }),
    transport: new HttpV2(url, { requestInit: auth }),
  }),
};

for (const clientName of ['v1', 'v2']) {
  for (const serverName of ['v1', 'v2']) {
    const modern = clientName === 'v2' && serverName === 'v2';
    test(`HTTP: client ${clientName} ↔ vigie gateway ↔ server ${serverName} (${modern ? '2026-07-28' : '2025-11-25'})`, async () => {
      const { client, transport } = clients[clientName](new URL(`${gatewayURL}/mcp/${serverName}`));
      await client.connect(transport);
      try {
        if (clientName === 'v2') {
          assert.equal(client.getProtocolEra(), modern ? 'modern' : 'legacy');
        }
        const { tools } = await client.listTools();
        assert.deepEqual(tools.map((t) => t.name).sort(), ['echo'], 'drop_table is denied by the policy');

        const echoed = await client.callTool({ name: 'echo', arguments: { text: 'bonjour <Vigie> & co' } });
        assert.equal(echoed.isError ?? false, false);
        assert.equal(echoed.content[0].text, 'bonjour <Vigie> & co');

        const blocked = await client.callTool({ name: 'echo', arguments: { text: `key ${awsKey}` } });
        assert.equal(blocked.isError, true);
        assert.match(blocked.content[0].text, /Vigie blocked this call to tool "echo": arguments\.text looks like an AWS access key/);

        const denied = await client.callTool({ name: 'drop_table', arguments: { name: 'invoices' } });
        assert.equal(denied.isError, true);
      } finally {
        await client.close();
      }

      const call = spans().filter((s) => s.Name === 'tools/call echo' && s.SpanKind === 3 && s.attrs['vigie.upstream.name'] === serverName).at(-1);
      assert.ok(call, 'a CLIENT span for the relayed call');
      assert.equal(call.attrs['network.transport'], 'tcp');
      assert.equal(call.attrs['network.protocol.name'], 'http');
      assert.equal(call.attrs['server.address'], '127.0.0.1');
      assert.equal(call.attrs['mcp.protocol.version'], modern ? '2026-07-28' : '2025-11-25');
      const received = spans().filter((s) => s.Name === 'tools/call echo' && s.SpanKind === 2 && s.attrs['vigie.upstream.name'] === serverName).at(-1);
      assert.equal(received.attrs['vigie.client.name'], 'compat');
      if (serverName === 'v1') {
        assert.match(received.attrs['mcp.session.id'], /^[0-9a-f]{32}$/, 'a hash of the session id');
      }
      assert.ok(!readFileSync(traces, 'utf8').includes(awsKey), 'the key is in no span');
    });
  }
}

test('HTTP: a request without the gateway token is refused', async () => {
  const response = await fetch(`${gatewayURL}/mcp/v2`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', accept: 'application/json, text/event-stream' },
    body: JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/list' }),
  });
  assert.equal(response.status, 401);
});
