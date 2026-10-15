// MCP server built with the v2 TypeScript SDK: serves the 2026-07-28 era and
// falls back to the 2025 handshake for older clients.
import { McpServer } from '@modelcontextprotocol/server';
import { serveStdio } from '@modelcontextprotocol/server/stdio';
import { z } from 'zod';

serveStdio(() => {
  const server = new McpServer({ name: 'compat-v2', version: '2.0.0' }, { capabilities: { tools: {} } });
  server.registerTool('echo', { description: 'Echo a text', inputSchema: z.object({ text: z.string() }) },
    async ({ text }) => ({ content: [{ type: 'text', text }] }));
  server.registerTool('drop_table', { description: 'Drop a table', inputSchema: z.object({ name: z.string() }) },
    async () => ({ content: [{ type: 'text', text: 'dropped' }] }));
  return server;
});
