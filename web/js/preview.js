// The one-line preview of a message under the open room in the room list: plain text, with the Markdown taken out. Code blocks read
// "[code]", a diff "[diff]" and an image "[image]", so a fence or a hunk header never shows as raw text.
import { fenceLanguage, isDiffLanguage, looksLikeDiff } from './code-highlight.js';

export function previewText(body) {
  const lines = String(body || '').replace(/\r\n?/g, '\n').split('\n');
  const out = [];
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i], fence = /^\s*(`{3,}|~{3,})([^`]*)$/.exec(line);
    if (fence) {
      const code = []; i++;
      while (i < lines.length && !lines[i].trim().startsWith(fence[1])) code.push(lines[i++]);
      const language = fenceLanguage(fence[2]);
      out.push(isDiffLanguage(language) || (!language && looksLikeDiff(code.join('\n'))) ? '[diff]' : '[code]');
      continue;
    }
    if (/^(\t| {4})\S/.test(line) && !/^\s*([-*+]|\d+[.)])\s/.test(line) && (i === 0 || !lines[i - 1].trim())) { out.push('[code]'); while (i + 1 < lines.length && /^(\t| {4})/.test(lines[i + 1])) i++; continue; }
    out.push(line.replace(/^\s{0,3}(#{1,6}\s+|>\s?|[-*+]\s+(\[[ xX]\]\s+)?|\d+[.)]\s+)/, ''));
  }
  return out.join(' ')
    .replace(/!\[[^\]]*\]\([^)]*\)/g, '[image]')
    .replace(/\[([^\]]*)\]\((?:[^)\s]|\s(?!"))*(?:\s"[^"]*")?\)/g, '$1')
    .replace(/<\/?u>/gi, '')
    .replace(/`([^`]*)`/g, '$1')
    .replace(/(\*\*|__)(.+?)\1/g, '$2').replace(/~~(.+?)~~/g, '$1').replace(/(^|[\s(])[*_]([^*_\n]+?)[*_](?=$|[\s).,!?:;])/g, '$1$2')
    .replace(/\s+/g, ' ').trim();
}
