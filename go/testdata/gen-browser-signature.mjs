/* Regenerate helper/go/testdata/browser-signature.json — using the panel's OWN
   signingInput, lifted out of monitor.js rather than copied by hand.

   The point of the fixture is that a signature made by WebCrypto verifies in
   Go. A hand-copied signing input would still produce a fixture that passes
   while the shipped panel disagreed, which is the exact failure the fixture
   exists to catch. So: read monitor.js, cut the function out by matching
   braces, evaluate that, and sign with it.

   Run from the repo root:
     node <this> > helper/go/testdata/browser-signature.json  */
import { readFileSync } from 'node:fs';

const src = readFileSync('monitor.js', 'utf8');
const start = src.indexOf('function signingInput(');
if (start < 0) throw new Error('monitor.js: signingInput is gone — has it been renamed?');
let depth = 0, end = -1;
for (let i = src.indexOf('{', start); i < src.length; i++) {
  if (src[i] === '{') depth++;
  else if (src[i] === '}' && --depth === 0) { end = i + 1; break; }
}
if (end < 0) throw new Error('monitor.js: could not find the end of signingInput');
const fnSrc = src.slice(start, end);

const version = (src.match(/const SIG_VERSION = '([^']+)'/) || [])[1];
if (!version) throw new Error('monitor.js: SIG_VERSION is gone');

const signingInput = new Function('SIG_VERSION',
  fnSrc + '; return signingInput;')(version);

const b64 = u8 => Buffer.from(u8).toString('base64');
const kp = await crypto.subtle.generateKey({ name: 'Ed25519' }, true, ['sign', 'verify']);
const pub = b64(new Uint8Array(await crypto.subtle.exportKey('raw', kp.publicKey)));

/* A newline and a non-ASCII byte, which are the two things the length prefixes
   exist for: the newline is the v1 re-cut, and the multi-byte character is the
   one case where counting characters instead of bytes would disagree. */
const f = {
  pub,
  agent: 'ho2JBnXCez_Q',
  id: 'c7f3a1',
  kind: 'sh',
  target: '',
  at: 1785969600,
  cmd: 'systemctl status nginx\nuptime — ok',
};
const sig = new Uint8Array(await crypto.subtle.sign({ name: 'Ed25519' }, kp.privateKey,
  signingInput(f.agent, f.id, f.kind, f.target, f.at, f.cmd)));

process.stdout.write(JSON.stringify({ ...f, sig: b64(sig) }, null, 1) + '\n');
