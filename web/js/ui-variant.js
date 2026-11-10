// The A/B/C... experiment switch in the top bar (up to five options). An experiment is a named question with up to three options; while one is active the pill
// reads "UI A | B | C ... - TESTING: <name>" with the chosen option underlined, and clicking it cycles the options. With no experiment
// active the pill is hidden and nothing here runs. The choice is kept in this browser, per experiment, and can be forced with ?ui=<experiment>:<a|b|c>.
//
// To run an experiment: defineExperiment(key, name, [labelA, labelB, labelC]) where the code that has the options lives, add its key
// to the ACTIVE list (several can run at once, each with its own pill and its own ?ui=<key>:<opt>, repeated), and read
// activeVariant(key) wherever the options differ (it answers 'a' when that experiment is not running). ACTIVE is empty when nothing is
// being tested, and then no pill shows.
// Listen for the 'ui-variant-change' event on document to redraw when the choice changes.
const storageKey = key => `aimebu2.ui.variant.${key}`; // one saved choice per experiment
const LETTERS = ['a', 'b', 'c', 'd', 'e'];
// An experiment has as many options as it has labels (three when it gives none), at most five: A to E.
const optionsOf = key => LETTERS.slice(0, Math.min(LETTERS.length, experiments[key]?.labels?.length || 3));

export const experiments = {};
// The running pills by experiment, so code can set one back (chooseVariant) as a click would.
const choosers = {};
export function chooseVariant(key, variant) { choosers[key]?.(variant, true); }
export function defineExperiment(key, name, labels, { hidden = false } = {}) { experiments[key] = { name, labels, hidden }; }

// The experiments that are on right now (each gets its own pill), or [] for none.
export const ACTIVE = ['forks']; // the way a thread panel shows forks is being chosen (see pages/chat/rows.js)

// The option of experiment `key`: its pill's choice while it runs ('a' otherwise). Each experiment keeps its own attribute on <html>
// (data-ui-<key>), so several can run at once; data-ui and data-ui-experiment still say which pill was set last.
const attr = key => `data-ui-${key}`;
export function activeVariant(key) {
  const value = document.documentElement.getAttribute(attr(key));
  return optionsOf(key).includes(value) ? value : 'a';
}

function storedVariant(key) {
  try { return localStorage.getItem(storageKey(key)); } catch { return null; }
}

// Starts every active experiment: the first gets the pill in the top bar, each further one a copy of it beside the first.
export function initUIVariants(template = document.querySelector('#ui-variant-toggle'), keys = ACTIVE) {
  const live = keys.filter(key => experiments[key]);
  if (!live.length) { initUIVariant(template, null); return; }
  let after = template;
  live.forEach((key, index) => {
    let button = template;
    if (index) { button = template.cloneNode(true); button.removeAttribute('id'); button.hidden = false; after.after(button); }
    after = button; initUIVariant(button, key);
  });
}

export function initUIVariant(button = document.querySelector('#ui-variant-toggle'), key = Array.isArray(ACTIVE) ? ACTIVE[0] : ACTIVE) {
  try { localStorage.removeItem('aimebu2.ui.variant'); } catch { /* the single key of the message-actions trial: a stale choice must not seed the next experiment */ }
  const experiment = key ? experiments[key] : null;
  if (!experiment) {
    button.hidden = true;
    delete document.documentElement.dataset.ui; delete document.documentElement.dataset.uiExperiment;
    return;
  }
  // ?ui=<experiment>:<option> forces an option (?ui=composer:b). A bare ?ui=b, or one naming another experiment, is a leftover from
  // an earlier experiment: it is ignored and stripped from the URL.
  // With several experiments running the parameter is repeated (?ui=first:b&ui=second:c); each pill reads its own and a
  // value that names no running experiment is dropped.
  const url = new URL(location.href), raws = url.searchParams.getAll('ui');
  const variants = optionsOf(key), valid = value => variants.includes(value);
  const mine = raws.map(raw => /^([\w-]+):([a-e])$/.exec(raw)).filter(match => match && match[1] === key && valid(match[2]));
  const requested = mine.length ? mine[0][2] : null;
  const running = Array.isArray(ACTIVE) ? ACTIVE : [ACTIVE];
  const keep = raws.filter(raw => { const match = /^([\w-]+):([a-e])$/.exec(raw); return match && (running.includes(match[1]) || match[1] === key) && experiments[match[1]] && optionsOf(match[1]).includes(match[2]); });
  if (raws.length && keep.length !== raws.length) { url.searchParams.delete('ui'); for (const raw of keep) url.searchParams.append('ui', raw); history.replaceState(history.state, '', url); }
  const saved = storedVariant(key);
  if (experiment.hidden && !requested && !valid(saved)) { button.hidden = true; document.documentElement.removeAttribute(attr(key)); return; }
  let variant = valid(requested) ? requested : valid(saved) ? saved : 'a';
  button.hidden = false;
  // The options of the pill: "A | B | C ...", one per option of this experiment.
  const options = button.querySelector('.ui-variant-options');
  if (options) {
    options.replaceChildren(...variants.flatMap((letter, index) => {
      const item = document.createElement('span'); item.className = `ui-variant-opt ui-variant-${letter}`; item.textContent = letter.toUpperCase();
      if (!index) return [item];
      const bar = document.createElement('span'); bar.setAttribute('aria-hidden', 'true'); bar.textContent = '|';
      return [bar, item];
    }));
  }
  const testing = button.querySelector('.ui-variant-testing');
  if (testing) testing.textContent = ` \u2014 TESTING: ${experiment.name}`;

  function show(next, updateURL) {
    variant = next;
    document.documentElement.dataset.ui = variant; document.documentElement.dataset.uiExperiment = key; document.documentElement.setAttribute(attr(key), variant);
    button.dataset.active = variant;
    const index = variants.indexOf(variant), following = variants[(index + 1) % variants.length].toUpperCase();
    const label = experiment.labels?.[index];
    button.setAttribute('aria-label', `Testing ${experiment.name}: option ${variant.toUpperCase()}${label ? ` (${label})` : ''}. Switch to ${following}`);
    button.title = `Testing ${experiment.name}: ${label || `option ${variant.toUpperCase()}`}. Click for ${following}`;
    try { localStorage.setItem(storageKey(key), variant); } catch { /* Private browsing may deny storage. */ }
    if (updateURL) {
      const next = new URL(location.href);
      const others = next.searchParams.getAll('ui').filter(raw => !raw.startsWith(`${key}:`));
      next.searchParams.delete('ui'); for (const raw of [...others, `${key}:${variant}`]) next.searchParams.append('ui', raw);
      history.replaceState(history.state, '', next);
    }
    document.dispatchEvent(new CustomEvent('ui-variant-change', { detail: { variant, experiment: key } }));
  }

  show(variant, false);
  choosers[key] = show;
  button.addEventListener('click', () => show(variants[(variants.indexOf(variant) + 1) % variants.length], true));
}
