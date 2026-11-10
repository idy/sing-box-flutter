// Local preview for the built static site. Run through the project launch entry.
import http from 'node:http';
import { readFile, realpath } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../.build/site');
const types = { '.html': 'text/html; charset=utf-8', '.css': 'text/css; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.svg': 'image/svg+xml', '.woff2': 'font/woff2', '.yaml': 'text/plain; charset=utf-8', '.txt': 'text/plain; charset=utf-8' };
await readFile(path.join(root, 'index.html'));
http.createServer(async (request, response) => {
  try {
    const url = new URL(request.url, 'http://localhost');
    const file = await realpath(path.join(root, decodeURIComponent(url.pathname === '/' ? '/index.html' : url.pathname)));
    if (!file.startsWith(root + path.sep)) throw new Error('Outside website');
    const content = await readFile(file);
    response.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream', 'Cache-Control': 'no-store' });
    response.end(content);
  } catch {
    response.writeHead(404, { 'Content-Type': 'text/plain; charset=utf-8' });
    response.end('Page not found.');
  }
}).listen(Number(process.env.PORT || 4173), '127.0.0.1', function () {
  console.log(`Project website ready on http://127.0.0.1:${this.address().port}`);
});
