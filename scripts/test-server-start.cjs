// scripts/server-start.cjs: a server whose port was taken between picking and binding is started again on another port; any other failure is
// reported at once; a stranger answering on the port is not mistaken for the server. It uses small stand-in servers (a node script that
// prints what aimebu prints), no aimebu build and no browser.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const net = require('node:net');
const os = require('node:os');
const path = require('node:path');
const { startServer, bindFailed } = require('./server-start.cjs');

const work = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), 'server-start-test-'));
// A stand-in `aimebu server` (run as `node fake`, so no executable file is needed): reads the port from AIMEBU_PORT, and acts as FAKE_MODE says.
const fake = path.join(work, 'fake-aimebu');
fs.writeFileSync(fake, `const http = require('node:http'), fs = require('node:fs');
const port = Number(process.env.AIMEBU_PORT), mode = process.env.FAKE_MODE;
const count = process.env.FAKE_COUNT ? Number(fs.readFileSync(process.env.FAKE_COUNT, 'utf8') || 0) + 1 : 0;
if (process.env.FAKE_COUNT) fs.writeFileSync(process.env.FAKE_COUNT, String(count));
if (mode === 'crash') { console.error('something else broke'); process.exit(1); }
if (mode === 'taken-first' && count === 1) { console.error('cannot listen on 127.0.0.1:' + port + ': port ' + port + ' is already in use by another process'); process.exit(1); }
if (mode === 'taken-always') { console.error('cannot listen on 127.0.0.1:' + port + ': port ' + port + ' is already in use by another process'); process.exit(1); }
if (mode === 'silent') { setTimeout(() => {}, 60000); return; }
const server = http.createServer((req, res) => { res.end('ok'); });
server.on('error', error => { console.error(String(error)); process.exit(1); });
server.listen(port, '127.0.0.1');
`);
const env = port => ({ ...process.env, AIMEBU_PORT: String(port) });

(async () => {
  assert.ok(bindFailed('cannot listen on 127.0.0.1:9997: port 9997 is already in use by another process (listen tcp: bind: address already in use)'));
  assert.ok(bindFailed('Error: listen EADDRINUSE: address already in use 127.0.0.1:5000'));
  assert.ok(!bindFailed('something else broke'));

  // Starts and answers.
  {
    const server = await startServer(process.execPath, port => ({ ...env(port), FAKE_MODE: 'ok' }), { args: [fake] });
    assert.equal(server.origin, `http://127.0.0.1:${server.port}`);
    assert.equal((await fetch(server.origin)).status, 200);
    server.child.kill();
  }

  // The first start finds its port taken: a second try, on another port, succeeds.
  {
    const count = path.join(work, 'count-1'); fs.writeFileSync(count, '0');
    const ports = [];
    const server = await startServer(process.execPath, port => { ports.push(port); return { ...env(port), FAKE_MODE: 'taken-first', FAKE_COUNT: count }; }, { args: [fake] });
    assert.equal(ports.length, 2, 'one retry');
    assert.notEqual(ports[0], ports[1], 'on a new port');
    assert.equal((await fetch(server.origin)).status, 200);
    server.child.kill();
  }

  // Taken every time: it gives up after `attempts`, saying so, and leaves nothing running.
  {
    const count = path.join(work, 'count-2'); fs.writeFileSync(count, '0');
    await assert.rejects(startServer(process.execPath, port => ({ ...env(port), FAKE_MODE: 'taken-always', FAKE_COUNT: count }), { attempts: 3, args: [fake] }), /did not start after 3 tries: its port was taken each time/);
    assert.equal(fs.readFileSync(count, 'utf8'), '3');
  }

  // Another kind of failure is not retried, and the message carries what the server printed.
  {
    const count = path.join(work, 'count-3'); fs.writeFileSync(count, '0');
    await assert.rejects(startServer(process.execPath, port => ({ ...env(port), FAKE_MODE: 'crash', FAKE_COUNT: count }), { label: 'the thing', args: [fake] }), /the thing did not start: something else broke/);
    assert.equal(fs.readFileSync(count, 'utf8'), '1', 'tried once');
  }

  // A stranger already listening (and answering at once) on the picked port: the server's own bind fails with EADDRINUSE, so that attempt is not taken
  // for a start just because something answers there, and the second try, on another port, is.
  {
    const stranger = net.createServer(socket => { socket.end('HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nhi'); });
    const ports = [];
    const server = await startServer(process.execPath, port => {
      ports.push(port);
      if (ports.length === 1) stranger.listen(port, '127.0.0.1'); // bound before the child process has started
      return { ...env(port), FAKE_MODE: 'ok' };
    }, { args: [fake] });
    assert.equal(ports.length, 2, 'the port the stranger held was given up after the server failed to bind it');
    assert.equal(await (await fetch(server.origin)).text(), 'ok', 'and the answer is the server\'s own, not the stranger\'s');
    stranger.close(); server.child.kill();
  }

  fs.rmSync(work, { recursive: true, force: true });
  console.log('server-start: ok');
})().catch(error => { console.error(error); process.exit(1); });
