// MCP server built with the v1 TypeScript SDK (2025 handshake era).
import { McpServer } from 'sdk-v1/server/mcp.js';
import { StdioServerTransport } from 'sdk-v1/server/stdio.js';
import { z } from 'zod';

const server = new McpServer({ name: 'compat-v1', version: '1.0.0' });
server.registerTool('echo', { description: 'Echo a text', inputSchema: { text: z.string() } },
  async ({ text }) => ({ content: [{ type: 'text', text }] }));
server.registerTool('drop_table', { description: 'Drop a table', inputSchema: { name: z.string() } },
  async () => ({ content: [{ type: 'text', text: 'dropped' }] }));
await server.connect(new StdioServerTransport());
