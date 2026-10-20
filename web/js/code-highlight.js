// Fenced code blocks in rendered messages: syntax highlighting (highlight.js, vendored in web/vendor/highlight, loaded only when a
// block names a language it knows) and our own diff rendering (no library). Both only add spans around the same text, so a
// block's textContent never changes and the Copy button, which copies the raw source it was given, is unaffected.
const BASE = '/vendor/highlight';
// Grammar file -> the aliases a fence may carry (the grammars' own). The grammar called `shell` is for console sessions
// (`console`, `shellsession`); a fence labelled `shell` is plain commands, so that name is sent to bash below.
export const LANGUAGES = {
  bash: ['sh', 'zsh'], c: ['h'], cpp: ['cc', 'c++', 'h++', 'hpp', 'hh', 'hxx', 'cxx'], csharp: ['cs', 'c#'], css: [], dockerfile: ['docker'],
  go: ['golang'], graphql: ['gql'], ini: ['toml'], java: ['jsp'], javascript: ['js', 'jsx', 'mjs', 'cjs'], json: ['jsonc', 'json5'],
  kotlin: ['kt', 'kts', 'ktm', 'ktx'], lua: ['pluto'], makefile: ['mk', 'mak', 'make'], markdown: ['md', 'mkdown', 'mkd'], nginx: ['nginxconf'], php: [],
  protobuf: ['proto'], python: ['py', 'gyp', 'ipython'], ruby: ['rb', 'gemspec', 'podspec', 'thor', 'irb'], rust: ['rs'], shell: ['console', 'shellsession'],
  sql: [], swift: [], typescript: ['ts', 'tsx', 'mts', 'cts'], xml: ['html', 'xhtml', 'rss', 'atom', 'xjb', 'xsd', 'xsl', 'plist', 'wsf', 'svg'], yaml: ['yml'],
};
const FILE = new Map();
for (const file of Object.keys(LANGUAGES)) FILE.set(file, file);
FILE.set('shell', 'bash');
for (const [file, aliases] of Object.entries(LANGUAGES)) for (const alias of aliases) if (!FILE.has(alias)) FILE.set(alias, file);
// A block bigger than this is shown plain: highlighting is synchronous work on the page's thread.
export const MAX_HIGHLIGHT = 40000;

// The language word of a fence's info string: "go", "golang title=x", "{.go}", "language-go" all give "go".
export function fenceLanguage(info) {
  const word = String(info || '').trim().split(/\s+/)[0] || '';
  const name = word.replace(/^\{?\.?/, '').replace(/^language-/i, '').replace(/[},;]+$/, '').toLowerCase();
  return /^[a-z0-9_+#.-]{1,24}$/.test(name) ? name : '';
}
export function grammarFile(language) { return FILE.get(language) || ''; }
export function isDiffLanguage(language) { return language === 'diff' || language === 'patch'; }

let core = null;
const grammars = new Map();
function loadCore() { core ||= import(`${BASE}/core.js`).then(module => module.default).catch(error => { core = null; throw error; }); return core; }
async function loadGrammar(hljs, file) {
  if (!grammars.has(file)) grammars.set(file, import(`${BASE}/languages/${file}.js`).then(module => { hljs.registerLanguage(file, module.default); }).catch(error => { grammars.delete(file); throw error; }));
  await grammars.get(file);
}

// highlight.js escapes its input, but the result is still rebuilt as DOM and cut down to text and `hljs-*` spans, so that nothing
// but those can reach the page whatever the library returns.
function toNodes(html) {
  const template = document.createElement('template'); template.innerHTML = html;
  const clean = parent => {
    for (const node of [...parent.childNodes]) {
      if (node.nodeType === Node.TEXT_NODE) continue;
      if (node.nodeType === Node.ELEMENT_NODE && node.tagName === 'SPAN') {
        for (const attribute of [...node.attributes]) if (attribute.name !== 'class') node.removeAttribute(attribute.name);
        node.className = node.className.split(/\s+/).filter(name => /^hljs-[a-z_-]+$/.test(name)).join(' ');
        clean(node);
      } else node.replaceWith(document.createTextNode(node.textContent));
    }
  };
  clean(template.content);
  return template.content;
}

// Highlights `code` (an element whose text is `text`) in `language`, if it is a known one and the block is not huge. Quietly leaves
// the block plain when the library cannot be loaded or the block changed meanwhile.
export async function highlightCode(code, text, language) {
  const file = grammarFile(language);
  if (!file || text.length > MAX_HIGHLIGHT || !text.trim()) return false;
  try {
    const hljs = await loadCore();
    await loadGrammar(hljs, file);
    if (code.textContent !== text || code.dataset.highlighted) return false;
    const nodes = toNodes(hljs.highlight(text, { language: file, ignoreIllegals: true }).value);
    if (nodes.textContent !== text) return false;
    code.replaceChildren(nodes); code.dataset.highlighted = file; code.classList.add('hljs');
    return true;
  } catch { return false; }
}

// ---- diffs ----
// Line kinds: file (--- / +++ headers), meta (diff --git, index, mode and rename lines, "\ No newline"), hunk (@@), add, del, ctx.
const HUNK = /^@@ -\d+(?:,(\d+))? \+\d+(?:,(\d+))? @@/;
const META = /^(?:diff |index |new file mode|deleted file mode|old mode|new mode|similarity index|dissimilarity index|rename |copy |Binary files |GIT binary patch)/;
export function classifyDiff(lines) {
  const kinds = [];
  let oldLeft = 0, newLeft = 0;
  for (let index = 0; index < lines.length; index++) {
    const line = lines[index], inHunk = oldLeft > 0 || newLeft > 0;
    const hunk = HUNK.exec(line);
    if (hunk && !inHunk) { oldLeft = hunk[1] === undefined ? 1 : Number(hunk[1]); newLeft = hunk[2] === undefined ? 1 : Number(hunk[2]); kinds.push('hunk'); continue; }
    if (inHunk) {
      if (line.startsWith('\\')) kinds.push('meta');
      else if (line.startsWith('+')) { newLeft--; kinds.push('add'); }
      else if (line.startsWith('-')) { oldLeft--; kinds.push('del'); }
      else { oldLeft--; newLeft--; kinds.push('ctx'); }
      continue;
    }
    if (line.startsWith('--- ') && String(lines[index + 1] ?? '').startsWith('+++ ')) kinds.push('file');
    else if (line.startsWith('+++ ') && kinds.at(-1) === 'file') kinds.push('file');
    else if (META.test(line)) kinds.push('meta');
    else if (line.startsWith('\\')) kinds.push('meta');
    // Outside any counted hunk (a hand-written or trimmed diff): the first character decides.
    else if (line.startsWith('+')) kinds.push('add');
    else if (line.startsWith('-')) kinds.push('del');
    else kinds.push('ctx');
  }
  return kinds;
}
// An unlabelled block counts as a unified diff when it has a hunk header, or a ---/+++ pair followed by a hunk header or a +/- line.
export function looksLikeDiff(text) {
  if (text.length > MAX_HIGHLIGHT) return false;
  const lines = text.split('\n');
  if (lines.some(line => HUNK.test(line))) return true;
  return lines.some((line, index) => line.startsWith('--- ') && String(lines[index + 1] ?? '').startsWith('+++ ') && /^[+-@]/.test(String(lines[index + 2] ?? '')));
}
// Fills `code` with one span per line (the newline stays a text node between them, so the text is unchanged).
export function renderDiff(code, text) {
  const lines = text.split('\n'), kinds = classifyDiff(lines), frame = document.createDocumentFragment();
  lines.forEach((line, index) => {
    const span = document.createElement('span'); span.className = `hl-line hl-${kinds[index]}`; span.textContent = line; frame.append(span);
    if (index < lines.length - 1) frame.append('\n');
  });
  code.replaceChildren(frame); code.classList.add('hl-diff'); code.dataset.highlighted = 'diff';
}
