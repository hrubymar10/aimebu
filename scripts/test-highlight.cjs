// Code highlighting helpers: fence language parsing, the language table against the vendored grammars' own aliases, diff
// classification, unified-diff detection, and the vendored files against SHA256SUMS. The DOM parts are covered by test-ui.
'use strict';
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const { pathToFileURL } = require('node:url');

(async () => {
  const web = path.join(__dirname, '..', 'web');
  const mod = await import(pathToFileURL(path.join(web, 'js', 'code-highlight.js')).href);
  const dir = path.join(web, 'vendor', 'highlight');

  // The vendored files are the ones SHA256SUMS lists, and nothing else is in there.
  const sums = fs.readFileSync(path.join(dir, 'SHA256SUMS'), 'utf8').trim().split('\n').map(line => line.split(/\s+/));
  for (const [hash, file] of sums) assert.equal(crypto.createHash('sha256').update(fs.readFileSync(path.join(dir, file))).digest('hex'), hash, `${file} matches SHA256SUMS`);
  const listed = new Set(sums.map(([, file]) => file));
  const present = ['core.js', 'LICENSE', ...fs.readdirSync(path.join(dir, 'languages')).map(name => `languages/${name}`)];
  assert.deepEqual([...listed].sort(), present.sort(), 'SHA256SUMS lists exactly the vendored files');

  // The language table matches the grammars: same files, and every alias the grammar declares is accepted (plus `shell` for bash).
  const core = (await import(pathToFileURL(path.join(dir, 'core.js')).href)).default;
  const files = fs.readdirSync(path.join(dir, 'languages')).map(name => name.replace(/\.js$/, '')).sort();
  assert.deepEqual(Object.keys(mod.LANGUAGES).sort(), files, 'LANGUAGES names every vendored grammar');
  for (const file of files) {
    core.registerLanguage(file, (await import(pathToFileURL(path.join(dir, 'languages', `${file}.js`)).href)).default);
    for (const alias of core.getLanguage(file).aliases || []) assert.equal(mod.grammarFile(alias), file, `${alias} -> ${file}`);
  }
  for (const [name, file] of [['golang', 'go'], ['go', 'go'], ['js', 'javascript'], ['jsx', 'javascript'], ['tsx', 'typescript'], ['sh', 'bash'], ['shell', 'bash'], ['yml', 'yaml'], ['toml', 'ini'], ['html', 'xml'], ['proto', 'protobuf'], ['c#', 'csharp'], ['docker', 'dockerfile'], ['make', 'makefile']]) assert.equal(mod.grammarFile(name), file, `${name} -> ${file}`);
  for (const name of ['', 'diff', 'text', 'mermaid', 'nonsense']) assert.equal(mod.grammarFile(name), '', `${name || '(none)'} has no grammar`);
  // A real highlight through the vendored core: token spans around the same text.
  const html = core.highlight('func greet() string { return "hi" } // x', { language: 'go', ignoreIllegals: true }).value;
  assert.match(html, /<span class="hljs-keyword">func<\/span>/); assert.match(html, /hljs-string/); assert.match(html, /hljs-comment/);
  assert.equal(core.highlight('<script>alert(1)</script>', { language: 'xml' }).value.includes('<script>'), false, 'markup in the source comes out escaped');

  // Fence info strings.
  for (const [info, language] of [['go', 'go'], ['golang title=x', 'golang'], ['{.go}', 'go'], ['language-go', 'go'], ['  GO  ', 'go'], ['c++', 'c++'], ['', ''], ['<script>', ''], ['a'.repeat(40), '']]) assert.equal(mod.fenceLanguage(info), language, JSON.stringify(info));
  assert.ok(mod.isDiffLanguage('diff') && mod.isDiffLanguage('patch') && !mod.isDiffLanguage('go'));

  // Diff lines, including the cases a first-character rule gets wrong: "--- " / "+++ " lines inside a hunk are changes, not headers.
  const kinds = text => mod.classifyDiff(text.split('\n'));
  assert.deepEqual(kinds('diff --git a/x b/x\nindex 1..2 100644\n--- a/x\n+++ b/x\n@@ -1,3 +1,3 @@\n keep\n-old\n+new\n tail\n\\ No newline at end of file'),
    ['meta', 'meta', 'file', 'file', 'hunk', 'ctx', 'del', 'add', 'ctx', 'meta']);
  assert.deepEqual(kinds('@@ -1,2 +1,2 @@\n--- not a header\n+++ not a header\n-a\n+b\n@@ -9 +9 @@\n-c\n+d'), ['hunk', 'del', 'add', 'del', 'add', 'hunk', 'del', 'add'], 'counted hunks keep ---/+++ lines as changes');
  assert.deepEqual(kinds('--- a/f\n+++ b/f\n@@ -1 +1 @@\n-x\n+y\n--- a/g\n+++ b/g\n@@ -1 +1 @@\n-p\n+q'), ['file', 'file', 'hunk', 'del', 'add', 'file', 'file', 'hunk', 'del', 'add'], 'a second file header after a finished hunk');
  assert.deepEqual(kinds('+added\n-removed\n context'), ['add', 'del', 'ctx'], 'a trimmed diff with no hunk header falls back to the first character');
  assert.deepEqual(kinds('@@ -1,3 +1,3 @@\n a\n\n b'), ['hunk', 'ctx', 'ctx', 'ctx'], 'an empty line inside a hunk is context');
  assert.ok(mod.looksLikeDiff('@@ -1 +1 @@\n-a\n+b'));
  assert.ok(mod.looksLikeDiff('--- a/f\n+++ b/f\n-a\n+b'));
  assert.ok(!mod.looksLikeDiff('--- a/f\n+++ b/f'), 'two header lines alone are not enough');
  assert.ok(!mod.looksLikeDiff('echo hi\n- item\n+ item'), 'a list is not a diff');
  assert.ok(!mod.looksLikeDiff('x'.repeat(mod.MAX_HIGHLIGHT + 1)), 'a huge block is not scanned');
  console.log('code highlight test passed');
})().catch(error => { console.error(error); process.exit(1); });
