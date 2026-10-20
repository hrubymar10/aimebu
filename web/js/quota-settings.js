import { element } from './usage-format.js';
import { settingsRow, settingsGroup } from './settings-row.js';
import { iconButton } from './icon-button.js';

// Settings -> Usages -> Automatic account switching: the owner's per-tool ordered profile list,
// limits, protected profiles, after-reset order, dry run, and the server's decisions. Everything the
// server says (reasons, names, profile statuses) is rendered with textContent only.
const TOOLS = [['claude', 'Claude'], ['codex', 'Codex']];
const RANGES = { session_max: [1, 100], weekly_max: [1, 100], drop_points: [1, 100], warmup_seconds: [0, 600], min_dwell_minutes: [0, 120] };
const DEFAULT_LIMIT = 95;

const clone = value => JSON.parse(JSON.stringify(value));
const canon = value => Array.isArray(value) ? value.map(canon) : value && typeof value === 'object' ? Object.fromEntries(Object.keys(value).sort().map(key => [key, canon(value[key])])) : value;
const same = (a, b) => JSON.stringify(canon(a)) === JSON.stringify(canon(b));
const when = value => { const date = value ? new Date(value) : null; return !date ? 'never' : Number.isNaN(date.getTime()) ? 'unknown' : date.toLocaleString(); };
const percent = value => typeof value === 'number' ? `${Math.round(value)}%` : '-';

// A text button, or with `glyph` an icon button whose label is also its tooltip.
function button(text, label, onclick, { disabled = false, quiet = true, focus = '', glyph = '', danger = false } = {}) {
  const item = glyph ? iconButton(glyph, label, { danger }) : element('button', quiet ? 'btn' : 'btn btn-primary', text); item.type = 'button'; item.setAttribute('aria-label', label);
  item.disabled = disabled; item.onclick = onclick; if (focus) item.dataset.quotaFocus = focus;
  return item;
}

function numberInput(id, value, label, range, oninput) {
  const input = element('input'); input.type = 'number'; input.id = id; input.setAttribute('aria-label', label);
  input.min = String(range[0]); input.max = String(range[1]); input.step = '1';
  input.value = Number.isFinite(value) ? String(value) : '';
  input.oninput = () => oninput(input.value === '' ? NaN : Number(input.value));
  return input;
}

function describe(decision) {
  const route = decision.from && decision.to ? `${decision.from} → ${decision.to}` : decision.from || decision.to || '';
  const text = ({
    switch: decision.dry_run ? `would switch ${route}` : `switched ${route}`,
    stay: `stay on ${decision.from || 'the active profile'}`,
    hold: 'hold', idle: 'idle', error: 'error',
  })[decision.action] || String(decision.action || '');
  return decision.reason && ['hold', 'error', 'switch'].includes(decision.action) ? `${text}: ${decision.reason}` : text;
}

export class QuotaSettings {
  constructor(source) {
    this.source = source;
    this.node = element('section', 'quota-settings'); this.node.hidden = true;
    this.node.setAttribute('aria-label', 'Automatic account switching');
    this.data = null; this.drafts = {}; this.errors = {}; this.notes = {}; this.open = new Set(); this.busy = new Map(); // tool -> what the server is being asked, while a call is in flight
    this.loadError = ''; this.generation = 0; this.stale = false;
    // A push that arrived while a field had focus left the fields alone; show the server's copy once focus leaves them.
    this.node.addEventListener('focusout', () => setTimeout(() => {
      if (this.stale && !this.editing()) this.render();
    }, 0));
  }
  editing() { return this.node.contains(document.activeElement) && document.activeElement.matches('input, select, textarea'); }

  // Called by the Usages panel after every load: only the owner of an enabled account switcher sees this.
  async sync({ owner, enabled }) {
    if (!owner || !enabled) { this.hide(); return; }
    await this.load();
  }
  hide() { this.generation++; this.node.hidden = true; this.node.replaceChildren(); this.data = null; this.drafts = {}; }

  // Pre-filled defaults (saved: false) are never acted on: Save is open for them even unchanged, so the owner can accept them as they are.
  savable(tool) { return this.dirty(tool) || this.data?.tools?.[tool]?.saved === false; }
  dirty(tool) { return !!this.data?.tools?.[tool] && !!this.drafts[tool] && !same(this.drafts[tool], this.data.tools[tool].config); }

  async load() {
    const generation = ++this.generation;
    let data;
    try { data = await this.source.load(); }
    catch (error) {
      if (generation !== this.generation) return;
      if (error.status === 403 || error.status === 404) { this.hide(); return; } // not an owner, or not served
      this.loadError = error.message || 'Could not load automatic switching';
      if (this.data) this.render();
      return;
    }
    if (generation !== this.generation) return;
    const unchanged = this.data && !this.loadError && same(this.data, data) && !this.node.hidden;
    const editedBefore = new Set(TOOLS.filter(([tool]) => this.dirty(tool)).map(([tool]) => tool)); // judged against the copy the draft was made from
    const configChanged = !!this.data && TOOLS.some(([tool]) => !same(this.data.tools?.[tool]?.config, data.tools?.[tool]?.config) || this.data.tools?.[tool]?.saved !== data.tools?.[tool]?.saved);
    this.loadError = ''; this.data = data;
    if (unchanged) return; // a live push that changed nothing must not rebuild the section under the reader
    for (const [tool] of TOOLS) if (data.tools?.[tool] && !editedBefore.has(tool)) this.drafts[tool] = clone(data.tools[tool].config);
    // The server's status changes on every evaluation. A reader typing in a field keeps their caret and any IME
    // composition: only the server-driven blocks are replaced, never the fields.
    // A focused field whose tool has no typed edits is stale when the configuration changed, so it is rebuilt now; one the
    // owner has edited keeps its element and is brought up to date when focus leaves.
    const focusedTool = document.activeElement?.closest?.('.quota-tool')?.dataset.tool;
    if (!this.node.hidden && this.editing() && (!configChanged || editedBefore.has(focusedTool))) { if (configChanged) this.stale = true; this.patchLive(); return; }
    this.render();
  }

  render(focus = '') {
    this.stale = false;
    const active = this.node.contains(document.activeElement) ? document.activeElement.dataset.quotaFocus || document.activeElement.id : '';
    const head = [element('h4', 'quota-title', 'Automatic account switching'), element('p', 'settings-desc quota-intro', 'The server picks the first profile in each order that is still under its limits and switches to it, the same way the Switch button does. A switch waits until no agent is running a turn. Nothing is switched while dry run is on.')];
    if (this.loadError) { const problem = element('p', 'error', this.loadError); problem.setAttribute('role', 'alert'); head.push(problem); }
    this.node.replaceChildren(...head, ...TOOLS.filter(([tool]) => this.data?.tools?.[tool]).map(([tool, label]) => this.tool(tool, label)));
    this.node.hidden = false;
    this.restoreFocus(focus || active);
  }
  restoreFocus(key) {
    if (!key) return;
    const find = wanted => [...this.node.querySelectorAll('[data-quota-focus], [id]')].find(item => (item.dataset.quotaFocus || item.id) === wanted && !item.disabled);
    let target = find(key);
    if (!target) { // a moved row's button may now be disabled: land on its sibling
      const parts = key.split(':');
      if (parts.length === 3) target = find(`${parts[0]}:${parts[1]}:${parts[2] === 'up' ? 'down' : 'up'}`) || find(`${parts[0]}:${parts[1]}:remove`);
    }
    target?.focus();
  }

  touch(tool) { // a number was edited: refresh only the Save/Discard state, never the field being typed in
    const box = this.node.querySelector(`.quota-tool[data-tool="${tool}"]`);
    if (!box) return;
    const dirty = this.dirty(tool);
    box.querySelector('.quota-save').disabled = !this.savable(tool) || this.busy.has(tool);
    box.querySelector('.quota-discard').disabled = !dirty;
    box.querySelector('.quota-unsaved').hidden = !dirty;
  }
  change(tool, focus = '') { this.errors[tool] = ''; this.notes[tool] = ''; this.render(focus); }

  // The part that follows the server: status lines, the breaker banner and the latched-reset note. A live push
  // replaces only this block and the decision list while the reader is typing, never the fields.
  live(tool, label) {
    const info = this.data.tools[tool], state = info.state || {};
    const block = element('div', 'quota-live');
    const status = element('div', 'quota-status');
    const line = (name, text) => { status.append(element('p', `settings-desc quota-${name}`, text)); };
    if (!info.saved) line('defaults', 'These are suggested defaults built from your profile names. Nothing is saved until you press Save.');
    if (info.config.enabled && info.config.dry_run) line('dry-run', 'Dry run: decisions are recorded below, nothing is switched.');
    line('last', info.last_decision ? `Last decision (${when(info.last_decision.at)}): ${describe(info.last_decision)}` : 'No decision yet.');
    if (state.hold) line('hold', `Current hold: ${state.hold}`);
    line('evaluated', `Last evaluated: ${when(state.evaluated_at)}`);
    block.append(status);
    if (state.tripped) {
      const banner = element('div', 'quota-banner'); banner.setAttribute('role', 'alert');
      banner.append(element('p', 'quota-tripped', `Automatic switching is paused: it switched too often in one hour (${when(state.tripped_at)}). Check the order and the limits, then re-enable it.`),
        button('Re-enable switching', `Re-enable automatic switching for ${label}`, () => this.act(tool, 'reenable'), { quiet: false, disabled: this.busy.has(tool), focus: `${tool}-reenable` }));
      block.append(banner);
    }
    if (state.reset_latched_at) {
      const reset = element('div', 'quota-reset');
      reset.append(element('p', 'settings-desc quota-reset-text', `A weekly reset was seen at ${when(state.reset_latched_at)}${typeof state.weekly_max_seen === 'number' ? ` (the weekly peak was ${percent(state.weekly_max_seen)})` : ''}; the order after the reset is in effect. Re-arm when the next week starts.`),
        button('Re-arm', `Re-arm the reset detection for ${label}`, () => this.act(tool, 'rearm'), { disabled: this.busy.has(tool), focus: `${tool}-rearm` }));
      block.append(reset);
    }
    return block;
  }
  patchLive() {
    for (const [tool, label] of TOOLS) {
      const box = this.node.querySelector(`.quota-tool[data-tool="${tool}"]`), info = this.data?.tools?.[tool];
      if (!box || !info) continue;
      box.querySelector('.quota-live').replaceWith(this.live(tool, label));
      box.querySelector('.quota-decisions').replaceWith(this.decisions(tool, label, info));
    }
  }

  tool(tool, label) {
    const info = this.data.tools[tool], draft = this.drafts[tool];
    const stored = this.data.profiles?.[tool] || [];
    const box = element('div', 'quota-tool'); box.dataset.tool = tool;
    box.append(element('h5', 'quota-tool-title', label));

    box.append(this.live(tool, label));

    const enabled = element('input'); enabled.type = 'checkbox'; enabled.checked = !!draft.enabled; enabled.dataset.quotaFocus = `${tool}-enabled`;
    enabled.onchange = () => { draft.enabled = enabled.checked; this.change(tool, `${tool}-enabled`); };
    const dry = element('input'); dry.type = 'checkbox'; dry.checked = !!draft.dry_run; dry.dataset.quotaFocus = `${tool}-dry-run`;
    dry.onchange = () => { draft.dry_run = dry.checked; this.change(tool, `${tool}-dry-run`); };
    box.append(settingsGroup([
      settingsRow({ label: `Switch ${label} automatically`, desc: `Move ${label} to the first usable profile in the order below when the active one runs out of quota.`, control: enabled, id: `quota-${tool}-enabled`, className: 'settings-row-toggle' }),
      settingsRow({ label: 'Dry run', desc: 'Record what would be switched without switching. Turn this off to switch for real.', control: dry, id: `quota-${tool}-dry-run`, className: 'settings-row-toggle' }),
    ], `${label} switching`));
    box.append(this.order(tool, 'order', draft.profiles, stored, `${label} profile order`));
    box.append(this.protect(tool, label, draft, stored));
    box.append(this.afterReset(tool, label, draft, stored));

    const advanced = element('details', 'quota-advanced'); advanced.open = this.open.has(`${tool}:advanced`);
    advanced.ontoggle = () => { if (advanced.open) this.open.add(`${tool}:advanced`); else this.open.delete(`${tool}:advanced`); };
    advanced.append(element('summary', '', 'Advanced'), settingsGroup([
      settingsRow({ label: 'Warm-up (seconds)', desc: 'How long every usable profile must report fresh usage before the server acts on it. 0 to 600.', control: numberInput(`quota-${tool}-warmup`, draft.warmup_seconds, `${label} warm-up in seconds`, RANGES.warmup_seconds, value => { draft.warmup_seconds = value; this.touch(tool); }), id: `quota-${tool}-warmup` }),
      settingsRow({ label: 'Minimum time between switches (minutes)', desc: 'After an automatic switch the server waits this long before another one. This also delays a switch away from an exhausted account right after a switch. 0 to 120.', control: numberInput(`quota-${tool}-dwell`, draft.min_dwell_minutes, `${label} minimum minutes between switches`, RANGES.min_dwell_minutes, value => { draft.min_dwell_minutes = value; this.touch(tool); }), id: `quota-${tool}-dwell` }),
    ], `${label} advanced`));
    box.append(advanced);

    const dirty = this.dirty(tool);
    const save = button('Save', `Save ${label} automatic switching`, () => this.save(tool), { quiet: false, disabled: !this.savable(tool) || this.busy.has(tool), focus: `${tool}-save` }); save.classList.add('quota-save');
    const discard = button('Discard changes', `Discard ${label} changes`, () => { this.drafts[tool] = clone(info.config); this.change(tool); }, { disabled: !dirty, focus: `${tool}-discard` }); discard.classList.add('quota-discard');
    const unsaved = element('span', 'settings-desc quota-unsaved', 'Unsaved changes'); unsaved.hidden = !dirty;
    const actions = element('div', 'settings-actions quota-actions'); actions.append(save, discard, unsaved);
    const problem = element('p', 'error quota-error', this.errors[tool] || ''); problem.setAttribute('role', 'alert');
    const note = element('p', 'settings-desc quota-note', this.busy.get(tool) || this.notes[tool] || ''); note.setAttribute('role', 'status'); note.tabIndex = -1; note.dataset.quotaFocus = `${tool}-note`;
    box.append(actions, problem, note, this.decisions(tool, label, info));
    return box;
  }

  // The ordered list: one row per entry (name, limits, up, down, remove) and an add row.
  order(tool, kind, entries, stored, groupLabel) {
    const protectedNames = new Set(this.drafts[tool].protected);
    const rows = entries.map((entry, index) => {
      const key = name => `${kind}:${entry.name}:${name}`;
      const session = numberInput(`quota-${tool}-${kind}-${index}-session`, entry.session_max, `${entry.name} session limit (%)`, RANGES.session_max, value => { entry.session_max = value; this.touch(tool); });
      const weekly = numberInput(`quota-${tool}-${kind}-${index}-weekly`, entry.weekly_max, `${entry.name} weekly limit (%)`, RANGES.weekly_max, value => { entry.weekly_max = value; this.touch(tool); });
      const move = delta => { const target = index + delta; [entries[index], entries[target]] = [entries[target], entries[index]]; this.change(tool, key(delta < 0 ? 'up' : 'down')); };
      const controls = element('div', 'quota-entry-controls');
      const field = (text, input) => { const wrap = element('span', 'quota-entry-field'); const caption = element('span', 'quota-entry-caption', text); caption.setAttribute('aria-hidden', 'true'); wrap.append(caption, input); return wrap; };
      controls.append(field('Session %', session), field('Weekly %', weekly),
        button('', `Move ${entry.name} up`, () => move(-1), { disabled: index === 0, focus: key('up'), glyph: 'arrow-up' }),
        button('', `Move ${entry.name} down`, () => move(1), { disabled: index === entries.length - 1, focus: key('down'), glyph: 'arrow-down' }),
        button('', `Remove ${entry.name} from the order`, () => { entries.splice(index, 1); this.change(tool, `${kind}-add`); }, { focus: key('remove'), glyph: 'close', danger: true }));
      const missing = !stored.includes(entry.name);
      return settingsRow({ label: `${index + 1}. ${entry.name}${missing ? ' (not found)' : ''}`, desc: missing ? 'This profile no longer exists, so it is skipped. Remove it or restore the profile.' : `Switch away from ${entry.name} once its session or weekly usage reaches these limits.`, control: controls, id: `quota-${tool}-${kind}-${index}-row`, className: 'settings-row-stack' });
    });
    const available = stored.filter(name => !entries.some(entry => entry.name === name) && !protectedNames.has(name));
    const select = element('select'); select.id = `quota-${tool}-${kind}-add`; select.dataset.quotaFocus = `${kind}-add`;
    const placeholder = element('option', '', available.length ? 'Choose a profile' : 'No other profiles'); placeholder.value = ''; select.append(placeholder);
    for (const name of available) { const option = element('option', '', name); option.value = name; select.append(option); }
    select.disabled = !available.length;
    const add = button('Add', `Add the chosen profile to the ${groupLabel.toLowerCase()}`, () => { if (!select.value) return; entries.push({ name: select.value, session_max: DEFAULT_LIMIT, weekly_max: DEFAULT_LIMIT }); this.change(tool, `${kind}-add`); }, { disabled: true });
    select.onchange = () => { add.disabled = !select.value; };
    const control = element('div', 'quota-add-controls'); control.append(select, add);
    rows.push(settingsRow({ label: 'Add profile', desc: 'Profiles in the store that are not in this order and not protected.', control, id: `quota-${tool}-${kind}-add` }));
    return settingsGroup(rows, groupLabel);
  }

  protect(tool, label, draft, stored) {
    const names = [...new Set([...stored, ...draft.protected])];
    const rows = names.map((name, index) => {
      const box = element('input'); box.type = 'checkbox'; box.checked = draft.protected.includes(name); box.dataset.quotaFocus = `protect:${name}`;
      box.onchange = () => {
        draft.protected = draft.protected.filter(item => item !== name);
        if (box.checked) {
          draft.protected.push(name);
          draft.profiles = draft.profiles.filter(entry => entry.name !== name);
          if (draft.after_reset) {
            draft.after_reset.profiles = draft.after_reset.profiles.filter(entry => entry.name !== name);
            if (draft.after_reset.profile === name) draft.after_reset.profile = draft.profiles[0]?.name || '';
          }
        }
        this.change(tool, `protect:${name}`);
      };
      return settingsRow({ label: `Protect ${name}`, desc: `Never switch to or away from ${name} while it is active, and keep it out of the orders. For a profile parked on purpose.`, control: box, id: `quota-${tool}-protect-${index}`, className: 'settings-row-toggle' });
    });
    if (!rows.length) return element('p', 'settings-desc quota-none', `There are no ${label} profiles to protect yet.`);
    return settingsGroup(rows, `${label} protected profiles`);
  }

  afterReset(tool, label, draft, stored) {
    const toggle = element('input'); toggle.type = 'checkbox'; toggle.checked = !!draft.after_reset; toggle.dataset.quotaFocus = `${tool}-after-reset`;
    toggle.onchange = () => { draft.after_reset = toggle.checked ? { profile: draft.profiles[0]?.name || '', drop_points: 20, profiles: clone(draft.profiles) } : null; this.change(tool, `${tool}-after-reset`); };
    const rows = [settingsRow({ label: 'Use a different order after a reset', desc: 'Watch one profile’s weekly usage; when it drops by the chosen number of points the server treats the week as reset and switches to the order below. Re-arm it by hand.', control: toggle, id: `quota-${tool}-after-reset`, className: 'settings-row-toggle' })];
    const wrap = element('div', 'quota-after-reset');
    if (draft.after_reset) {
      const after = draft.after_reset;
      const watched = element('select'); watched.id = `quota-${tool}-watched`; watched.dataset.quotaFocus = `${tool}-watched`;
      const choices = [...new Set([...draft.profiles.map(entry => entry.name), after.profile].filter(Boolean))];
      for (const name of choices) { const option = element('option', '', name); option.value = name; watched.append(option); }
      watched.value = after.profile; watched.onchange = () => { after.profile = watched.value; this.touch(tool); };
      rows.push(settingsRow({ label: 'Watched profile', desc: 'The profile whose weekly usage is watched for a reset.', control: watched, id: `quota-${tool}-watched` }),
        settingsRow({ label: 'Drop (points)', desc: 'How far the weekly usage must fall below its highest value to count as a reset. 1 to 100.', control: numberInput(`quota-${tool}-drop`, after.drop_points, `${label} reset drop in percentage points`, RANGES.drop_points, value => { after.drop_points = value; this.touch(tool); }), id: `quota-${tool}-drop` }));
      wrap.append(settingsGroup(rows, `${label} reset detection`), this.order(tool, 'after', after.profiles, stored, `${label} order after a reset`));
    } else wrap.append(settingsGroup(rows, `${label} reset detection`));
    return wrap;
  }

  decisions(tool, label, info) {
    const details = element('details', 'quota-decisions'); details.open = this.open.has(`${tool}:decisions`);
    details.ontoggle = () => { if (details.open) this.open.add(`${tool}:decisions`); else this.open.delete(`${tool}:decisions`); };
    const list = Array.isArray(info.decisions) ? info.decisions : [];
    details.append(element('summary', '', `Recent decisions (${list.length})`));
    if (!list.length) { details.append(element('p', 'settings-desc', 'Nothing has been decided yet.')); return details; }
    const items = element('ol', 'quota-decision-list');
    for (const decision of list) {
      const item = element('li', 'quota-decision'); item.dataset.action = String(decision.action || '').replace(/[^a-z]/g, '');
      item.append(element('span', 'quota-decision-text', `${when(decision.at)}: ${describe(decision)}${decision.dry_run && decision.action === 'switch' ? '' : decision.dry_run ? ' (dry run)' : ''}`));
      const profiles = Object.entries(decision.profiles || {}).map(([name, value]) => `${name}: ${value?.status || '?'}, session ${percent(value?.session)}, weekly ${percent(value?.weekly)}`);
      if (profiles.length) item.append(element('span', 'settings-desc quota-decision-profiles', profiles.join('; ')));
      items.append(item);
    }
    details.append(items);
    return details;
  }

  numbers(draft) {
    const bad = (key, value) => !Number.isInteger(value) || value < RANGES[key][0] || value > RANGES[key][1];
    const range = key => `${RANGES[key][0]} to ${RANGES[key][1]}`;
    for (const entry of [...draft.profiles, ...(draft.after_reset?.profiles || [])]) {
      if (bad('session_max', entry.session_max) || bad('weekly_max', entry.weekly_max)) return `The limits for ${entry.name} must be whole numbers from ${range('session_max')}.`;
    }
    if (draft.enabled && draft.after_reset && draft.after_reset.profiles.length < 2) return 'The order after the reset needs at least two profiles to turn switching on.';
    if (draft.after_reset && bad('drop_points', draft.after_reset.drop_points)) return `The reset drop must be a whole number from ${range('drop_points')}.`;
    if (bad('warmup_seconds', draft.warmup_seconds)) return `Warm-up must be a whole number of seconds from ${range('warmup_seconds')}.`;
    if (bad('min_dwell_minutes', draft.min_dwell_minutes)) return `The minimum time between switches must be a whole number of minutes from ${range('min_dwell_minutes')}.`;
    return '';
  }

  async save(tool) {
    const problem = this.numbers(this.drafts[tool]);
    if (problem) { this.errors[tool] = problem; this.notes[tool] = ''; this.render(); return; }
    await this.run(tool, () => this.source.save(tool, this.drafts[tool]), 'Saved.', 'Saving… (waits for a running switch)');
  }
  async act(tool, action) { await this.run(tool, () => this.source[action](tool), action === 'rearm' ? 'Re-armed.' : 'Re-enabled.', 'Applying… (waits for a running switch)', true); }

  // The server takes the same lock a running switch holds, so a save, re-arm or re-enable can wait for that switch: say so while it does.
  async run(tool, call, done, pending, keepDraft = false) {
    if (this.busy.has(tool)) return;
    this.busy.set(tool, pending); this.errors[tool] = ''; this.notes[tool] = ''; this.render();
    try {
      const result = await call();
      this.data.tools[tool] = result;
      if (!keepDraft || !this.dirty(tool)) this.drafts[tool] = clone(result.config);
      this.notes[tool] = done;
    } catch (error) { this.errors[tool] = error.message || 'The change was not accepted'; }
    finally { this.busy.delete(tool); this.render(`${tool}-note`); }
  }
}
