// A malicious server tries to get a changed tool description, or a denied
// tool, past Vigie to the official TypeScript SDK clients.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, writeFileSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { Client as ClientV1 } from 'sdk-v1/client/index.js';
import { StdioClientTransport as StdioV1 } from 'sdk-v1/client/stdio.js';
import { Client as ClientV2 } from '@modelcontextprotocol/client';
import { StdioClientTransport as StdioV2 } from '@modelcontextprotocol/client/stdio';

const here = dirname(fileURLToPath(import.meta.url));
const vigie = process.env.VIGIE_BIN ?? resolve(here, '../../bin/vigie');
assert.ok(existsSync(vigie), `${vigie} not found: run "make build" first`);

const clients = { v1: [ClientV1, StdioV1], v2: [ClientV2, StdioV2] };

for (const mode of ['string-id', 'float-id', 'deep', 'nameless', 'ambiguous', 'duplicate']) {
  for (const [name, [Client, Transport]] of Object.entries(clients)) {
    test(`client ${name}, malicious server (${mode}): nothing unapproved gets through`, async () => {
      const dir = mkdtempSync(join(tmpdir(), 'vigie-adversarial-'));
      const config = join(dir, 'vigie.yaml');
      writeFileSync(config, `version: 1\npins_dir: ${join(dir, 'pins')}\nservers:\n  evil:\n    tools:\n      deny: ["drop_*"]\n`);
      // A first, honest session pins "echo"; the attack comes in the second.
      for (const session of ['clean', mode]) {
        const transport = new Transport({
          command: vigie,
          args: ['run', '--server', 'evil', '--config', config, '--log-level', 'error', '--',
            process.execPath, join(here, 'evil-server.mjs'), session],
          env: { ...process.env, OTEL_EXPORTER_OTLP_ENDPOINT: '' },
          stderr: 'ignore',
        });
        const client = new Client({ name: `adversarial-${name}`, version: '1.0.0' });
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
