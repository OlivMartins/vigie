// Vigie between the official MCP TypeScript SDKs, client and server, v1 and
// v2, in both protocol eras. Run with "make compat" (builds bin/vigie first).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, writeFileSync, existsSync } from 'node:fs';
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

// A key assembled at run time: the repository holds no literal credential.
const awsKey = 'AKIA' + 'Z7QJ4XWVN2L5KT3M';

const clients = {
  v1: { Client: ClientV1, Transport: StdioV1, options: {} },
  v2: { Client: ClientV2, Transport: StdioV2, options: { versionNegotiation: { mode: 'auto' } } },
};

function setup() {
  const dir = mkdtempSync(join(tmpdir(), 'vigie-compat-'));
  const config = join(dir, 'vigie.yaml');
  writeFileSync(config, `version: 1\npins_dir: ${join(dir, 'pins')}\nservers:\n  compat:\n    tools:\n      deny: ["drop_*"]\n`);
  return { config, traces: join(dir, 'traces.jsonl') };
}

function spans(path) {
  return readFileSync(path, 'utf8').trim().split('\n').map((line) => {
    const span = JSON.parse(line);
    span.attrs = Object.fromEntries((span.Attributes ?? []).map((kv) => [kv.Key, kv.Value.Value]));
    return span;
  });
}

for (const [clientName, c] of Object.entries(clients)) {
  for (const serverName of ['v1', 'v2']) {
    const modern = clientName === 'v2' && serverName === 'v2';
    test(`client ${clientName} ↔ vigie ↔ server ${serverName} (${modern ? '2026-07-28' : 'handshake'})`, async () => {
      const { config, traces } = setup();
      const transport = new c.Transport({
        command: vigie,
        args: ['run', '--server', 'compat', '--config', config, '--trace-file', traces, '--log-level', 'error',
          '--', process.execPath, join(here, `server-${serverName}.mjs`)],
        env: { ...process.env, OTEL_EXPORTER_OTLP_ENDPOINT: '' },
        stderr: 'inherit',
      });
      const client = new c.Client({ name: `compat-client-${clientName}`, version: '1.0.0' }, c.options);
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
      } finally {
        await client.close();
      }

      const recorded = spans(traces);
      const call = recorded.find((s) => s.Name === 'tools/call echo' && s.SpanKind === 3);
      assert.ok(call, 'a CLIENT span for the relayed call');
      assert.equal(call.attrs['mcp.method.name'], 'tools/call');
      assert.equal(call.attrs['gen_ai.tool.name'], 'echo');
      assert.equal(call.attrs['network.transport'], 'pipe');
      if (modern) {
        assert.equal(call.attrs['mcp.protocol.version'], '2026-07-28');
      } else {
        assert.match(call.attrs['mcp.protocol.version'], /^2025-/);
      }
      const denied = recorded.find((s) => s.Name === 'tools/call echo' && s.attrs['vigie.decision'] === 'block');
      assert.equal(denied?.attrs['vigie.rule'], 'secrets.arguments');
      assert.ok(!readFileSync(traces, 'utf8').includes(awsKey), 'the key never reaches telemetry');
    });
  }
}
