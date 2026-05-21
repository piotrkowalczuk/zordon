// Minimal stdlib-only HTTP server for examples/nodejs — no deps, so
// `npm install` is a no-op build and the example runs without
// downloading anything beyond the node toolchain + refreshed corepack.
//
// Note: no explicit stdout flush. Alpha hands nodejs services a PTY by
// default (toolchainDefaultsFor[ToolchainNode].TTY == true), so
// process.stdout is line-buffered and the startup `up ...` line
// reaches alpha's log immediately — same Ruby-pipe-buffering pitfall
// avoided the same way.
const http = require("node:http");

const port = Number(process.env.PORT || 8080);
const host = "127.0.0.1";
const body = "nodejs-example ok\n";

const server = http.createServer((_req, res) => {
  res.writeHead(200, {
    "Content-Type": "text/plain",
    "Content-Length": Buffer.byteLength(body),
  });
  res.end(body);
});

server.listen(port, host, () => {
  console.log(`up ${host}:${port}`);
});
