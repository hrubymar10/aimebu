import { html } from '../../ui.js';

// Keyed by the text, so each new failure starts collapsed.
export function DetectionDetails({ detail }) {
  return html`
    <details class="harness-detect-details" hidden=${!detail} key=${detail}>
      <summary>Details</summary>
      <p>${detail}</p>
    </details>
  `;
}
