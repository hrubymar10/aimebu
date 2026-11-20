import { html, render } from '../../ui.js';
import { VARIANTS, usagesVariant, setUsagesVariant } from './variant.js';

const HINT = 'Usages view. A: the cards as they were. B: v1-style tiles, a bar per limit. C: one row per account, with the closest limit first.';

function Pill() {
  const variant = usagesVariant.value;
  return html`<div class="usages-variant-pill" role="group" aria-label="Usages view" title=${HINT}>
    ${VARIANTS.map(name => html`<button type="button" key=${name} class=${`usages-variant-option${variant === name ? ' active' : ''}`} data-variant=${name} aria-pressed=${String(variant === name)} title=${HINT} onClick=${() => setUsagesVariant(name)}>${name.toUpperCase()}</button>`)}</div>`;
}

// The A/B/C choice for every usages view (the Usages page and the usages panel), drawn once in the app top bar.
export function mountUsagesVariantPill(container) { render(html`<${Pill} />`, container); }
