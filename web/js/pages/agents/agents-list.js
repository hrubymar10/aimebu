import { html, Component, useRef, useState } from '../../ui.js';
import { currentUser } from '../../api.js';
import { IconButton } from '../../components/icon-button.js';
import { harnessIcon, agentLabel } from '../../sender-identity.js';
import { relativeTime, stateAge } from '../../time.js';
import { harnessName } from '../../harness-version.js';
import { healthLabel } from '../health/model.js';
import { actionsOf, activityOf, contextOf, grantsOf, liveOf, profileRows, stateBadge, stateDot, stateOf, switchStatusOf } from './model.js';

const PATHS = {
  dm: 'M3 4h14a1 1 0 0 1 1 1v8a1 1 0 0 1-1 1H9l-4 3v-3H3a1 1 0 0 1-1-1V5a1 1 0 0 1 1-1z',
  pause: 'M6 4h3v12H6zM11 4h3v12h-3z',
  stop: 'M5 5h10v10H5z',
};

function Glyph({ name }) {
  return html`<svg viewBox="0 0 20 20" width="13" height="13" aria-hidden="true"><path d=${PATHS[name]} fill="currentColor" /></svg>`;
}

function QuickButton({ className, label, icon, ...rest }) {
  return html`<button type="button" class=${`btn-icon ${className}`} title=${label} aria-label=${label} ...${rest}><${Glyph} name=${icon} /></button>`;
}

// Read here, so a tick of the clock renders these two texts and nothing else of the card.
function StateAge({ store, since }) {
  const at = Date.parse(since || '');
  return html`<span class="agent-state-age">${Number.isFinite(at) ? stateAge((store.now.value - at) / 1000) : ''}</span>`;
}

function LiveChip({ store, live }) {
  const at = Date.parse(live.at || '');
  const known = Number.isFinite(at);
  return html`<span class="agent-live-chip" hidden=${!known}>${known ? `${live.label} ${stateAge(Math.max(0, Math.floor((store.now.value - at) / 1000)))}` : ''}</span>`;
}

function CopyButton({ value, label, target }) {
  const [state, setState] = useState('copy');
  const copyLabel = `Copy ${label.toLowerCase()}`;
  const click = async () => {
    try { await navigator.clipboard.writeText(String(value)); setState('check'); } catch {
      setState('failed');
      const range = document.createRange(); range.selectNodeContents(target.current); const selection = getSelection(); selection.removeAllRanges(); selection.addRange(range);
    }
    setTimeout(() => setState('copy'), 1500);
  };
  return html`<${IconButton} name=${state === 'check' ? 'check' : 'copy'} label=${state === 'check' ? 'Copied' : state === 'failed' ? 'Copy failed: press Ctrl or ⌘ C' : copyLabel} className="profile-copy" onClick=${click} />`;
}

function Fact({ label, value, mono, copy }) {
  const target = useRef(null);
  return html`<div class="profile-fact"><span class="profile-fact-label">${label}</span>${mono ? html`<code class="profile-fact-value" ref=${target}>${String(value)}</code>` : html`<strong class="profile-fact-value" ref=${target}>${String(value)}</strong>`}${copy && html`<${CopyButton} value=${value} label=${label} target=${target} />`}</div>`;
}

function Output({ agent, state, store }) {
  const download = () => {
    const url = URL.createObjectURL(new Blob([state.text], { type: 'text/plain;charset=utf-8' }));
    const link = document.createElement('a'); link.href = url; link.download = `${agent.slug || agent.id}-output.txt`; document.body.append(link); link.click(); link.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  };
  return html`<details class="profile-output" open=${state.open} onToggle=${event => { if (event.currentTarget.open !== state.open) store.setOutputOpen(event.currentTarget.open); }}>
    <summary>Output</summary>
    <div class="profile-output-bar"><${IconButton} name="refresh" label="Refresh output" className="profile-output-refresh" disabled=${state.loading} onClick=${() => store.loadOutput(agent.id)} /><${IconButton} name="arrow-down" label="Download output" className="profile-output-download" disabled=${!state.text} onClick=${download} /></div>
    <p class="profile-output-note">The end of what this agent's harness printed to stderr, kept on its machine for 14 days. It can contain secrets from tool output.</p>
    <pre class="profile-output-text" tabindex="0" role="region" aria-label="Agent output" hidden=${!state.text}>${state.text}</pre>
    <p class="profile-output-status" role="status">${state.loading ? 'Loading…' : state.status}</p></details>`;
}

function Profile({ agent, profile, output, readOutput, store }) {
  const rows = profileRows(agent, profile, { harness: harnessName, relative: relativeTime });
  return html`<div class="profile-facts-list">${rows.map(([label, value, options]) => html`<${Fact} key=${label} label=${label} value=${value} ...${options} />`)}</div>${readOutput && output?.agentID === agent.id && html`<${Output} agent=${agent} state=${output} store=${store} />`}`;
}

// A card renders again only when its own props changed: the list hands every card the record it already held when the data is the same.
export class AgentCard extends Component {
  shouldComponentUpdate(next) {
    const now = this.props;
    for (const key of Object.keys(next)) if (next[key] !== now[key]) return true;
    return false;
  }

  render({ agent, expanded, profile, output, readOutput, down, lifecycle, api }) {
    const state = stateOf(agent), activity = activityOf(agent, state), grants = grantsOf(agent), context = contextOf(agent.latest_usage), live = liveOf(agent);
    const sw = switchStatusOf(agent, harnessName), actions = actionsOf(agent, { down, lifecycle });
    const { src, label } = harnessIcon(agent.tool || '');
    const name = agentLabel(agent) || agent.id, dm = `Message ${agent.name || agent.slug || 'agent'} privately`;
    const reason = key => actions.find(action => action.name === key);
    const toggle = () => api.store.toggleProfile(agent.id);
    return html`<article class=${expanded ? 'agent-card agent-card-compact expanded' : 'agent-card agent-card-compact'} data-agent-id=${agent.id} data-woken-by=${agent.woken_by || ''} data-live-at=${live.at} data-live-label=${live.label}
      onClick=${event => { if (!event.target.closest('button, a, input, select, textarea, .profile-output')) toggle(); }}>
      <div class="agent-id agent-title"><span class=${`agent-online-dot ${stateDot(state.kind)} agent-dot`} aria-hidden="true" data-state=${state.kind} title=${state.title}></span>
        <img class="harness-icon agent-harness-icon" alt="" src=${src} title=${`${agent.model || 'unknown'} · ${label}`} /><span class="agent-emoji" aria-hidden="true">${agent.role_emoji || ''}</span>
        <span class="agent-identity"><span class="agent-id-text agent-name" title=${name}>${name}</span></span>
        <div class="agent-state" data-since=${state.since} data-state=${state.kind} title=${state.title}><span class=${`agent-state-pill agent-state-badge agent-state-${stateBadge(state.kind)}`}><span class="agent-state-label">${state.name}</span>${' '}<${StateAge} store=${api.store} since=${state.since} /></span></div>
        <span class="agent-role" title=${agent.role_updated ? 'Role updated; respawn to apply the project definition.' : ''}>${`${agent.role_emoji || ''} ${agent.role || 'Agent'}${agent.role_deleted ? ' (deleted)' : ''}`.trim()}</span>
        <div class="agent-card-actions agent-quick"><${QuickButton} className="agent-private" label=${dm} icon="dm" onClick=${() => api.onPrivate(agent)} />
          <${QuickButton} className="agent-pause" label="Pause" icon="pause" disabled=${reason('act-pause').disabled} onClick=${() => api.store.control(agent.id, 'pause')} />
          <${QuickButton} className="agent-stop icon-btn-danger" label="Stop" icon="stop" disabled=${reason('act-stop').disabled} onClick=${() => api.store.control(agent.id, 'stop')} />
          <${IconButton} name=${expanded ? 'close' : 'chevron-right'} label=${expanded ? 'Close agent profile' : 'Open agent profile'} className="agent-expand" aria-expanded=${String(expanded)} onClick=${toggle} /></div></div>
      <small class="agent-meta">${`${harnessName(agent.tool) || 'unknown'} · ${agent.model || 'unknown'}`}</small>
      <small class="agent-role-update">${agent.role_updated ? 'Role updated · respawn to apply' : ''}</small>
      <div class="agent-health">${(agent.health_flags || []).map((flag, index) => html`<span key=${`${flag.code}${index}`} class=${`health-flag health-${flag.code}`} title=${healthLabel(flag)}>${healthLabel(flag)}</span>`)}</div>
      <p class="agent-activity" title=${[activity.last && `Last agent event: ${new Date(activity.last).toLocaleString()}`, activity.seen && `Supervisor seen: ${new Date(activity.seen).toLocaleString()}`].filter(Boolean).join(' · ') || 'No activity time recorded'}>${activity.text}</p>
      <p class="agent-switch-status" hidden=${sw.hidden} onClick=${sw.inbox ? () => document.querySelector('#inbox-toggle')?.click() : null}>${sw.text}</p>
      <small class="agent-boundary-grants" hidden=${grants.hidden}>${grants.text}</small>
      <div class="agent-context" title=${context.title} aria-label=${`Memory: ${context.title}`}><span>Memory</span><span class="agent-context-track" hidden=${!context.known}><span class="agent-context-fill" style=${`width:${context.width}`}></span></span><span class="agent-context-value">${context.value}</span></div>
      <${LiveChip} store=${api.store} live=${live} />
      <${IconButton} name="health" label="Recovery needed" className="agent-recovery" hidden=${!agent.recovery_block} onClick=${() => api.onRecovery(agent)} />
      <div class="agent-profile-facts">${expanded && html`<${Profile} agent=${agent} profile=${profile} output=${output} readOutput=${readOutput} store=${api.store} />`}</div>
      <div class="agent-actions">${actions.map(action => html`<button key=${action.name} type="button" class=${`btn-icon ${action.name === 'act-stop' || action.name === 'remove' ? 'icon-btn-danger ' : ''}agent-act agent-${action.name}`} disabled=${action.disabled} title=${action.title} aria-label=${action.label} aria-describedby=${action.disabled ? `agent-hint-${agent.id}-${action.name}` : null}
        onClick=${() => ({ message: () => api.onPrivate(agent), 'act-pause': () => api.store.control(agent.id, 'pause'), 'act-stop': () => api.store.control(agent.id, 'stop'), switch: () => api.openSwitch(agent.id), reactivate: () => api.store.control(agent.id, 'reactivate'), remove: () => api.store.remove(agent) })[action.name]()}><span class=${`icon-mask icon-mask-${{ message: 'message', 'act-pause': 'pause', 'act-stop': 'stop', switch: 'switch', reactivate: 'play', remove: 'trash' }[action.name]}`} aria-hidden="true"></span></button>`)}</div>
      <div class="agent-action-hints">${actions.map(action => html`<small key=${action.name} class="agent-action-hint" id=${`agent-hint-${agent.id}-${action.name}`} hidden=${!action.hint}>${action.hint}</small>`)}</div></article>`;
  }
}

function Human({ human, kind, api }) {
  const mine = human.id === currentUser;
  return html`<article class=${mine ? 'agent-card agent-card-compact human-card is-you' : 'agent-card agent-card-compact human-card'} data-human-id=${human.id}><div class="agent-id agent-title"><span class="agent-online-dot active" aria-hidden="true" data-state="idle"></span>
    <img class="harness-icon agent-harness-icon" src="/icons/human.svg" alt="" title="human" aria-hidden="true" /><span class="agent-id-text agent-name human-name" ...${mine ? { title: 'You' } : {}}>${human.login}</span><small class="human-role">${human.role || ''}</small>
    <div class="agent-card-actions agent-quick">${!mine && kind !== 'dm' && html`<${QuickButton} className="human-dm" label=${`Message ${human.login} privately`} icon="dm" onClick=${() => api.onPrivateHuman?.(human)} />`}</div></div></article>`;
}

export function HumansList({ store, api }) {
  const { kind } = store.roomInfo.value;
  return html`${store.humans.value.map(human => html`<${Human} key=${human.id} human=${human} kind=${kind} api=${api} />`)}`;
}

export function AgentsList({ store, api }) {
  const agents = [...store.agents.value.values()], open = store.profileId.value, profile = store.profileData.value, output = store.output.value;
  const room = store.roomInfo.value;
  const readOutput = api.canReadOutput(), down = store.available.value ? '' : 'Agent controls are unavailable';
  return html`${agents.map(agent => html`<${AgentCard} key=${agent.id} agent=${agent} expanded=${open === agent.id} profile=${open === agent.id ? profile : null} output=${open === agent.id ? output : null} readOutput=${readOutput} down=${down} lifecycle=${room.lifecycle} api=${api} />`)}
    ${store.loaded.value && !agents.length && html`<p class="agents-empty">No agents in this room</p>`}`;
}
