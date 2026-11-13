import { html, useLayoutEffect, useRef } from '../../ui.js';
import { renderMarkdown } from '../../markdown.js';
import { relativeTime } from '../../time.js';

const SECTIONS = [
  ['current_state', 'Current state'],
  ['open_backlog', 'Open backlog'],
  ['conventions', 'Conventions'],
  ['gotchas', 'Gotchas'],
  ['timeline', 'Timeline'],
];
const SECTION_NAMES = Object.fromEntries(SECTIONS);

function Markdown({ source }) {
  const host = useRef(null);
  useLayoutEffect(() => { host.current.replaceChildren(renderMarkdown(source)); }, [source]);
  return html`<div class="handbook-markdown md-rendered" ref=${host}></div>`;
}

function Updated({ document }) {
  if (!document.updated_at) return html`<p class="handbook-meta">No updates yet.</p>`;
  return html`<p class="handbook-meta">Updated by ${document.updated_by || 'an agent'} · <time datetime=${document.updated_at} title=${document.updated_at}>${relativeTime(document.updated_at)}</time> · Revision ${document.rev}</p>`;
}

function Document({ document }) {
  const sections = document.sections || {};
  return html`<div class="handbook-document">
    <${Updated} document=${document} />
    ${document.over_budget && html`<p class="handbook-budget" role="status">${document.nudge || 'This handbook is over its 1,000-line guide. The leader should condense older Timeline entries.'}</p>`}
    ${SECTIONS.map(([key, label]) => html`<section class="handbook-section" key=${key} aria-label=${label}>
      <h4>${label}</h4>
      ${sections[key]?.trim() ? html`<${Markdown} source=${sections[key]} />` : html`<p class="handbook-empty-section">No entry yet.</p>`}
    </section>`)}
  </div>`;
}

function RevisionHistory({ store }) {
  const revisions = store.revisions.value;
  return html`<div id="handbook-history" class="handbook-history" aria-label="Handbook revision history">
    ${store.historyLoading.value && !revisions.length && html`<p role="status">Loading revisions…</p>`}
    ${store.historyError.value && html`<p class="error" role="alert">Could not load revisions: ${store.historyError.value}</p>`}
    ${!store.historyLoading.value && !store.historyError.value && !revisions.length && html`<p>No revisions yet.</p>`}
    ${revisions.length > 0 && html`<ol class="handbook-revisions">${revisions.map(item => html`<li key=${item.rev}>
      <div class="handbook-revision-head"><button type="button" class="btn" onClick=${() => store.showRevision(item.rev)} aria-label=${`View handbook revision ${item.rev}`}>Revision ${item.rev}</button>
        <span>${SECTION_NAMES[item.section] || item.section || 'Handbook'} · ${item.operation || 'updated'}</span>
        <small>${item.updated_by || 'an agent'} · <time datetime=${item.updated_at || ''} title=${item.updated_at || ''}>${item.updated_at ? relativeTime(item.updated_at) : 'unknown time'}</time></small></div>
      ${item.diff && html`<details><summary>View changes</summary><pre class="handbook-diff">${item.diff}</pre></details>`}
    </li>`)}</ol>`}
    ${store.nextBeforeRev.value != null && html`<button type="button" class="btn" disabled=${store.historyLoading.value} onClick=${() => store.loadHistory(true)}>${store.historyLoading.value ? 'Loading…' : 'Older revisions'}</button>`}
    ${store.historyError.value && html`<button type="button" class="btn" onClick=${() => store.loadHistory(revisions.length > 0)}>Retry</button>`}
  </div>`;
}

export function HandbookPage({ store, projectName, onClose }) {
  const snapshotRev = store.snapshotRev.value;
  const document = snapshotRev == null ? store.document.value : store.snapshot.value;
  return html`<div id="project-handbook" class="handbook-page">
    <header class="handbook-header"><div><small>${projectName}</small><h2 id="project-handbook-title">Project handbook</h2></div>
      <button type="button" class="btn-icon" title="Close handbook" aria-label="Close handbook" onClick=${onClose}><span class="icon-mask icon-mask-close" aria-hidden="true"></span></button></header>
    <div class="handbook-content">
      <p class="settings-desc">Project state maintained by the leader. Read the current handbook or an earlier revision.</p>
      <div class="handbook-actions">
        ${snapshotRev != null && html`<button type="button" class="btn" onClick=${() => store.showCurrent()}>Current handbook</button>`}
        <button type="button" class="btn" aria-expanded=${String(store.historyOpen.value)} onClick=${() => store.showHistory()}>${store.historyOpen.value ? 'Hide history' : 'Revision history'}</button>
      </div>
      ${snapshotRev != null && html`<p class="handbook-snapshot-label">Viewing revision ${snapshotRev}</p>`}
      ${(snapshotRev == null ? store.documentLoading.value : store.snapshotLoading.value) && html`<p role="status">Loading handbook…</p>`}
      ${(snapshotRev == null ? store.documentError.value : store.snapshotError.value) && html`<p class="error" role="alert">Could not load handbook: ${snapshotRev == null ? store.documentError.value : store.snapshotError.value}</p>`}
      ${snapshotRev == null && store.documentError.value && html`<button type="button" class="btn" onClick=${() => store.load(store.projectID)}>Retry handbook</button>`}
      ${document && html`<${Document} document=${document} />`}
      ${store.historyOpen.value && html`<${RevisionHistory} store=${store} />`}
    </div>
  </div>`;
}
