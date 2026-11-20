import { html, Fragment, useState } from '../../ui.js';
import { IconButton } from '../../components/icon-button.js';
import { promptDialog } from '../../dialogs.js';
import { percent, windowLabel, paceExplanation, sparkGeometry, countdown, shown, staleResetText, creditsText } from '../../usage-format.js';
import { staleAge } from '../../usage-alerts.js';
import { sortProviders, toolOf, isSwitchable, providerStatus, providerConfigured, canEnable, movedOrder, profileKey, switchView, profileStatusLine } from './model.js';
import { profileActions } from './profile-actions.js';
import { CopilotSetup, MistralSetup, OllamaSetup } from './provider-card.js';

// Usages, variant C: "headroom rows". The page answers one question first, in one sentence at the top: which limit is closest to running out, and
// when it resets. Below it every account is one row, grouped by provider, with one thin bar per main window (session, weekly, monthly, and any other
// window that has crossed the alert thresholds). A bar is coloured by how close it is to its limit, with the same thresholds as the Inbox alerts, so a
// glance down the colour of the row edges says the same thing as the headline. Everything else (the 48-hour sparkline, the pace sentence, the other
// windows, credits, the e-mail, rename and delete) is one click away, in the row's details. Styles: the "Usages variant C" block in v2-only.css.

const MAIN_WINDOWS = ['session', 'weekly', 'monthly'];
const time = date => date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
const when = value => { const ms = Date.parse(value); return Number.isFinite(ms) ? new Date(ms).toLocaleString([], { weekday: 'short', hour: '2-digit', minute: '2-digit' }) : ''; };

// How close a window is to its limit, by the share left: the Inbox alert thresholds decide (20% and 10% unless the owner changed them).
export function levelOf(left, warning = 20, critical = 10) {
  if (left <= critical) return 'critical';
  if (left <= warning) return 'warning';
  return 'ok';
}

const leftOf = window => 100 - percent(window.percent_used);

// The windows a collapsed row shows: the main ones, and any other one that has reached a threshold (an Opus or Spark limit that is about to stop work
// must not hide in the details). A profile that reports none of the main windows shows its first one.
export function laneWindows(windows, warning, critical) {
  const list = windows || [];
  const lanes = list.filter(window => MAIN_WINDOWS.includes(window.key) || levelOf(leftOf(window), warning, critical) !== 'ok');
  return lanes.length ? lanes : list.slice(0, 1);
}

// "Runs out in …" when the pace says the window empties before its reset, from the time the snapshot was taken.
function runsOut(window, fetchedAt) {
  const pace = window.pace;
  if (!pace || pace.lasts_to_reset || pace.eta_seconds == null) return '';
  const base = Number.isFinite(Date.parse(fetchedAt)) ? Date.parse(fetchedAt) : Date.now();
  return `runs out in ${countdown(new Date(base + pace.eta_seconds * 1000).toISOString())}`;
}

// The one sentence at the top: the window with the least left among the fresh, readable profiles. Stale data makes no claim here.
export function headline(providers, { warning = 20, critical = 10, refreshSec, now = Date.now() } = {}) {
  let tightest = null;
  for (const provider of providers || []) {
    for (const profile of provider.profiles || []) {
      if (profile.status && profile.status !== 'ok') continue;
      if (staleAge(profile.last_refresh_at, refreshSec, now)) continue;
      for (const window of profile.windows || []) {
        const left = leftOf(window);
        if (!tightest || left < tightest.left) tightest = { provider, profile, window, left };
      }
    }
  }
  if (!tightest) return { level: 'none', text: 'No limits reported yet.' };
  const { provider, profile, window, left } = tightest;
  const level = levelOf(left, warning, critical);
  const who = `${provider.label || provider.provider_name} · ${profile.profile_name || 'local'}`;
  const parts = [`${windowLabel(window.key)} ${shown(left)} left`, runsOut(window, profile.last_refresh_at), window.reset_at ? `resets in ${countdown(window.reset_at)}` : ''].filter(Boolean);
  const lead = level === 'critical' ? 'Almost out' : level === 'warning' ? 'Running low' : 'All accounts have headroom. Tightest';
  return { level, text: `${lead}: ${who}, ${parts.join(', ')}.`, target: profileKey(provider.provider_name, profile) };
}

// The level of a whole row: its worst window.
function rowLevel(profile, warning, critical, stale) {
  if (stale) return 'stale';
  if (profile.status && profile.status !== 'ok' && !(profile.windows || []).length) return 'none';
  const windows = profile.windows || [];
  if (!windows.length) return 'none';
  return levelOf(Math.min(...windows.map(leftOf)), warning, critical);
}

// One window as a compact lane: label, bar with the expected-pace tick, the figure, the reset countdown.
function Lane({ window, usedDisplay, stale, warning, critical }) {
  const left = leftOf(window), value = usedDisplay ? 100 - left : left;
  const label = windowLabel(window.key), side = usedDisplay ? 'used' : 'left';
  const level = stale ? 'stale' : levelOf(left, warning, critical);
  const pace = stale ? null : window.pace, expected = Number(pace?.expected_percent);
  const paceState = pace?.state === 'reserve' ? 'reserve' : pace?.state === 'deficit' ? 'deficit' : 'on-track';
  const reset = window.reset_at ? (stale ? staleResetText(window.reset_at) : countdown(window.reset_at)) : '';
  return html`<div class=${`uc-lane uc-level-${level}`} data-window=${window.key}>
    <span class="uc-lane-label" title=${label}>${label}</span>
    <div class="uc-bar" role="progressbar" aria-label=${`${label} ${side}`} aria-valuenow=${String(Math.round(value))} aria-valuemin="0" aria-valuemax="100">
      <span class="uc-fill" style=${{ width: `${value}%` }}></span>
      ${pace && Number.isFinite(expected) && html`<i class=${`uc-pace uc-pace-${paceState}`} style=${{ left: `${usedDisplay ? percent(expected) : 100 - percent(expected)}%` }} title=${paceExplanation(pace, usedDisplay)}></i>`}
    </div>
    <span class="uc-lane-value">${shown(value)}</span>
    <span class="uc-lane-reset" title=${window.reset_at ? `${stale ? '' : 'Resets '}${when(window.reset_at)}` : ''}>${reset && !stale && html`<span class="icon-mask icon-mask-refresh" aria-hidden="true"></span>`}${reset}</span>
  </div>`;
}

// A window in the details: the bar again, with its 48-hour line and the pace sentence, which explains itself on hover and on focus.
function WindowDetail({ window, usedDisplay, stale, fetchedAt, warning, critical }) {
  const spark = sparkGeometry(window, usedDisplay);
  const pace = stale ? null : window.pace;
  let paceText = '';
  if (pace) {
    const delta = Math.abs(Number(pace.delta_percent) || 0);
    const state = pace.state === 'reserve' ? `${shown(delta)} in reserve` : pace.state === 'deficit' ? `${shown(delta)} in deficit` : 'On pace';
    const eta = pace.lasts_to_reset ? 'lasts until reset' : runsOut(window, fetchedAt);
    paceText = [state, eta].filter(Boolean).join(' · ');
  }
  const explanation = pace ? paceExplanation(pace, usedDisplay) : '';
  return html`<div class="uc-window">
    <${Lane} window=${window} usedDisplay=${usedDisplay} stale=${stale} warning=${warning} critical=${critical} />
    ${spark && html`<figure class="uc-spark">
      <svg viewBox=${spark.viewBox} preserveAspectRatio="none" role="img" aria-label=${spark.label}>
        <polyline class="uc-spark-line" points=${spark.points} fill="none" vector-effect="non-scaling-stroke" />
        <circle class="uc-spark-dot" cx=${spark.end[0]} cy=${spark.end[1]} r="1.6" />
      </svg>
      <figcaption><span>48 h ago</span><span>${usedDisplay ? '% used' : '% left'}</span><span>now</span></figcaption>
    </figure>`}
    ${paceText && html`<p class=${`uc-pace-text uc-pace-${pace.state === 'reserve' ? 'reserve' : pace.state === 'deficit' ? 'deficit' : 'on-track'}`} title=${explanation} tabindex="0" aria-label=${`${paceText}. ${explanation}`}>${paceText}</p>`}
  </div>`;
}

function AccountRow({ store, provider, profile, state, usedDisplay, refreshSec, warning, critical, tool, switchable, affected, eligible, hasActive, reauth, open, onToggle }) {
  const name = profile.profile_name || 'local';
  const configured = (state.profiles || []).some(item => item.tool === tool && item.name === profile.profile_name);
  const view = switchView({ switchable, enabled: state.enabled, owner: state.owner, configured, profile, hasActive, eligible });
  const manage = !switchable || !state.enabled || !state.owner || !configured;
  const age = staleAge(profile.last_refresh_at, refreshSec);
  const stale = !!age;
  const windows = profile.windows || [];
  const lanes = laneWindows(windows, warning, critical);
  const level = rowLevel(profile, warning, critical, stale);
  const status = profileStatusLine(profile, time);
  const credits = profile.credits ? creditsText(profile.credits) : '';
  const detailId = `uc-detail-${provider.provider_name}-${name}`.replace(/[^A-Za-z0-9_-]/g, '_');
  const { use, installAnyway, rename, remove } = profileActions({ store, tool, profile, affected });
  const activeText = profile.active ? 'Active account' : 'Inactive account';
  return html`<li class=${`uc-row uc-level-${level}${profile.active ? ' is-active' : ''}${stale ? ' is-stale' : ''}${open ? ' is-open' : ''}`} data-profile=${name}>
    <div class="uc-row-main">
      <button type="button" class="row-btn uc-row-toggle" aria-expanded=${open ? 'true' : 'false'} aria-controls=${detailId} title=${open ? `Hide ${name}'s details` : `Show ${name}'s details`} onClick=${onToggle}>
        <span class="icon-mask icon-mask-chevron-right uc-chevron" aria-hidden="true"></span>
        <span class="uc-dot" role="img" aria-label=${activeText} title=${activeText}></span>
        <strong class="uc-name">${name}</strong>
        ${profile.plan && html`<span class="uc-plan" title=${profile.plan}>${profile.plan}</span>`}
      </button>
      <div class="uc-lanes">
        ${lanes.length ? lanes.map((window, index) => html`<${Lane} key=${`${window.key}:${index}`} window=${window} usedDisplay=${usedDisplay} stale=${stale} warning=${warning} critical=${critical} />`)
          : html`<span class="uc-empty">${status || 'No limits reported'}</span>`}
      </div>
      <div class="uc-row-actions">
        ${stale && html`<span class="uc-stale-badge" title=${`Stale, last update ${age} ago`}>${`${age} old`}</span>`}
        ${!view.hidden && html`<${IconButton} name="switch" label=${view.disabled ? view.title : `Switch to ${name}`} className="uc-switch" disabled=${view.disabled} onClick=${use} />`}
      </div>
    </div>
    ${open && html`<div class="uc-detail" id=${detailId}>
      <div class="uc-windows">${windows.map((window, index) => html`<${WindowDetail} key=${`${window.key}:${index}`} window=${window} usedDisplay=${usedDisplay} stale=${stale} fetchedAt=${profile.last_refresh_at} warning=${warning} critical=${critical} />`)}</div>
      <dl class="uc-facts">
        ${profile.email && html`<${Fragment}><dt>Account</dt><dd class="uc-email">${profile.email}</dd><//>`}
        ${credits && html`<${Fragment}><dt>Credits</dt><dd class="uc-credits">${credits}</dd><//>`}
        ${status && html`<${Fragment}><dt>Status</dt><dd class="uc-status">${status}</dd><//>`}
        ${stale && html`<${Fragment}><dt>Data</dt><dd class="uc-stale">Stale, last update ${age} ago</dd><//>`}
      </dl>
      <div class="uc-detail-actions">
        ${stale && state.owner && html`<${IconButton} name="refresh" label=${`Refresh ${provider.label || provider.provider_name}`} className="uc-refresh" onClick=${() => store.mutateUsage(() => store.source.refresh(provider.provider_name))} />`}
        ${reauth && profile.has_credentials && !view.hidden && html`<${IconButton} name="key-off" label="Install anyway: use the stored credentials without refreshing them" className="uc-install-anyway" disabled=${view.disabled} onClick=${installAnyway} />`}
        ${!manage && html`<${IconButton} name="edit" label=${`Rename ${name}`} className="uc-rename" onClick=${rename} />
          <${IconButton} name="trash" label=${`Delete ${name}`} danger className="uc-remove" onClick=${remove} />`}
      </div>
    </div>`}
  </li>`;
}

// The credential forms of the three providers the server cannot read by itself (shared with variants A and B), behind the provider's setup button.
const SETUPS = { 'github-copilot': CopilotSetup, mistral: MistralSetup, 'ollama-cloud': OllamaSetup };
function ProviderSetup({ store, provider, configured }) {
  const Setup = SETUPS[provider.provider_name];
  return Setup ? html`<div class="uc-setup"><${Setup} store=${store} provider=${provider} configured=${configured} /></div>` : null;
}

const HAS_SETUP = new Set(Object.keys(SETUPS));
const MASKED = new Set(['codex', 'claude-code', 'github-copilot', 'ollama-cloud', 'mistral']);

function ProviderGroup({ store, provider, order, providers, state, alerts, usedDisplay, refreshSec, warning, critical, revision, openRows, toggleRow, setupOpen, toggleSetup }) {
  const key = provider.provider_name, tool = toolOf(key), label = provider.label || key;
  const switchable = isSwitchable(tool);
  const affected = (state.affected_agents || {})[tool] || [];
  const eligible = (state.eligibility || []).find(item => item.tool === tool);
  const hasActive = (state.profiles || []).some(item => item.tool === tool && item.active);
  const configured = providerConfigured(provider);
  const owner = !!state.owner;
  // The active account first: it is the one work runs on now.
  const profiles = [...(provider.profiles || [])].sort((a, b) => Number(!!b.active) - Number(!!a.active));
  const idle = !provider.enabled && profiles.every(profile => !(profile.windows || []).length);
  const profileAction = async (title, method) => {
    const name = await promptDialog('Profile name', { title, label: 'Profile name', required: true, confirm: title === 'Add profile' ? 'Add' : 'Import' });
    if (name?.trim()) store.mutateSwitcher(() => store.source[method](tool, name.trim()));
  };
  const move = delta => store.mutateUsage(() => store.source.settings({ provider_order: movedOrder(providers, order, delta) }));
  return html`<section class=${`uc-provider${idle ? ' is-idle' : ''}`} data-provider=${key} aria-label=${label}>
    <header class="uc-provider-head">
      <span class="uc-provider-icon" aria-hidden="true">${MASKED.has(key) ? html`<span class=${`usages-provider-mask usages-provider-mask-${key}`}></span>` : 'AI'}</span>
      <h4 class="uc-provider-name">${label}</h4>
      <span class="uc-provider-status">${providerStatus(provider)}</span>
      ${switchable && state.enabled && html`<span class="uc-affected" title=${affected.length ? affected.map(agent => `${agent.id}${agent.running ? ' (running)' : ''}`).join(', ') : 'No agents use this tool'}>${affected.length ? `${affected.length} agent${affected.length === 1 ? '' : 's'}` : ''}</span>`}
      ${owner && html`<div class="uc-provider-actions">
        ${switchable && state.enabled && html`<${IconButton} name="plus" label=${`Add a ${label} profile`} className="uc-add" onClick=${() => profileAction('Add profile', 'add')} />
          <${IconButton} name="person-plus" label=${`Import the current ${label} login`} className="uc-import" onClick=${() => profileAction('Import current login', 'import')} />`}
        ${HAS_SETUP.has(key) && html`<${IconButton} name="more" label=${`${label} credentials`} className="uc-setup-toggle" aria-expanded=${setupOpen ? 'true' : 'false'} onClick=${toggleSetup} />`}
        <${IconButton} name=${provider.enabled ? 'pause' : 'play'} label=${provider.enabled ? `Disable ${label}` : `Enable ${label}`} className="uc-enable" disabled=${!canEnable(provider, configured)} onClick=${() => store.mutateUsage(() => store.source.provider(key, !provider.enabled))} />
        <${IconButton} name="arrow-up" label=${`Move ${label} up`} className="uc-move-up" disabled=${movedOrder(providers, order, -1) === null} onClick=${() => move(-1)} />
        <${IconButton} name="arrow-down" label=${`Move ${label} down`} className="uc-move-down" disabled=${movedOrder(providers, order, 1) === null} onClick=${() => move(1)} />
      </div>`}
    </header>
    ${owner && setupOpen && HAS_SETUP.has(key) && html`<${ProviderSetup} key=${`${key}-${revision}`} store=${store} provider=${provider} configured=${configured} />`}
    ${idle ? html`<p class="uc-idle">Not set up${owner && HAS_SETUP.has(key) ? ': add credentials with the ⋯ button.' : '.'}</p>`
      : html`<ul class="uc-rows">${profiles.map(profile => { const id = profileKey(key, profile); return html`<${AccountRow} key=${id} store=${store} provider=${provider} profile=${profile} state=${state} usedDisplay=${usedDisplay} refreshSec=${refreshSec}
        warning=${warning} critical=${critical} tool=${tool} switchable=${switchable} affected=${affected} eligible=${eligible} hasActive=${hasActive} open=${openRows.has(id)} onToggle=${() => toggleRow(id)}
        reauth=${alerts.some(alert => alert.kind === 'reauth_required' && alert.tool === tool && alert.profile === profile.profile_name && alert.role === 'store')} />`; })}</ul>`}
  </section>`;
}

const LEVEL_ICON = { critical: 'stop', warning: 'pause', ok: 'check', none: 'chart-column' };

// The list part of the Usages page (what UsagesList draws in variant A), from the same store.
export function UsagesVariantC({ store }) {
  const [openRows, setOpenRows] = useState(() => new Set());
  const [setups, setSetups] = useState(() => new Set());
  const usage = store.usage.value, switcher = store.switcher.value;
  if (!usage && !switcher) return null;
  const providers = sortProviders(usage?.providers);
  const state = switcher || { enabled: false, profiles: [], eligibility: [], affected_agents: {} };
  const settings = usage?.settings || {};
  const usedDisplay = settings.percent_display === 'used';
  const warning = Number(settings.alert_warning_remaining) || 20, critical = Number(settings.alert_critical_remaining) || 10;
  const refreshSec = settings.refresh_interval_sec;
  const toggle = setter => id => setter(current => { const next = new Set(current); if (next.has(id)) next.delete(id); else next.add(id); return next; });
  const toggleRow = toggle(setOpenRows), toggleSetup = toggle(setSetups);
  const top = headline(providers, { warning, critical, refreshSec });
  return html`<div class="usages-c">
    ${providers.length > 0 && html`<p class=${`uc-headline uc-level-${top.level}`} role="status">
      <span class=${`icon-mask icon-mask-${LEVEL_ICON[top.level]} uc-headline-icon`} aria-hidden="true"></span>
      ${top.target ? html`<button type="button" class="row-btn uc-headline-text" title="Show this account's details" onClick=${() => { if (!openRows.has(top.target)) toggleRow(top.target); }}>${top.text}</button>` : html`<span class="uc-headline-text">${top.text}</span>`}
    </p>`}
    ${providers.map((provider, order) => html`<${ProviderGroup} key=${provider.provider_name} store=${store} provider=${provider} order=${order} providers=${providers} state=${state} alerts=${usage?.credential_alerts || []}
      usedDisplay=${usedDisplay} refreshSec=${refreshSec} warning=${warning} critical=${critical} revision=${store.revision.value} openRows=${openRows} toggleRow=${toggleRow}
      setupOpen=${setups.has(provider.provider_name)} toggleSetup=${() => toggleSetup(provider.provider_name)} />`)}
    ${!providers.length && html`<p class="usage-empty">No usage providers are available yet.</p>`}
    ${providers.length > 0 && html`<p class="uc-legend">
      <span><i class="uc-key uc-level-ok"></i>more than ${warning}% left</span>
      <span><i class="uc-key uc-level-warning"></i>${warning}% or less</span>
      <span><i class="uc-key uc-level-critical"></i>${critical}% or less</span>
      <span><i class="uc-key-tick"></i>expected pace</span>
      <span>Open a row for its history and details.</span>
    </p>`}
  </div>`;
}
