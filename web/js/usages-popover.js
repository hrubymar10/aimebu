import { element, creditsText, percent, windowLabel, usageSparkline, sparkFigure, paceExplanation, age, countdown, shown, staleResetText, USAGE_LEGEND } from './usage-format.js';
import { focusPopover, returnFocus } from './popover-focus.js';
import { expiryBadge } from './usage-expiry.js';
import { mergeAlerts, staleAge } from './usage-alerts.js';
import { confirmDialog } from './dialogs.js';
import { html, render, effect } from './ui.js';
import { UsagesStore } from './pages/usages/store.js';
import { UsagesVariantB } from './pages/usages/variant-b.js';
import { UsagesVariantC } from './pages/usages/variant-c.js';
import { usagesVariant } from './pages/usages/variant.js';

// A glanceable overview of every configured provider and profile, so usage can be checked
// without opening Settings. Read-only: all management stays in Settings -> Usages.
const groupKey = 'aimebu2.usages.group';
const PROVIDER_MASKS = new Set(['codex', 'claude-code', 'github-copilot', 'mistral', 'ollama-cloud']);
const TOOLS = { 'claude-code': 'claude', codex: 'codex' };


export class UsagesPopover {
  // canChange says whether this person may change usages (the owner): an admin reads them, and the refresh, which asks the providers again, is the owner's.
  constructor(source, openSettings, canChange = () => true) {
    this.source = source; this.openSettings = openSettings; this.canChange = canChange;
    this.toggle = document.querySelector('#usages-toggle');
    this.popover = document.querySelector('#usages-popover');
    // The panel is a column of the chat, not a floating popover: it sits where the members column is.
    this.chat = document.querySelector('.chat'); this.chat.append(this.popover);
    this.body = this.popover.querySelector('.usages-popover-body');
    this.ticket = 0;
    // Variants B and C of the A/B/C choice in the top bar are the Usages page's own components, drawn here from a store of this panel's own.
    this.alt = element('div', 'usages-popover-alt'); this.alt.hidden = true; this.body.after(this.alt);
    this.store = new UsagesStore({ source, section: this.alt, isOpen: () => !this.popover.hidden, isShown: () => !this.popover.hidden });
    this.variant = usagesVariant.peek();
    effect(() => { const next = usagesVariant.value; if (next === this.variant) return; this.variant = next; this.ticket++; if (!this.popover.hidden) this.load(); else this.syncVariant(); });
    this.toggle.setAttribute('aria-haspopup', 'dialog'); this.toggle.setAttribute('aria-expanded', 'false');
    this.toggle.onclick = event => { event.stopPropagation(); if (this.popover.hidden) this.open(); else this.close(); };
    this.refreshButton = this.popover.querySelector('.usages-popover-refresh');
    this.refreshButton.onclick = () => this.refresh();
    // Cooldown and failure hints from Refresh live outside the body, which a reload replaces.
    this.status = element('p', 'usages-popover-note usages-popover-status'); this.status.setAttribute('role', 'status'); this.status.hidden = true;
    this.popover.querySelector('header').after(this.status);
    this.groupButton = this.popover.querySelector('.usages-popover-group'); this.groupBy = localStorage.getItem(groupKey) === 'active' ? 'active' : 'providers'; this.usage = null; this.state = null; this.tokens = [];
    this.groupButton.onclick = () => { this.groupBy = this.groupBy === 'active' ? 'providers' : 'active'; try { localStorage.setItem(groupKey, this.groupBy); } catch { /* storage may be denied */ } this.syncGroupButton(); if (this.usage) this.render(this.usage); };
    this.syncGroupButton();
    this.popover.querySelector('.usages-popover-close').onclick = () => { this.close(); this.toggle.focus(); };
    document.addEventListener('keydown', event => { if (event.key === 'Escape' && !this.popover.hidden) { this.close(); this.toggle.focus(); } });
  }
  // Nothing of what was drawn stays after a sign-out: the tiles, the cached usage, switcher state and credential tokens.
  syncVariant() { const alt = this.variant !== 'a'; this.body.hidden = alt; this.alt.hidden = !alt; this.groupButton.hidden = alt; this.popover.dataset.variant = this.variant; if (!alt) render(null, this.alt); }
  reset() { this.store.forget(); render(null, this.alt); this.ticket++; this.usage = null; this.state = null; this.tokens = []; this.refreshSec = undefined; this.body.replaceChildren(); this.status.hidden = true; }
  open() { this.syncRefresh(); this.status.hidden = true; this.popover.hidden = false; this.chat.classList.add('usages-open'); this.toggle.setAttribute('aria-expanded', 'true'); focusPopover(this.popover); this.load(); }
  close() { this.ticket++; returnFocus(this.popover, this.toggle); this.popover.hidden = true; this.chat.classList.remove('usages-open'); this.toggle.setAttribute('aria-expanded', 'false'); }
  // Ask the server to fetch fresh snapshots (rate limited to one forced refresh per cooldown
  // window, shared with Settings), then re-read the overview the usual way.
  syncRefresh() { const allowed = !!this.canChange(); this.refreshButton.disabled = !allowed; this.refreshButton.title = allowed ? 'Refresh usages' : 'Only the owner can refresh usages'; }
  async refresh() {
    if (this.refreshButton.disabled) return;
    this.refreshButton.disabled = true; this.status.hidden = true;
    try {
      await this.source.refresh();
    } catch (error) {
      this.status.textContent = error.status === 429 && error.retryAfter ? `Refreshed moments ago; try again in ${error.retryAfter}s` : error.message || 'Refresh failed';
      this.status.hidden = false;
    }
    try { await this.load(); }
    finally { this.syncRefresh(); }
  }
  // Live push from the event stream: re-read in place while the popover is open. A closed
  // popover loads fresh on its next open, so there is nothing to keep current.
  changed() { if (!this.popover.hidden) this.load(); }
  async load() {
    const ticket = ++this.ticket;
    this.syncVariant();
    if (this.variant !== 'a') return this.loadAlt(ticket);
    if (!this.body.children.length) this.body.replaceChildren(element('p', 'usages-popover-note', 'Loading…'));
    try {
      // The switcher state and the credential tokens are owner data that may be absent; the usage numbers never depend on them.
      const [usage, state, tokens] = await Promise.all([this.source.list(), this.source.switcher?.().catch(() => null), this.source.credentialTargets?.().catch(() => null)]);
      if (ticket === this.ticket) { this.state = state; this.tokens = tokens?.targets || []; this.render(usage); }
    } catch (error) {
      if (ticket === this.ticket) this.body.replaceChildren(element('p', 'usages-popover-note', error.status === 403 ? 'Usage details are limited to the instance owner.' : error.message || 'Could not load usage snapshots'));
    }
  }
  async loadAlt(ticket) {
    this.store.owner.value = !!this.canChange();
    try {
      await this.store.load();
      if (ticket !== this.ticket) return;
      this.store.setError(''); this.alt.querySelector('.usages-popover-note')?.remove();
      render(this.variant === 'c' ? html`<${UsagesVariantC} store=${this.store} />` : html`<${UsagesVariantB} store=${this.store} />`, this.alt);
    } catch (error) {
      if (ticket === this.ticket) { render(null, this.alt); this.alt.replaceChildren(element('p', 'usages-popover-note', error.status === 403 ? 'Usage details are limited to the instance owner.' : error.message || 'Could not load usage snapshots')); }
    }
  }
  syncGroupButton() {
    const activeFirst = this.groupBy === 'active', label = activeFirst ? 'Group usages by provider' : 'Show active profiles first';
    this.groupButton.classList.toggle('active', activeFirst); this.groupButton.setAttribute('aria-pressed', String(activeFirst)); this.groupButton.setAttribute('aria-label', label); this.groupButton.title = label;
  }
  render(usage) {
    this.usage = usage;
    const usedDisplay = usage?.settings?.percent_display === 'used';
    const providers = [...(usage?.providers || [])].filter(provider => provider.available && provider.enabled).sort((a, b) => (a.order_number || 0) - (b.order_number || 0));
    if (!providers.length) {
      const empty = element('div', 'usages-empty'); empty.append(element('div', '', 'No usage providers enabled.'));
      const open = element('button', 'btn usages-popover-settings', 'Open Settings → Usages'); open.type = 'button'; open.onclick = () => { this.close(); this.openSettings(); };
      empty.append(open); this.body.replaceChildren(empty); return;
    }
    const groups = providers.map(provider => ({ provider, profiles: provider.profiles?.length ? provider.profiles : [{ profile_name: 'local', active: true }] }));
    // One alert for all the tools whose refresh is paused, in plain words, with the reasons folded away: the box was most of the first screen.
    const alerts = mergeAlerts(usage?.credential_alerts).map(alert => {
      const node = element('div', `usages-credential-alert usages-credential-alert-${String(alert.kind || '').replace(/[^a-z_]/g, '')}`); node.append(element('p', '', alert.text));
      if (alert.details.length) { const more = element('details', ''); more.append(element('summary', '', 'Details'), ...alert.details.map(line => element('p', '', line))); node.append(more); }
      return node;
    });
    this.refreshSec = usage?.settings?.refresh_interval_sec;
    const tiles = list => list.filter(group => group.profiles.length).map(group => this.tile(group.provider, group.profiles, usedDisplay));
    const nodes = [];
    if (alerts.length) { const box = element('div', 'usages-credential-alerts'); box.setAttribute('role', 'alert'); box.append(...alerts); nodes.push(box); }
    if (this.groupBy === 'active') {
      const primary = groups.map(group => ({ ...group, profiles: group.profiles.filter(profile => profile.active) })), secondary = groups.map(group => ({ ...group, profiles: group.profiles.filter(profile => !profile.active) }));
      nodes.push(...tiles(primary));
      if (secondary.some(group => group.profiles.length)) { const divider = element('div', 'usages-group-divider'); divider.setAttribute('role', 'separator'); divider.setAttribute('aria-label', 'Other profiles'); nodes.push(divider); }
      nodes.push(...tiles(secondary));
    } else nodes.push(...tiles(groups));
    nodes.push(element('p', 'usages-popover-note usages-legend', USAGE_LEGEND));
    const list = element('div', 'usages-sidebar-list'); list.append(...nodes);
    this.body.replaceChildren(list);
  }
  tile(provider, profiles, usedDisplay) {
    const group = element('section', 'usages-popover-provider usages-provider-tile'); group.dataset.provider = provider.provider_name;
    const heading = element('div', 'usages-provider-heading'), title = element('div', 'usages-provider-title');
    const icon = element('span', 'usages-provider-icon'); icon.setAttribute('aria-hidden', 'true');
    if (PROVIDER_MASKS.has(provider.provider_name)) icon.append(element('span', `usages-provider-mask usages-provider-mask-${provider.provider_name}`)); else icon.textContent = 'AI';
    const text = element('div', '');
    text.append(element('h4', 'usages-popover-provider-name usages-provider-name', provider.label || provider.provider_name));
    const oldest = profiles.map(profile => profile.last_refresh_at).filter(Boolean).sort((a, b) => Date.parse(a) - Date.parse(b))[0];
    text.append(element('div', 'usages-provider-updated', oldest ? `Updated ${age(oldest)} ago` : String(profiles[0]?.status || 'Not configured').replaceAll('_', ' ')));
    title.append(icon, text); heading.append(title); group.append(heading);
    const sorted = this.groupBy === 'active' ? profiles : [...profiles].sort((a, b) => Number(!!b.active) - Number(!!a.active) || String(a.profile_name).localeCompare(String(b.profile_name)));
    for (const profile of sorted) group.append(this.profile(profile, usedDisplay, provider.provider_name));
    return group;
  }
  profile(profile, usedDisplay, providerName = '') {
    const age = staleAge(profile.last_refresh_at, this.refreshSec);
    const node = element('div', `usages-popover-profile switcher-tile-profile${profile.active ? ' is-active' : ''}${age ? ' is-stale' : ''}`);
    const head = element('div', 'usages-popover-profile-head switcher-tile-head');
    head.append(element('strong', 'usages-popover-profile-name switcher-tile-name', profile.profile_name || 'local'));
    const tool = TOOLS[providerName] || '';
    const badge = expiryBadge(this.tokens.find(target => target.tool === tool && target.profile === profile.profile_name && target.role === 'store'));
    if (badge) head.append(badge);
    if (this.canSwitch(tool) && !profile.active) head.append(this.switchPill(tool, profile));
    if (profile.active) head.append(element('span', 'usage-profile-active switcher-tile-active', 'Active'));
    else head.append(element('span', 'usage-profile-inactive', 'Inactive')); // said in words, not only by the colour of the card's edge
    if (profile.plan) head.append(element('span', 'usage-profile-plan switcher-tile-plan', profile.plan));
    node.append(head);
    const windows = profile.windows || [];
    if (windows.length) { const list = element('div', 'usage-windows'); list.append(...windows.map(window => this.window(window, usedDisplay, profile.last_refresh_at, !!age))); node.append(list); }
    else if (profile.has_credentials === false) node.append(element('div', 'usages-empty usages-empty-compact', 'Needs login'));
    if (profile.credits) node.append(element('div', 'usage-profile-credits usages-credits-row', creditsText(profile.credits)));
    const note = profile.status === 'token_expired' || profile.status === 'expired' ? 'Stale – token expired' : profile.stale ? 'Stale snapshot' : profile.status === 'ok' ? '' : !windows.length ? (profile.status || 'No snapshot').replaceAll('_', ' ') : '';
    if (note) node.append(element('div', 'usage-profile-status usages-error-line', note));
    if (age) {
      const line = element('div', 'usage-profile-stale', `Stale, last update ${age} ago`);
      if (this.canChange()) { const button = element('button', 'btn usage-refresh', 'Refresh'); button.type = 'button'; button.onclick = () => this.refresh(); line.append(' ', button); }
      node.append(line);
    }
    return node;
  }
  // v1's SWITCH pill: shown on an inactive profile when the account switcher is on and the tool can switch.
  canSwitch(tool) { return !!(tool && this.state?.enabled && !this.switching && (this.state.eligibility || []).find(item => item.tool === tool)?.eligible !== false); }
  switchPill(tool, profile) {
    const pill = element('button', 'btn switcher-switch-pill', 'Switch'); pill.type = 'button'; pill.dataset.tool = tool; pill.dataset.profile = profile.profile_name;
    pill.title = `Switch to ${profile.profile_name}`; pill.setAttribute('aria-label', pill.title);
    pill.onclick = () => this.switchTo(tool, profile);
    return pill;
  }
  async switchTo(tool, profile) {
    const affected = (this.state?.affected_agents || {})[tool] || [];
    const impact = affected.length ? `Affected agents: ${affected.map(agent => agent.id).join(', ')}.` : 'No affected agents.';
    if (!await confirmDialog(`${impact}${profile.has_credentials === false ? ' This profile needs a new login.' : ''} Switch to ${profile.profile_name}?`, { title: 'Switch account', confirm: 'Switch' })) return;
    this.switching = true; this.status.hidden = true;
    try { await this.source.use(tool, profile.profile_name); }
    catch (error) { this.status.textContent = error.message || 'Account switch failed'; this.status.hidden = false; }
    finally { this.switching = false; }
    await this.load();
  }
  // v1's window row: the label, the bar with the expected-pace tick, "N% left" with the reset countdown, then the pace line.
  // A stale profile (data older than twice the refresh interval) keeps its numbers but shows no pace claim and no live countdown.
  window(window, usedDisplay, fetchedAt, stale = false) {
    const row = element('div', 'usage-window usages-window-row');
    const used = percent(window.percent_used), value = usedDisplay ? used : 100 - used;
    const top = element('div', 'usages-window-top'); top.append(element('span', '', windowLabel(window.key))); row.append(top);
    const track = element('div', 'usage-track usages-progress');
    track.setAttribute('role', 'progressbar'); track.setAttribute('aria-label', `${windowLabel(window.key)} ${usedDisplay ? 'used' : 'left'}`);
    track.setAttribute('aria-valuenow', String(Math.round(value))); track.setAttribute('aria-valuemin', '0'); track.setAttribute('aria-valuemax', '100');
    const fill = element('span', 'usage-fill'); fill.style.width = `${value}%`; track.append(fill);
    const pace = stale ? null : window.pace, state = pace?.state === 'reserve' ? 'reserve' : pace?.state === 'deficit' ? 'deficit' : 'on-track';
    if (pace && Number.isFinite(Number(pace.expected_percent))) {
      const tick = element('i', `usage-pace-marker usages-pace-tick usages-pace-tick--${state}`); tick.style.left = `${usedDisplay ? percent(pace.expected_percent) : 100 - percent(pace.expected_percent)}%`; tick.title = 'Expected pace'; track.append(tick);
    }
    const meta = element('div', 'usages-window-meta usage-window-foot');
    meta.append(element('strong', '', `${shown(value)} ${usedDisplay ? 'used' : 'left'}`), element('span', '', window.reset_at ? (stale ? staleResetText(window.reset_at) : `Resets in ${countdown(window.reset_at)}`) : ''));
    const spark = usageSparkline(window, usedDisplay);
    row.append(track, ...(spark ? [sparkFigure(spark, usedDisplay)] : []), meta);
    if (pace) {
      const delta = Math.abs(Number(pace.delta_percent) || 0);
      const label = pace.state === 'reserve' ? `${shown(delta)} in reserve` : pace.state === 'deficit' ? `${shown(delta)} in deficit` : '';
      let eta = '';
      if (pace.lasts_to_reset) eta = 'Lasts until reset';
      else if (pace.eta_seconds != null) eta = `Runs out in ${countdown(new Date((Number.isFinite(Date.parse(fetchedAt)) ? Date.parse(fetchedAt) : Date.now()) + pace.eta_seconds * 1000).toISOString())}`;
      if (label || eta) {
        const line = element('div', `usages-pace-text usages-pace-text--${state} usage-pace-${pace.state || 'on_track'}`);
        if (label) line.append(element('span', '', label));
        if (label && eta) line.append(element('span', 'usages-pace-sep', '·'));
        if (eta) line.append(element('span', '', eta));
        const explanation = paceExplanation(pace, usedDisplay);
        line.title = explanation; line.tabIndex = 0; line.setAttribute('aria-label', `${[label, eta].filter(Boolean).join(' · ')}. ${explanation}`);
        row.append(line);
      }
    }
    return row;
  }
}
