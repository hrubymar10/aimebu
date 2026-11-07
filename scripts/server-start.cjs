// Starts an aimebu server on a port of its own, for the scripts that drive one (the page tests' scratch server, the smoke, the audits).
//
// A free port is found by listening on :0 and closing, and the server binds it a moment later, in another process: something else on the
// machine (another test run, the OS handing the port to an outgoing connection) can take it in between, and the server then says "already
// in use" and exits. With runs side by side that is a flake, so a start that fails that way is tried again on a new port, up to `attempts`
// times; any other failure is reported at once, with what the server printed.
const net = require('node:net');
const { spawn } = require('node:child_process');

const freePort = () => new Promise((resolve, reject) => { const server = net.createServer(); server.once('error', reject); server.listen(0, '127.0.0.1', () => { const port = server.address().port; server.close(() => resolve(port)); }); });

// How old a server must be before an answer on its port counts (see startServer).
const BIND_SETTLE_MS = 600;

// True when the server's output says it could not bind its port (listenError in cmd/aimebu/main.go: "port N is already in use by another process").
const bindFailed = logs => /already in use|address already in use|EADDRINUSE/i.test(logs);

// Starts `binary server` and waits for it to answer. `envFor(port)` gives the environment of one attempt (AIMEBU2_PORT among it). `label` names the
// server in an error; `args` is what follows the binary (a test passes a stand-in script). Returns { child, port, origin, logs() } where logs() is what the server printed so far.
async function startServer(binary, envFor, { label = 'server', attempts = 5, args = ['server'], cwd, stdio = ['ignore', 'pipe', 'pipe'], waitMs = 30000 } = {}) {
  let last = '';
  for (let attempt = 1; attempt <= attempts; attempt++) {
    const port = await freePort(), origin = `http://127.0.0.1:${port}`;
    const began = Date.now();
    const child = spawn(binary, args, { env: envFor(port), cwd, stdio });
    let logs = '';
    child.stderr?.on('data', data => { logs += data; }); child.stdout?.on('data', data => { logs += data; });
    let exited = false; child.once('exit', () => { exited = true; });
    const deadline = Date.now() + waitMs;
    let up = false;
    while (!up && !exited && Date.now() < deadline) {
      try { if ((await fetch(origin)).ok) { up = true; break; } } catch { /* not up yet */ }
      await new Promise(resolve => setTimeout(resolve, 100));
    }
    // Another process may answer on the port the server failed to take, and answer before the server has even tried to bind (the bind is among the
    // first things a server does, a few milliseconds in). So an answer in the first moments proves nothing: the server must still be running, and
    // not have reported a failed bind, once it is BIND_SETTLE_MS old. A real server takes longer than that to answer anyway.
    if (up) { const wait = began + BIND_SETTLE_MS - Date.now(); if (wait > 0) await new Promise(resolve => setTimeout(resolve, wait)); }
    if (up && !exited && !bindFailed(logs)) return { child, port, origin, logs: () => logs };
    last = logs;
    if (!exited) { try { child.kill('SIGTERM'); } catch { /* gone */ } await new Promise(resolve => child.once('exit', resolve)); }
    if (!bindFailed(logs)) throw new Error(`${label} did not start: ${logs}`);
  }
  throw new Error(`${label} did not start after ${attempts} tries: its port was taken each time: ${last}`);
}

module.exports = { freePort, startServer, bindFailed };
