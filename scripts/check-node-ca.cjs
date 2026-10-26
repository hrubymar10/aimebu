'use strict';
// Fails when the node this runs under trusts a CA root on the list of compromised roots (2026-10), in node's own bundled store or in the
// system store (/etc/ssl/certs/ca-certificates.crt, when the image has one). `make test-web` runs it first inside the container, so a new pin
// of WEB_TEST_NODE_IMAGE that still ships one of them cannot be adopted unnoticed. Fingerprints are the SHA-256 of the DER certificate.
const crypto = require('node:crypto');
const fs = require('node:fs');
const tls = require('node:tls');

const COMPROMISED = new Map([
  ['7DD78D5F4F13459A83DFF9ABBBA62EDBAF6F2D102BF257FD712F4D9F2746ED8D', 'emSign Root TLS CA - G3'],
  ['F2EDAAFF3D9B701FB4D14C9F285F8C0BAD90FC3BCC1FA07D47F4718EDABA34A5', 'emSign Root CS CA - G3'],
  ['D7A7A0FB5D7E2731D771E9484EBCDEF71D5F0C3E0A2948782BC83EE0EA699EF4', 'AAA Certificate Services'],
  ['86A1ECBA089C4A8D3BBE2734C612BA341D813E043CF9E8A862CD5C57A36BBE6B', 'emSign ECC Root CA - G3'],
]);
const SYSTEM_STORE = '/etc/ssl/certs/ca-certificates.crt';

const fingerprint = pem => crypto.createHash('sha256')
  .update(Buffer.from(pem.replace(/-----[^-]+-----/g, '').replace(/\s+/g, ''), 'base64')).digest('hex').toUpperCase();

const stores = { bundled: new Set(tls.rootCertificates.map(fingerprint)) };
try {
  const bundle = fs.readFileSync(SYSTEM_STORE, 'utf8');
  stores.system = new Set((bundle.match(/-----BEGIN CERTIFICATE-----[\s\S]+?-----END CERTIFICATE-----/g) || []).map(fingerprint));
} catch (err) {
  if (err.code !== 'ENOENT') throw err;
}

const found = [];
for (const [store, roots] of Object.entries(stores)) {
  for (const [print, name] of COMPROMISED) if (roots.has(print)) found.push(`${name} in the ${store} store`);
}
const sizes = Object.entries(stores).map(([store, roots]) => `${store} ${roots.size}`).join(', ');
if (found.length) {
  console.error(`node ${process.version} trusts compromised CA roots (${sizes}):\n  ${found.join('\n  ')}\nPin a node image whose stores are clean.`);
  process.exit(1);
}
console.log(`node ${process.version}: none of the ${COMPROMISED.size} compromised CA roots in its stores (${sizes})`);
