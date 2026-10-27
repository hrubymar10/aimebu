// Pasting into the rich composer. Which pasted text is Markdown the editor can turn into its own formatting, and which pasted HTML
// already carries formatting. Pure functions over strings, so they run in a vm test.

// What the text must contain to be worth converting: an inline mark the editor has, or a block it has.
const INLINE = [
  /(^|[\s(])\*\*[^*\s][^*\n]*\*\*/, // **bold**
  /(^|[\s(])__[^_\s][^_\n]*__/, // __bold__
  /(^|[\s(])~~[^~\s][^~\n]*~~/, // ~~strike~~
  /`[^`\n]+`/, // `code`
  /(^|[\s(])_[^_\s][^_\n]*_(?=$|[\s).,;:!?])/, // _italic_
  /(^|[\s(])\*[^*\s][^*\n]*\*(?=$|[\s).,;:!?])/, // *italic*
  /\[[^\]\n]+\]\((?:https?:\/\/|mailto:)[^)\s]+\)/, // [text](https://address)
];
const BLOCK = [
  /^\s*(`{3,}|~{3,})/m, // a fenced code block
  /^\s*(?:[-*+]|\d+\.)\s+\S/m, // a list item
  /^\s*>\s?\S/m, // a quote
];
// What the editor's document cannot hold (it has no headings, tables, rules, images or task lists): a paste that has any of it stays exactly as
// it was written, rather than being converted in part with the rest of its structure lost.
const UNSUPPORTED = [
  /^ {0,3}#{1,6}\s+\S/m, // a heading
  /^\s*\|.*\|\s*$/m, // a table row
  /^ {0,3}([-*_])(?:\s*\1){2,}\s*$/m, // a rule
  /!\[[^\]\n]*\]\([^)\n]*\)/, // an image
  /^\s*[-*+]\s+\[[ xX]\]\s/m, // a task item
];

// looksLikeMarkdown says pasted plain text is Markdown the editor can turn into formatting. Plain prose stays plain.
export function looksLikeMarkdown(text) {
  if (typeof text !== 'string' || !text.trim() || text.length > 200000) return false;
  if (UNSUPPORTED.some(pattern => pattern.test(text))) return false;
  return INLINE.some(pattern => pattern.test(text)) || BLOCK.some(pattern => pattern.test(text));
}

// carriesFormatting says pasted HTML already has the structure the editor reads (marks, lists, quotes, code, links). HTML that only has
// spans and divs, such as a code editor's copy, does not: its plain text is the Markdown source.
export function carriesFormatting(html) {
  return typeof html === 'string' && /<(?:strong|b|em|i|u|s|del|code|pre|ul|ol|li|blockquote|a)[\s>]/i.test(html);
}
