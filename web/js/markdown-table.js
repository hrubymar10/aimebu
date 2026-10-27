// How the Markdown renderer reads a table: a line with a pipe followed by a rule line of the same number of cells, with or without the
// outer pipes. Pure, so the composer's paste check reads tables exactly as the renderer does.

export function cells(line) {
  let row = line.trim(); if (row.startsWith('|')) row = row.slice(1); if (row.endsWith('|')) row = row.slice(0, -1);
  const out = []; let cell = '', ticks = 0;
  for (let i = 0; i < row.length; i++) {
    const char = row[i];
    if (char === '\\' && row[i + 1] === '|') { cell += '|'; i++; continue; }
    if (char === '`') { ticks = ticks ? 0 : 1; cell += char; continue; }
    if (char === '|' && !ticks) { out.push(cell.trim()); cell = ''; continue; }
    cell += char;
  }
  out.push(cell.trim()); return out;
}
export function tableSpec(line, next) {
  if (!line.includes('|')) return null;
  const headings = cells(line), rules = cells(next || '');
  if (headings.length !== rules.length || !rules.every(rule => /^:?-+:?$/.test(rule))) return null;
  return { headings, aligns: rules.map(rule => rule.startsWith(':') && rule.endsWith(':') ? 'center' : rule.endsWith(':') ? 'right' : 'left') };
}
