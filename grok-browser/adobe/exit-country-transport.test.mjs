import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import https from 'node:https';
import net from 'node:net';
import { once } from 'node:events';
import { execFileSync, spawn } from 'node:child_process';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { lookupExitCountry } from './exit-country.mjs';

// No Internet or live account: real curl uses only loopback TLS/proxy fixtures.
test('real curl honors HTTP/HTTPS and authenticated SOCKS5 routes with no account headers or direct fallback', { timeout: 15000 }, async t => {
  const dir = await mkdtemp(join(tmpdir(), 'adobe-country-transport-'));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const certPath = join(dir, 'cert.pem'), keyPath = join(dir, 'key.pem');
  execFileSync('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1', '-subj', '/CN=ipwho.is',
    '-addext', 'subjectAltName=DNS:ipwho.is,IP:127.0.0.1', '-keyout', keyPath, '-out', certPath], { stdio: 'ignore' });
  const tlsOptions = { cert: await readFile(certPath), key: await readFile(keyPath) };
  const sockets = new Set(), servers = [];
  t.after(async () => { for (const socket of sockets) socket.destroy(); await Promise.all(servers.map(server => new Promise(resolve => server.close(resolve)))); });
  async function listen(server) {
    server.on('connection', socket => { sockets.add(socket); socket.on('close', () => sockets.delete(socket)); });
    server.listen(0, '127.0.0.1'); await once(server, 'listening'); servers.push(server); return server.address().port;
  }
  let calls = 0, redirect = false;
  const target = https.createServer(tlsOptions, (req, res) => {
    calls++;
    assert.equal(req.headers.host, 'ipwho.is'); assert.equal(req.url, '/');
    assert.equal(req.headers.authorization, undefined); assert.equal(req.headers.cookie, undefined);
    assert.equal(req.headers['proxy-authorization'], undefined);
    if (redirect) { res.writeHead(302, { location: 'https://ipwho.is/redirected' }); res.end('{"country_code":"GB"}'); return; }
    res.setHeader('Content-Type', 'application/json'); res.end('{"success":true,"country_code":"JP"}');
  });
  const targetPort = await listen(target);
  let connects = 0;
  const tunnel = (req, client, head) => {
    connects++;
    assert.equal(req.url, 'ipwho.is:443');
    assert.equal(req.headers['proxy-authorization'], `Basic ${Buffer.from('fixture:secret').toString('base64')}`);
    const upstream = net.connect(targetPort, '127.0.0.1'); sockets.add(upstream);
    upstream.once('connect', () => { client.write('HTTP/1.1 200 Connection Established\r\n\r\n'); if (head.length) upstream.write(head); client.pipe(upstream); upstream.pipe(client); });
    upstream.on('error', () => client.destroy()); client.on('error', () => upstream.destroy());
  };
  const proxyHTTP = http.createServer(); proxyHTTP.on('connect', tunnel);
  const proxyHTTPS = https.createServer(tlsOptions); proxyHTTPS.on('connect', tunnel);
  const httpPort = await listen(proxyHTTP), httpsPort = await listen(proxyHTTPS);
  const spawnProcess = (command, args, options) => spawn(command, [...args, '--cacert', certPath, '--proxy-cacert', certPath], options);
  for (const route of [`http://fixture:secret@127.0.0.1:${httpPort}`, `https://fixture:secret@127.0.0.1:${httpsPort}`]) {
    assert.deepEqual(await lookupExitCountry(route, { spawnProcess }), { country: 'JP', source: 'ipwho_is' });
  }
  assert.equal(connects, 2); assert.equal(calls, 2);

  const read = async (socket, length) => {
    while (true) { const buffer = socket.read(length); if (buffer !== null) return buffer; if (socket.destroyed) throw new Error('closed'); await once(socket, 'readable'); }
  };
  let socksAuth = 0;
  const proxySOCKS = net.createServer(socket => {
    (async () => {
      const greeting = await read(socket, 2); assert.equal(greeting[0], 5);
      await read(socket, greeting[1]); socket.write(Buffer.from([5, 2]));
      const auth = await read(socket, 2); assert.equal(auth[0], 1);
      const username = await read(socket, auth[1]), size = await read(socket, 1), password = await read(socket, size[0]);
      assert.equal(username.toString(), 'fixture'); assert.equal(password.toString(), 'secret'); socksAuth++;
      socket.write(Buffer.from([1, 0]));
      const header = await read(socket, 4); assert.deepEqual([...header], [5, 1, 0, 3]);
      const hostSize = await read(socket, 1), host = await read(socket, hostSize[0]), port = await read(socket, 2);
      assert.equal(host.toString(), 'ipwho.is'); assert.equal(port.readUInt16BE(), 443);
      const upstream = net.connect(targetPort, '127.0.0.1'); sockets.add(upstream); await once(upstream, 'connect');
      socket.write(Buffer.from([5, 0, 0, 1, 127, 0, 0, 1, 1, 1])); socket.pipe(upstream); upstream.pipe(socket);
      upstream.on('error', () => socket.destroy()); socket.on('error', () => upstream.destroy());
    })().catch(error => { socket.destroy(); t.assert.fail(error.message); });
  });
  const socksPort = await listen(proxySOCKS);
  for (const protocol of ['socks5', 'socks5h']) {
    assert.deepEqual(await lookupExitCountry(`${protocol}://fixture:secret@127.0.0.1:${socksPort}`, { spawnProcess }), { country: 'JP', source: 'ipwho_is' });
  }
  assert.equal(socksAuth, 2); assert.equal(calls, 4);

  // Even a working environment proxy cannot override explicit direct.
  const prior = process.env.HTTPS_PROXY;
  process.env.HTTPS_PROXY = `http://fixture:secret@127.0.0.1:${httpPort}`;
  try {
    assert.deepEqual(await lookupExitCountry('direct', {
      spawnProcess: (command, args, options) => spawnProcess(command, [...args, '--connect-to', `ipwho.is:443:127.0.0.1:${targetPort}`], options),
    }), { country: 'JP', source: 'ipwho_is' });
    assert.equal(connects, 2); assert.equal(calls, 5);
    assert.deepEqual(await lookupExitCountry('http://127.0.0.1:1', { spawnProcess }), { country: 'US', source: 'default_us' });
    assert.equal(calls, 5); assert.equal(connects, 2);
  } finally { if (prior === undefined) delete process.env.HTTPS_PROXY; else process.env.HTTPS_PROXY = prior; }
  redirect = true;
  assert.deepEqual(await lookupExitCountry(`http://fixture:secret@127.0.0.1:${httpPort}`, { spawnProcess }), { country: 'US', source: 'default_us' });
  assert.equal(calls, 6);
});
