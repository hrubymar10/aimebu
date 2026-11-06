// Accessibility audit of the static web UI: accessible names, text contrast and visible focus rings,
// across themes and desktop/phone viewports, for the main surfaces plus every popover and dialog.
// Usage: node scripts/a11y-audit.cjs [--json] [--strict]
// Exit status 1 when a control has no name or no focus ring; --strict also fails on any contrast miss.
// Surfaces are forced visible one at a time; fixtures come from ui-fixture.cjs (real renderers).
const { withPages, THEMES, VIEWPORTS } = require('./ui-fixture.cjs');

const SURFACES = [
  { name: 'app', root: '#app-view', exclude: '[data-surface], dialog, .popover-panel' },
  { name: 'health popover', root: '#health-popover', show: true },
  { name: 'usages popover', root: '#usages-popover', show: true },
  { name: 'system events popover', root: '#system-events-popover', show: true },
  { name: 'settings dialog', root: '#settings-dialog', dialog: true },
  { name: 'settings machines', root: '#settings-machines', show: true },
  { name: 'settings harnesses', root: '#settings-harnesses', show: true },
  { name: 'room info dialog', root: '#room-info-dialog', dialog: true },
  { name: 'room export dialog', root: '#room-export-dialog', dialog: true },
  { name: 'knowledge dialog', root: '#knowledge-dialog', dialog: true },
  { name: 'spawn dialog', root: '#spawn-dialog', dialog: true },
  { name: 'switch dialog', root: '#switch-dialog', dialog: true },
  { name: 'machine pair dialog', root: '#machine-pair-dialog', dialog: true },
  { name: 'project dialog', root: '#project-dialog', dialog: true },
  { name: 'import wizard (blocked)', root: '#import-v1-blocked', dialog: true },
  { name: 'message debug dialog', root: '.message-debug-dialog', dialog: true },
  { name: 'emoji picker', root: '.emoji-picker', show: true },
  { name: 'first-run theme picker', root: '#theme-view', show: true },
];

// Runs inside the page. Returns { names, contrast, focus } for one surface.
function auditSurface({ root, show, dialog, exclude }) {
  const rootEl = document.querySelector(root);
  if (!rootEl) return { missing: true };
  const forced = [];
  for (let node = rootEl; node && node !== document.body; node = node.parentElement) if (node.hidden) { node.hidden = false; forced.push(node); }
  if (dialog && !rootEl.open) { rootEl.setAttribute('open', ''); forced.push({ dialogOpen: rootEl }); }
  const hiddenInside = [...rootEl.querySelectorAll('[hidden]')].filter(node => node.closest(root) === rootEl);
  void hiddenInside; // elements the UI itself keeps hidden stay hidden: they are not part of the surface

  const within = element => (!exclude || !element.closest(exclude) || element.closest(exclude) === rootEl) && element.getClientRects().length > 0;
  const label = element => {
    const id = element.id ? `#${element.id}` : '';
    const cls = [...element.classList].slice(0, 2).map(name => `.${name}`).join('');
    return `${element.tagName.toLowerCase()}${id}${cls}`;
  };
  const text = node => (node.textContent || '').replace(/\s+/g, ' ').trim();

  // 1. accessible names
  const names = [];
  const interactive = 'button, a[href], input:not([type=hidden]), select, textarea, [role=button], [role=tab], [role=menuitem], summary';
  for (const element of rootEl.querySelectorAll(interactive)) {
    if (!within(element)) continue;
    let name = '';
    const where = label(element) + (element.type && element.tagName === 'INPUT' ? `[${element.type}]` : '') + (element.name ? `[name=${element.name}]` : '');
    const labelledby = element.getAttribute('aria-labelledby');
    if (labelledby) name = labelledby.split(/\s+/).map(id => text(document.getElementById(id) || document.createElement('i'))).join(' ').trim();
    if (!name) name = (element.getAttribute('aria-label') || '').trim();
    if (!name && element.labels?.length) name = [...element.labels].map(text).join(' ').trim();
    if (!name && !['INPUT', 'SELECT', 'TEXTAREA'].includes(element.tagName)) name = text(element) || element.querySelector('img[alt]:not([alt=""])')?.alt || element.querySelector('svg title')?.textContent || '';
    if (!name && element.tagName === 'INPUT' && ['button', 'submit', 'reset'].includes(element.type)) name = element.value;
    const weak = !name && (element.title || element.placeholder);
    if (!name) names.push({ element: where, weak: weak ? `title/placeholder only: "${element.title || element.placeholder}"` : '' });
  }

  // 1b. one button system: every button is a .btn, an .btn-icon or a .row-btn (with .btn-primary / .btn-danger as the only variants)
  const unclassed = [];
  for (const element of rootEl.querySelectorAll('button')) {
    if (!within(element)) continue;
    // A theme tile is a radio drawn as a preview of its theme (the first-run picker), not one of the three button kinds.
    if (!element.matches('.btn, .btn-icon, .row-btn, .settings-nav-item, .theme-tile[role="radio"]')) unclassed.push(`${label(element)} "${text(element).slice(0, 24)}"`);
  }

  // 2. contrast
  const parse = value => {
    const match = value.match(/rgba?\(([^)]+)\)/);
    if (!match) { const srgb = value.match(/color\(srgb ([^)]+)\)/); if (!srgb) return null; const parts = srgb[1].replace('/', ' ').split(/\s+/).map(Number); return [parts[0] * 255, parts[1] * 255, parts[2] * 255, parts[3] ?? 1]; }
    const parts = match[1].split(/[ ,\/]+/).filter(Boolean).map(Number);
    return [parts[0], parts[1], parts[2], parts[3] ?? 1];
  };
  const over = (top, bottom) => { const a = top[3] + bottom[3] * (1 - top[3]); return a === 0 ? [0, 0, 0, 0] : [0, 1, 2].map(i => (top[i] * top[3] + bottom[i] * bottom[3] * (1 - top[3])) / a).concat(a); };
  const luminance = ([r, g, b]) => { const f = v => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; }; return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b); };
  const background = element => {
    let layers = [], opacity = 1;
    for (let node = element; node; node = node.parentElement) {
      const style = getComputedStyle(node), color = parse(style.backgroundColor);
      if (color && color[3] > 0) { layers.push(color); if (color[3] >= 1) break; }
    }
    let base = [255, 255, 255, 1];
    const rootBg = parse(getComputedStyle(document.documentElement).backgroundColor); if (rootBg && rootBg[3] > 0) base = over(rootBg, base);
    for (let i = layers.length - 1; i >= 0; i--) base = over(layers[i], base);
    void opacity;
    return base;
  };
  const contrast = [];
  const walker = document.createTreeWalker(rootEl, NodeFilter.SHOW_TEXT);
  const seen = new Set();
  for (let node; (node = walker.nextNode());) {
    const element = node.parentElement;
    if (!element || seen.has(element) || !node.nodeValue.trim() || !within(element) || element.closest(':disabled, [aria-disabled=true]')) continue;
    seen.add(element);
    const style = getComputedStyle(element);
    if (style.visibility === 'hidden' || ['SCRIPT', 'STYLE', 'OPTION'].includes(element.tagName)) continue;
    let opacity = 1; for (let n = element; n; n = n.parentElement) opacity *= Number(getComputedStyle(n).opacity);
    const bg = background(element), fgRaw = parse(style.color); if (!fgRaw) continue;
    const fg = over([fgRaw[0], fgRaw[1], fgRaw[2], fgRaw[3] * opacity], bg);
    const ratio = (Math.max(luminance(fg), luminance(bg)) + 0.05) / (Math.min(luminance(fg), luminance(bg)) + 0.05);
    const size = parseFloat(style.fontSize), bold = Number(style.fontWeight) >= 700;
    const large = size >= 24 || (size >= 18.66 && bold);
    const need = large ? 3 : 4.5;
    if (ratio < need) contrast.push({ element: label(element), text: text(element).slice(0, 40), fg: style.color, bg: `rgb(${bg.slice(0, 3).map(Math.round).join(',')})`, ratio: Math.round(ratio * 100) / 100, need, size });
  }

  // 3. visible focus ring
  const focus = [];
  const focusable = 'button, a[href], input:not([type=hidden]), select, textarea, summary, [tabindex]:not([tabindex="-1"])';
  for (const element of rootEl.querySelectorAll(focusable)) {
    if (!within(element) || element.disabled) continue;
    element.focus({ focusVisible: true, preventScroll: true });
    if (document.activeElement !== element) continue;
    const style = getComputedStyle(element);
    const outline = style.outlineStyle !== 'none' && parseFloat(style.outlineWidth) > 0 && parse(style.outlineColor)?.[3] > 0;
    const shadow = style.boxShadow && style.boxShadow !== 'none';
    if (!outline && !shadow) focus.push({ element: label(element), outline: `${style.outlineStyle} ${style.outlineWidth}`, shadow: style.boxShadow });
    element.blur();
  }

  for (const item of forced.reverse()) { if (item.dialogOpen) item.dialogOpen.removeAttribute('open'); else item.hidden = true; }
  return { names, contrast, focus, unclassed };
}

(async () => {
  const report = {};
  await withPages(VIEWPORTS, async (viewport, page) => {
    for (const theme of THEMES) {
      await page.evaluate(theme => { const style = document.createElement('style'); style.textContent = '*,*::before,*::after{transition:none!important;animation:none!important}'; document.head.append(style); if (theme === 'dark') delete document.documentElement.dataset.theme; else document.documentElement.dataset.theme = theme; }, theme);
      for (const surface of SURFACES) {
        const result = await page.evaluate(auditSurface, surface);
        if (result.missing) continue;
        report[`${viewport}/${theme}/${surface.name}`] = result;
      }
    }
  });
  if (process.argv.includes('--json')) { process.stdout.write(JSON.stringify(report, null, 1) + '\n'); return; }
  const names = new Map(), focus = new Map(), contrast = {}, unclassed = new Map();
  for (const [key, result] of Object.entries(report)) {
    const [viewport, theme, surface] = key.split('/');
    for (const item of result.names) names.set(`${surface}: ${item.element}${item.weak ? ` (${item.weak})` : ''}`, true);
    for (const item of result.focus) focus.set(`${surface}: ${item.element}`, true);
    for (const item of result.unclassed || []) unclassed.set(`${surface}: ${item}`, true);
    for (const item of result.contrast) { const bucket = contrast[`${viewport}/${theme}`] ||= new Map(); const id = `${surface}: ${item.element} "${item.text}"`; if (!bucket.has(id) || bucket.get(id).ratio > item.ratio) bucket.set(id, item); }
  }
  console.log(`== Controls without an accessible name (${names.size})`); for (const item of names.keys()) console.log('  ' + item);
  console.log(`== Focusable controls with no visible focus ring (${focus.size})`); for (const item of focus.keys()) console.log('  ' + item);
  console.log(`== Buttons that are none of .btn, .btn-icon, .row-btn (${unclassed.size})`); for (const item of unclassed.keys()) console.log('  ' + item);
  for (const [context, bucket] of Object.entries(contrast)) {
    const items = [...bucket.entries()].sort((a, b) => a[1].ratio - b[1].ratio);
    console.log(`== Contrast failures ${context}: ${items.length}`);
    for (const [id, item] of items.slice(0, 5)) console.log(`  ${item.ratio}:1 (need ${item.need}) ${id}  fg ${item.fg} on ${item.bg}`);
  }
  const contrastFailures = Object.values(contrast).reduce((total, bucket) => total + bucket.size, 0);
  if (names.size || focus.size || unclassed.size || (process.argv.includes('--strict') && contrastFailures)) process.exitCode = 1;
})().catch(error => { console.error(error); process.exit(1); });
