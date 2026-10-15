// MCP servers over Streamable HTTP for the compatibility tests, run as
// "node http-server.mjs v1|v2": v1 is the sessionful wiring of the v1 SDK
// (2025 revisions: sessions, SSE responses, GET stream), v2 the createMcpHandler
// entry of the v2 SDK (2026-07-28, with its stateless 2025 fallback). The
// port is printed on standard output once listening.
import { createServer } from 'node:http';
import { randomUUID } from 'node:crypto';
import { z } from 'zod';

const flavor = process.argv[2];

function tools(server, register) {
  register(server, 'echo', 'Echo a text', async ({ text }) => ({ content: [{ type: 'text', text }] }));
  register(server, 'drop_table', 'Drop a table', async () => ({ content: [{ type: 'text', text: 'dropped' }] }));
}

async function readBody(req) {
  const chunks = [];
  for await (const chunk of req) chunks.push(chunk);
  return Buffer.concat(chunks);
}

let handle;
if (flavor === 'v1') {
  const { McpServer } = await import('sdk-v1/server/mcp.js');
  const { StreamableHTTPServerTransport } = await import('sdk-v1/server/streamableHttp.js');
  const sessions = new Map();
  handle = async (req, res) => {
    const raw = await readBody(req);
    const body = raw.length ? JSON.parse(raw) : undefined;
    const sid = req.headers['mcp-session-id'];
    let transport = sid ? sessions.get(sid) : undefined;
    if (!transport && body?.method === 'initialize') {
      transport = new StreamableHTTPServerTransport({
        sessionIdGenerator: () => randomUUID(),
        onsessioninitialized: (id) => sessions.set(id, transport),
      });
      const server = new McpServer({ name: 'compat-http-v1', version: '1.0.0' });
      tools(server, (s, name, description, cb) => s.registerTool(name, { description, inputSchema: { text: z.string().optional(), name: z.string().optional() } }, cb));
      await server.connect(transport);
    }
    if (!transport) {
      res.writeHead(400, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ jsonrpc: '2.0', id: null, error: { code: -32000, message: 'Bad Request: No valid session ID provided' } }));
      return;
    }
    await transport.handleRequest(req, res, body);
  };
} else {
  const { McpServer, createMcpHandler } = await import('@modelcontextprotocol/server');
  const handler = createMcpHandler(() => {
    const server = new McpServer({ name: 'compat-http-v2', version: '2.0.0' }, { capabilities: { tools: {} } });
    tools(server, (s, name, description, cb) => s.registerTool(name, { description, inputSchema: z.object({ text: z.string().optional(), name: z.string().optional() }) }, cb));
    return server;
  });
  // node:http to the web-standard handler.
  handle = async (req, res) => {
    const raw = await readBody(req);
    const headers = new Headers();
    for (const [k, v] of Object.entries(req.headers)) {
      for (const value of [].concat(v)) headers.append(k, value);
    }
    const request = new Request(`http://${req.headers.host}${req.url}`, {
      method: req.method, headers, body: ['GET', 'HEAD'].includes(req.method) ? undefined : raw,
    });
    const response = await handler.fetch(request);
    res.writeHead(response.status, Object.fromEntries(response.headers));
    if (!response.body) return res.end();
    const reader = response.body.getReader();
    req.on('close', () => reader.cancel().catch(() => {}));
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      res.write(value);
    }
    res.end();
  };
}

const httpServer = createServer((req, res) => {
  handle(req, res).catch((err) => {
    console.error(err);
    if (!res.headersSent) res.writeHead(500);
    res.end();
  });
});
httpServer.listen(0, '127.0.0.1', () => console.log(httpServer.address().port));
