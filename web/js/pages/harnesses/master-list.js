import { html, useRef } from '../../ui.js';
import { useIdPrefix } from './id-prefix.js';
import { modelCount, harnessStatus, harnessTitle, tabTitle, tabKeyTarget } from './model.js';

// A horizontally scrolling tab list of the machine's harnesses; the card of the selected one is shown full width below it.
export function MasterList({ rows, selected, onSelect }) {
  const list = useRef(null);
  const prefix = useIdPrefix();
  const keys = (event, index) => {
    const to = tabKeyTarget(event.key, index, rows.length);
    if (to < 0) return;
    event.preventDefault();
    const tab = list.current.querySelectorAll('[role=tab]')[to];
    tab.focus();
    tab.scrollIntoView?.({ block: 'nearest', inline: 'nearest' });
    onSelect(rows[to].id);
  };
  // On a phone the tab strip is a select (the strip scrolls sideways and clips its labels); only one of the two is shown, by the style sheet.
  const picker = html`<select class="harness-master-select" aria-label="Harness" value=${selected} onChange=${event => onSelect(event.currentTarget.value)}>${rows.map(harness => html`<option key=${harness.id} value=${harness.id}>${`${harnessTitle(harness)} · ${modelCount(harness)}`}</option>`)}</select>`;
  return html`
    ${picker}
    <div class="harness-master" role="tablist" aria-orientation="horizontal" aria-label="Harnesses" ref=${list}>
      ${rows.map((harness, index) => html`
        <button
          type="button"
          class="row-btn harness-master-item"
          role="tab"
          key=${harness.id}
          id=${`${prefix}harness-tab-${harness.id}`}
          data-harness-id=${harness.id}
          aria-controls=${`${prefix}harness-card-${harness.id}`}
          aria-selected=${String(harness.id === selected)}
          tabIndex=${harness.id === selected ? 0 : -1}
          title=${tabTitle(harness)}
          onClick=${() => onSelect(harness.id)}
          onKeyDown=${event => keys(event, index)}
        >
          <span class=${`harness-master-dot${harnessStatus(harness) === 'Available' ? ' is-available' : ''}`}></span>
          <span class="harness-master-name">${harnessTitle(harness)}</span>
          <span class="harness-master-count">${modelCount(harness)}</span>
        </button>
      `)}
    </div>
  `;
}
