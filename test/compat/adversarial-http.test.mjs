// A malicious server over Streamable HTTP tries to get a changed tool
// description, or a denied tool, past Vigie's gateway to the official
// TypeScript SDK clients.
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdtempSync, writeFileSync, existsSync } from 'node:fs';
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

const modes = ['string-id', 'float-id', 'deep', 'nameless', 'ambiguous', 'duplicate', 'sse-string-id', 'content-type', 'error-status', 'get-stream'];
const clients = { v1: [ClientV1, HttpV1], v2: [ClientV2, HttpV2] };
const children = [];
let gatewayURL;

function freePort() {
  return new Promise((ok) => {
    const s = createServer();
    s.listen(0, '127.0.0.1', () => { const { port } = s.address(); s.close(() => ok(port)); });
  });
}

before(async () => {
  const evil = spawn(process.execPath, [join(here, 'evil-http-server.mjs')], { stdio: ['ignore', 'pipe', 'inherit'] });
  children.push(evil);
  const [data] = await new Promise((ok) => evil.stdout.once('data', (...a) => ok(a)));
  const upstream = `http://127.0.0.1:${String(data).trim()}/mcp`;
  const dir = mkdtempSync(join(tmpdir(), 'vigie-adversarial-http-'));
  const config = join(dir, 'vigie.yaml');
  const servers = modes.flatMap((mode) => Object.keys(clients).map((c) => `  ${mode}-${c}:\n    url: ${upstream}/${mode}/${c}`));
  writeFileSync(config, `version: 1\npins_dir: ${join(dir, 'pins')}\ndefaults:\n  tools:\n    deny: ["drop_*"]\nservers:\n${servers.join('\n')}\n`);
  const port = await freePort();
  const gateway = spawn(vigie, ['gateway', '--config', config, '--listen', `127.0.0.1:${port}`, '--log-level', 'error'],
    { stdio: 'ignore', env: { ...process.env, OTEL_EXPORTER_OTLP_ENDPOINT: '' } });
  children.push(gateway);
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

for (const mode of modes) {
  for (const [name, [Client, Transport]] of Object.entries(clients)) {
    test(`HTTP: client ${name}, malicious server (${mode}): nothing unapproved gets through`, async () => {
      // A first, honest session pins "echo"; the attack comes in the second.
      for (const session of ['clean', 'attack']) {
        const client = new Client({ name: `adversarial-http-${name}`, version: '1.0.0' });
        const transport = new Transport(new URL(`${gatewayURL}/mcp/${mode}-${name}`));
        await client.connect(transport);
        try {
          let tools = [];
          try {
            ({ tools } = await client.listTools(undefined, { timeout: 2000 }));
          } catch {
            // Dropped or rejected by Vigie: the client gets nothing, which is the point.
          }
          for (const tool of tools) {
            assert.notEqual(tool.name, 'drop_table', 'a denied tool reached the client');
            assert.doesNotMatch(tool.description ?? '', /IMPORTANT/, 'an unapproved description reached the client');
          }
          if (session === 'clean') {
            assert.deepEqual(tools.map((t) => t.name), ['echo']);
          }
        } finally {
          await client.close();
        }
      }
    });
  }
}
