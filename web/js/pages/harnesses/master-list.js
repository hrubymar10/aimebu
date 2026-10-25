import { html, useRef } from '../../ui.js';
import { harnessStatus, harnessTitle, tabTitle, tabKeyTarget } from './model.js';

// A vertical tab list of the machine's harnesses; the card of the selected one is shown beside it.
export function MasterList({ rows, selected, onSelect }) {
  const list = useRef(null);
  const keys = (event, index) => {
    const to = tabKeyTarget(event.key, index, rows.length);
    if (to < 0) return;
    event.preventDefault();
    const tab = list.current.querySelectorAll('[role=tab]')[to];
    tab.focus();
    onSelect(rows[to].id);
  };
  return html`
    <div class="harness-master" role="tablist" aria-orientation="vertical" aria-label="Harnesses" ref=${list}>
      ${rows.map((harness, index) => html`
        <button
          type="button"
          class="row-btn harness-master-item"
          role="tab"
          key=${harness.id}
          id=${`harness-tab-${harness.id}`}
          data-harness-id=${harness.id}
          aria-controls=${`harness-card-${harness.id}`}
          aria-selected=${String(harness.id === selected)}
          tabIndex=${harness.id === selected ? 0 : -1}
          title=${tabTitle(harness)}
          onClick=${() => onSelect(harness.id)}
          onKeyDown=${event => keys(event, index)}
        >
          <span class=${`harness-master-dot${harnessStatus(harness) === 'Available' ? ' is-available' : ''}`}></span>
          <span class="harness-master-name">${harnessTitle(harness)}</span>
          <span class="harness-master-count">${`${(harness.models || []).length}`}</span>
        </button>
      `)}
    </div>
  `;
}
