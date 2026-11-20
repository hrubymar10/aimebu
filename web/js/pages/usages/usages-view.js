import { html, signal, useState, useEffect, useRef, useLayoutEffect } from '../../ui.js';
import { IconButton } from '../../components/icon-button.js';
import { promptDialog } from '../../dialogs.js';
import { creditsText, percent, windowLabel, paceExplanation, sparkGeometry, dailyTargetText, countdown, shown, staleResetText, age as ageOf, USAGE_LEGEND } from '../../usage-format.js';
import { mergeAlerts, staleAge } from '../../usage-alerts.js';
import { expiryBadge } from '../../usage-expiry.js';
import { toolOf, isSwitchable, sortProviders, providerConfigured, canEnable, movedOrder, profileKey, profileStatusLine, switchView, affectedText } from './model.js';
import { CopilotSetup, MistralSetup, OllamaSetup } from './provider-setup.js';
import { profileActions } from './profile-actions.js';

// The Usages view (shared by Settings → Usages and the side panel): v1's usages panel, as it was drawn there (a tile per provider, its accounts one under the other, each limit a bar with a
// number under it), carrying what v2 added in the places v1 would have put it: the expected-pace tick on the bar, a thin 48-hour line under it, the
// pace words under the numbers, credits under the account, who is active in the account's own line. What changes a provider or an account hides behind one
// gear on the tile, so the tiles stay as quiet as v1's.
const groupKey = 'aimebu2.usages.group';
// The group-by choice is one signal, so the panel header's button and the Usages page's own read and change the same thing.
const readGroup = () => { try { return localStorage.getItem(groupKey) === 'active' ? 'active' : 'providers'; } catch { return 'providers'; } };
export const usagesGroup = signal(readGroup());
export function toggleUsagesGroup() {
  const next = usagesGroup.value === 'active' ? 'providers' : 'active';
  try { localStorage.setItem(groupKey, next); } catch { /* storage may be denied */ }
  usagesGroup.value = next;
}
export const groupLabel = groupBy => (groupBy === 'active' ? 'Group usages by provider' : 'Show active profiles first');
const MASKS = new Set(['codex', 'claude-code', 'github-copilot', 'mistral', 'ollama-cloud']);
const time = date => date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
const GROUP_ICON = html`<svg width="16" height="16" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true"><path fill-rule="evenodd" clip-rule="evenodd" d="M1.5 1h2v1H2v12h1.5v1h-2l-.5-.5v-13l.5-.5zm6 6h-2L5 6.5v-2l.5-.5h2l.5.5v2l-.5.5zM6 6h1V5H6v1zm7.5 1h-3l-.5-.5v-3l.5-.5h3l.5.5v3l-.5.5zM11 6h2V4h-2v2zm-3.5 6h-2l-.5-.5v-2l.5-.5h2l.5.5v2l-.5.5zM6 11h1v-1H6v1zm7.5 2h-3l-.5-.5v-3l.5-.5h3l.5.5v3l-.5.5zM11 12h2v-2h-2v2zm-1-2H8v1h2v-1zm0-5H8v1h2V5z"/></svg>`;

function WindowRow({ window, usedDisplay, fetchedAt, stale }) {
  const used = percent(window.percent_used), value = usedDisplay ? used : 100 - used;
  const label = windowLabel(window.key), side = usedDisplay ? 'used' : 'left';
  const pace = stale ? null : window.pace, expected = Number(pace?.expected_percent);
  const state = pace?.state === 'reserve' ? 'reserve' : pace?.state === 'deficit' ? 'deficit' : 'on-track';
  const spark = sparkGeometry(window, usedDisplay), target = pace ? dailyTargetText(window) : '';
  let paceLabel = '', eta = '';
  if (pace) {
    const delta = Math.abs(Number(pace.delta_percent) || 0);
    paceLabel = pace.state === 'reserve' ? `${shown(delta)} in reserve` : pace.state === 'deficit' ? `${shown(delta)} in deficit` : '';
    if (pace.lasts_to_reset) eta = 'Lasts until reset';
    else if (pace.eta_seconds != null) eta = `Runs out in ${countdown(new Date((Number.isFinite(Date.parse(fetchedAt)) ? Date.parse(fetchedAt) : Date.now()) + pace.eta_seconds * 1000).toISOString())}`;
  }
  const explanation = pace ? paceExplanation(pace, usedDisplay) : '', paceText = [paceLabel, eta].filter(Boolean).join(' · ');
  return html`<div class="usages-window-row usages-b-window">
    <div class="usages-window-top"><span>${label}</span></div>
    <div class="usages-progress usages-b-track" role="progressbar" aria-label=${`${label} ${side}`} aria-valuenow=${String(Math.round(value))} aria-valuemin="0" aria-valuemax="100">
      <span style=${{ width: `${value}%` }}></span>
      ${pace && Number.isFinite(expected) && html`<i class=${`usages-pace-tick usages-pace-tick--${state}`} style=${{ left: `${usedDisplay ? percent(expected) : 100 - percent(expected)}%` }} title="Expected pace"></i>`}
    </div>
    ${spark && html`<svg class="usages-b-spark" viewBox=${spark.viewBox} preserveAspectRatio="none" role="img" aria-label=${spark.label}><title>${`${spark.label} (the thin line)`}</title>
      <polyline points=${spark.points} fill="none" vector-effect="non-scaling-stroke" /><circle cx=${spark.end[0]} cy=${spark.end[1]} r="1.6" /></svg>`}
    <div class="usages-window-meta"><strong>${`${shown(value)} ${side}`}</strong><span>${window.reset_at ? (stale ? staleResetText(window.reset_at) : `Resets in ${countdown(window.reset_at)}`) : ''}</span></div>
    ${paceText && html`<div class=${`usages-pace-text usages-pace-text--${state}`} title=${explanation} tabindex="0" aria-label=${`${paceText}. ${explanation}`}>
      ${paceLabel && html`<span>${paceLabel}</span>`}${paceLabel && eta && html`<span class="usages-pace-sep">·</span>`}${eta && html`<span>${eta}</span>`}</div>`}
    ${target && html`<div class="usages-daily-target">${target}</div>`}
  </div>`;
}

function ProfileRow({ store, provider, profile, state, usedDisplay, refreshSec, tool, affected, eligible, hasActive, token, manage, readOnly, reauth }) {
  const name = profile.profile_name || 'local';
  const switchable = isSwitchable(tool);
  const configured = (state.profiles || []).some(item => item.tool === tool && item.name === profile.profile_name);
  const view = switchView({ switchable, enabled: state.enabled, owner: state.owner, configured, profile, hasActive, eligible });
  const stale = staleAge(profile.last_refresh_at, refreshSec);
  const status = profileStatusLine(profile, time), credits = profile.credits ? creditsText(profile.credits) : '';
  const { use, installAnyway, rename, remove } = profileActions({ store, tool, profile, affected });
  const badge = expiryBadge(token);
  const managed = manage && switchable && state.enabled && state.owner && configured;
  const windows = profile.windows || [];
  return html`<div class=${`switcher-tile-profile usages-b-profile${profile.active ? ' is-active' : ''}${stale ? ' is-stale' : ''}`} data-profile=${name}>
    <div class="switcher-tile-head">
      <span class="switcher-tile-name" title=${badge?.title || ''}>${name}</span>
      ${badge && html`<span class=${badge.className} title=${badge.title} data-level=${badge.dataset.level} dangerouslySetInnerHTML=${{ __html: badge.innerHTML }}></span>`}
      ${!view.hidden && html`<${IconButton} name="switch" label=${view.title || `Switch to ${name}`} className="usages-b-switch" disabled=${view.disabled} onClick=${use} />`}
      ${reauth && !readOnly && profile.has_credentials && !view.hidden && html`<${IconButton} name="key-off" label="Install anyway: use the stored credentials without refreshing them" className="usages-b-install-anyway" disabled=${view.disabled} onClick=${installAnyway} />`}
      ${managed && html`<${IconButton} name="edit" label=${`Rename ${name}`} className="usages-b-rename" onClick=${rename} />`}
      ${managed && html`<${IconButton} name="trash" label=${`Delete ${name}`} danger className="usages-b-remove" onClick=${remove} />`}
      ${profile.active ? html`<span class="switcher-tile-active">Active</span>` : html`<span class="usages-b-inactive">Inactive</span>`}
      ${profile.plan && html`<span class="switcher-tile-plan" title=${profile.email ? `${profile.plan} · ${profile.email}` : profile.plan}>${profile.plan}</span>`}
    </div>
    ${windows.length ? windows.map((window, index) => html`<${WindowRow} window=${window} usedDisplay=${usedDisplay} fetchedAt=${profile.last_refresh_at} stale=${!!stale} key=${`${window.key}:${index}`} />`)
      : profile.has_credentials === false ? html`<div class="usages-empty usages-empty-compact">Needs login</div>` : html`<div class="usages-empty usages-empty-compact">No usage data yet.</div>`}
    ${credits && html`<div class="usages-credits-row"><span>${credits}</span></div>`}
    ${status && html`<div class="usages-error-line usages-b-status">${status}</div>`}
    ${stale && html`<div class="usages-stale-line">Stale, last update ${stale} ago${state.owner && !readOnly && html` <${IconButton} name="refresh" label="Refresh now" className="usages-b-refresh" onClick=${() => store.mutateUsage(() => store.source.refresh(provider.provider_name))} />`}</div>`}
  </div>`;
}

function Tile({ store, provider, profiles, order, providers, state, usedDisplay, refreshSec, tokens, revision, readOnly, alerts }) {
  const key = provider.provider_name, tool = toolOf(key);
  const [manage, setManage] = useState(false);
  const refocus = useRef(null);
  // A reorder redraws the list and carries the pressed button with its tile; a browser that cannot move a focused node drops focus. Put it back.
  useLayoutEffect(() => {
    const pending = refocus.current;
    if (!pending) return;
    refocus.current = null;
    const button = pending.node;
    if (!button.isConnected) return;
    // The moved tile may land a screen away: keep the pressed button where it was in the settings section, so the order can be walked click by click.
    const section = button.closest('.settings-section');
    if (section) section.scrollTop += button.getBoundingClientRect().top - pending.top;
    button.focus({ preventScroll: true });
  });
  const owner = !!state.owner && !readOnly, switchable = isSwitchable(tool), configured = providerConfigured(provider);
  const affected = (state.affected_agents || {})[tool] || [];
  const eligible = (state.eligibility || []).find(item => item.tool === tool);
  const hasActive = (state.profiles || []).some(item => item.tool === tool && item.active);
  const oldest = profiles.map(profile => profile.last_refresh_at).filter(Boolean).sort((a, b) => Date.parse(a) - Date.parse(b))[0];
  const updated = oldest ? `Updated ${ageOf(oldest)} ago` : !provider.available ? 'Unavailable' : !provider.enabled ? 'Not configured' : String(profiles[0]?.status || 'No snapshot').replaceAll('_', ' ');
  const profileAction = async (label, method) => {
    const name = await promptDialog('Profile name', { title: label, label: 'Profile name', required: true, confirm: label === 'Add profile' ? 'Add' : 'Import' });
    if (name?.trim()) store.mutateSwitcher(() => store.source[method](tool, name.trim()));
  };
  const move = (delta, event) => { refocus.current = { node: event.currentTarget, top: event.currentTarget.getBoundingClientRect().top }; store.mutateUsage(() => store.source.settings({ provider_order: movedOrder(providers, order, delta) })); };
  return html`<article class=${`usages-provider-tile usages-b-tile${provider.enabled ? '' : ' is-off'}`} data-provider=${key}>
    <div class="usages-provider-heading">
      <div class="usages-provider-title">
        <span class="usages-provider-icon" aria-hidden="true">${MASKS.has(key) ? html`<span class=${`usages-provider-mask usages-provider-mask-${key}`}></span>` : 'AI'}</span>
        <div><h4 class="usages-provider-name">${provider.label || key}</h4><div class="usages-provider-updated">${updated}</div></div>
      </div>
      ${owner && html`<${IconButton} name="more" label=${manage ? `Hide ${provider.label || key} settings` : `Manage ${provider.label || key}`} className="usages-b-manage" aria-pressed=${String(manage)} aria-expanded=${String(manage)} onClick=${() => setManage(!manage)} />`}
    </div>
    ${owner && manage && html`<div class="usages-b-manage-panel">
      <div class="usages-b-manage-row">
        <${IconButton} name=${provider.enabled ? 'pause' : 'play'} label=${provider.enabled ? `Disable ${provider.label || key}` : `Enable ${provider.label || key}`} className="usages-b-toggle" disabled=${!canEnable(provider, configured)} onClick=${() => store.mutateUsage(() => store.source.provider(key, !provider.enabled))} />
        <${IconButton} name="arrow-up" label=${`Move ${provider.label || key} up`} disabled=${movedOrder(providers, order, -1) === null} onClick=${event => move(-1, event)} />
        <${IconButton} name="arrow-down" label=${`Move ${provider.label || key} down`} disabled=${movedOrder(providers, order, 1) === null} onClick=${event => move(1, event)} />
        ${switchable && state.enabled && html`<${IconButton} name="person-plus" label="Add profile" className="usages-b-add" onClick=${() => profileAction('Add profile', 'add')} />
        <${IconButton} name="copy" label="Import current login" className="usages-b-import" onClick=${() => profileAction('Import current login', 'import')} />`}
      </div>
      ${switchable && state.enabled && html`<p class="usage-affected">${affectedText(affected)}</p>`}
      ${key === 'github-copilot' && html`<div class="usage-provider-actions"><${CopilotSetup} store=${store} provider=${provider} key=${`copilot-${revision}`} /></div>`}
      ${key === 'mistral' && html`<div class="usage-provider-actions"><${MistralSetup} store=${store} provider=${provider} key=${`mistral-${revision}`} /></div>`}
      ${key === 'ollama-cloud' && html`<div class="usage-provider-actions"><${OllamaSetup} store=${store} provider=${provider} configured=${configured} key=${`ollama-${revision}`} /></div>`}
    </div>`}
    ${profiles.map(profile => html`<${ProfileRow} key=${profileKey(key, profile)} store=${store} provider=${provider} profile=${profile} state=${state} usedDisplay=${usedDisplay} refreshSec=${refreshSec} tool=${tool}
      affected=${affected} eligible=${eligible} hasActive=${hasActive} manage=${manage} readOnly=${readOnly} reauth=${alerts.some(alert => alert.kind === 'reauth_required' && alert.tool === tool && alert.profile === profile.profile_name && alert.role === 'store')} token=${tokens.find(item => item.tool === tool && item.profile === profile.profile_name && item.role === 'store')} />`)}
  </article>`;
}

// readOnly is the side panel's view: v1's panel only showed, so every control that changes a provider or an account is left out and Switch (and the panel
// header's own refresh and group buttons) remain; Settings → Usages draws the same view with its management controls.
export function UsagesView({ store, readOnly = false, openSettings }) {
  const usage = store.usage.value, state = store.switcher.value, revision = store.revision.value;
  const groupBy = usagesGroup.value;
  const [tokens, setTokens] = useState([]);
  // The token-expiry icons come from the credential targets, which are the owner's and may be absent: the numbers never wait for them.
  useEffect(() => {
    let live = true;
    if (!store.owner.peek() || !store.source.credentialTargets) { setTokens([]); return undefined; }
    Promise.resolve(store.source.credentialTargets()).then(result => { if (live) setTokens(result?.targets || []); }, () => { if (live) setTokens([]); });
    return () => { live = false; };
  }, [revision]);
  if (!usage && !state) return null;
  const providers = sortProviders(usage?.providers);
  const shown_ = state || { enabled: false, profiles: [], eligibility: [], affected_agents: {} };
  const usedDisplay = usage?.settings?.percent_display === 'used', refreshSec = usage?.settings?.refresh_interval_sec;
  // A provider that is off is a tile with its heading alone (v1's inactive tile), so it can be switched on from here.
  const groups = providers.map((provider, order) => ({ provider, order, profiles: !provider.enabled ? [] : provider.profiles?.length ? provider.profiles : [{ profile_name: 'local', active: true }] }));
  const sorted = list => [...list].sort((a, b) => Number(!!b.active) - Number(!!a.active));
  const part = (keep, off) => groups.map(group => ({ ...group, profiles: group.profiles.filter(keep) })).filter(group => group.profiles.length || (off && !group.provider.enabled));
  const primary = groupBy === 'active' ? part(profile => profile.active, false) : groups;
  const secondary = groupBy === 'active' ? part(profile => !profile.active, true) : [];
  // The Settings page already draws the owner's credential alerts in its switcher block, so only the read-only panel (which has no such block) draws them here.
  const alerts = readOnly ? mergeAlerts(usage?.credential_alerts) : [];
  const tile = group => html`<${Tile} key=${group.provider.provider_name} store=${store} provider=${group.provider} profiles=${sorted(group.profiles)} order=${group.order} providers=${providers} state=${shown_}
    usedDisplay=${usedDisplay} refreshSec=${refreshSec} tokens=${tokens} revision=${revision} readOnly=${readOnly} alerts=${usage?.credential_alerts || []} />`;
  return html`<div class="usages-b">
    ${alerts.length > 0 && html`<div class="usages-credential-alerts" role="alert">${alerts.map(alert => html`<div class=${`usages-credential-alert usages-credential-alert-${String(alert.kind || '').replace(/[^a-z_]/g, '')}`}><p>${alert.text}</p>
      ${alert.details.length > 0 && html`<details><summary>Details</summary>${alert.details.map(line => html`<p>${line}</p>`)}</details>`}</div>`)}</div>`}
    <div class="usages-b-grid">${primary.map(tile)}</div>
    ${secondary.length > 0 && html`<div class="usages-group-divider" role="separator" aria-label="Other profiles"></div><div class="usages-b-grid">${secondary.map(tile)}</div>`}
    ${!providers.length && html`<p class="usage-empty">No usage providers are available yet.</p>`}
    ${readOnly && providers.length > 0 && !providers.some(provider => provider.enabled) && html`<div class="usages-empty"><div>No usage providers enabled.</div>${openSettings && html`<button type="button" class="btn usages-popover-settings" onClick=${openSettings}>Open Settings → Usages</button>`}</div>`}
    ${providers.length > 0 && html`<p class="settings-desc usages-legend">${USAGE_LEGEND}</p>`}
  </div>`;
}

// B's two actions for the Usages page's header, where the panel's own header already holds them (refresh, and the group button).
export function UsagesActions({ store }) {
  const activeFirst = usagesGroup.value === 'active', label = groupLabel(usagesGroup.value);
  return html`<span class="usages-b-actions">
    ${store.owner.value && html`<${IconButton} name="refresh" label="Refresh usages" className="usages-b-refresh-all" onClick=${() => store.mutateUsage(() => store.source.refresh())} />`}
    <button type="button" class=${`btn-icon usages-b-group${activeFirst ? ' active' : ''}`} title=${label} aria-label=${label} aria-pressed=${String(activeFirst)} onClick=${toggleUsagesGroup}>${GROUP_ICON}</button>
  </span>`;
}
