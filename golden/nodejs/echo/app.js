// echo is a conformance-test fixture: a Node service that mirrors back
// its execution context (env, cwd, argv, the node that built it) as
// JSON so tests can assert on what zordon actually passed it. It is
// the Node twin of golden/{go,rust}/echo and speaks the identical wire
// shape so the Go-side test harness decodes all three with one struct.
//
// stdlib-only on purpose: no deps means `npm install` is a near-instant
// no-op build, the suite works offline beyond toolchain materialization,
// and the registry never blocks tests.
//
// Listen-address resolution is bilingual to keep all conformance tests
// on this one fixture:
//
//   - `-addr HOST:PORT` argv wins when present — matches the Go/Rust
//     fixtures' convention, used by tests that set `runtime { cmd }`.
//   - else, env.PORT on 127.0.0.1 — used by tests on the inferred
//     `<pm> start` path, since `npm start` doesn't pass argv through
//     to the underlying script without an explicit `--` separator.
const http = require("node:http");

const argv = process.argv;
const explicitAddr = flag(argv, "-addr");
const [host, port] = explicitAddr
  ? splitAddr(explicitAddr)
  : ["127.0.0.1", Number(process.env.PORT || 8080)];

const body = responseBody(argv);

const server = http.createServer((_req, res) => {
  res.writeHead(200, {
    "Content-Type": "application/json",
    "Content-Length": Buffer.byteLength(body),
    "Connection": "close",
  });
  res.end(body);
});

server.on("error", (err) => {
  console.error(`bind ${host}:${port}: ${err.message}`);
  process.exit(1);
});

server.listen(port, host, () => {
  console.log(`up ${host}:${port}`);
});

function flag(args, name) {
  const i = args.indexOf(name);
  if (i < 0 || i + 1 >= args.length) return null;
  return args[i + 1];
}

function splitAddr(s) {
  const i = s.lastIndexOf(":");
  return [s.slice(0, i), Number(s.slice(i + 1))];
}

function responseBody(argv) {
  const obj = {
    env: { ...process.env },
    cwd: process.cwd(),
    argv,
    runtime_version: process.version,
  };
  if (process.env.NODE_ECHO_GREETING === "1") {
    obj.greeting = "on";
  }
  return JSON.stringify(obj);
}
