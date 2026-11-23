import { html, Component, useRef, useState } from '../../ui.js';
import { currentUser } from '../../api.js';
import { IconButton } from '../../components/icon-button.js';
import { harnessIcon, agentLabel } from '../../sender-identity.js';
import { relativeTime, stateAge } from '../../time.js';
import { harnessName } from '../../harness-version.js';
import { healthLabel } from '../health/model.js';
import { actionsOf, quickActionsOf, activityOf, keepAliveOf, contextOf, dotOf, grantsOf, liveOf, profileRows, stateBadge, stateDot, stateOf, switchStatusOf } from './model.js';

const PATHS = {
  dm: 'M3 4h14a1 1 0 0 1 1 1v8a1 1 0 0 1-1 1H9l-4 3v-3H3a1 1 0 0 1-1-1V5a1 1 0 0 1 1-1z',
  pause: 'M6 4h3v12H6zM11 4h3v12h-3z',
  stop: 'M5 5h10v10H5z',
  play: 'M6 4l10 6-10 6z',
};

function Glyph({ name }) {
  return html`<svg viewBox="0 0 20 20" width="13" height="13" aria-hidden="true"><path d=${PATHS[name]} fill="currentColor" /></svg>`;
}

function QuickButton({ className, label, icon, ...rest }) {
  return html`<button type="button" class=${`btn-icon ${className}`} title=${label} aria-label=${label} ...${rest}><${Glyph} name=${icon} /></button>`;
}

// Read here, so a tick of the clock renders these two texts and nothing else of the card.
// How long the agent has been in its state, on the state pill. A collapsed card of an idle agent leaves it out ("idle", not "idle 31:23": how long nothing happened is detail for the
// open card); a stopped agent never shows an age. A working agent keeps it on a collapsed card too, since that is useful while something runs.
function StateAge({ store, since, hidden }) {
  if (hidden) return null;
  const at = Date.parse(since || '');
  return html`<span class="agent-state-age">${Number.isFinite(at) ? stateAge((store.now.value - at) / 1000) : ''}</span>`;
}

// What the agent is doing under its name, when the state pill on the right does not already say it: a working agent is "working" in the pill, so the chip adds
// only the "tool" of an agent that is running a tool.
function LiveChip({ store, live, said }) {
  const at = Date.parse(live.at || '');
  const known = Number.isFinite(at) && live.label !== said;
  return html`<span class="agent-live-chip" hidden=${!known}>${known ? `${live.label} ${stateAge(Math.max(0, Math.floor((store.now.value - at) / 1000)))}` : ''}</span>`;
}

// The words behind a card's one status dot: its tooltip and, visually hidden, the same sentence for a screen reader (online or offline, and warm or cold: dotOf). Read here, so a
// tick of the clock changes the minutes left and nothing else of the card.
function DotWords({ store, agent, state }) {
  const dot = dotOf(agent, state, store.now.value);
  return html`<span class="dot-words" title=${dot.title}><span class="sr-only dot-sr">${dot.title}</span></span>`;
}

function CopyButton({ value, label, target }) {
  const [state, setState] = useState('copy');
  const copyLabel = label === 'Resume' ? 'Copy resume command' : `Copy ${label.toLowerCase()}`;
  const click = async () => {
    try { await navigator.clipboard.writeText(String(value)); setState('check'); } catch {
      setState('failed');
      const range = document.createRange(); range.selectNodeContents(target.current); const selection = getSelection(); selection.removeAllRanges(); selection.addRange(range);
    }
    setTimeout(() => setState('copy'), 1500);
  };
  return html`<${IconButton} name=${state === 'check' ? 'check' : 'copy'} label=${state === 'check' ? 'Copied' : state === 'failed' ? 'Copy failed: press Ctrl or ⌘ C' : copyLabel} className=${`profile-copy${label === 'Resume' ? ' profile-copy-resume' : ''}`} onClick=${click} />`;
}

// A path or an id is cut at its separators, never in the middle of a name: a break opportunity after each of / - _ . (the characters of the text itself are
// untouched, so selecting and copying give the same string).
const BREAKS = new Set(['/', '-', '_', '.']);
export const breakable = text => String(text).split(/([/\-_.])/).filter(part => part !== '').flatMap(part => (BREAKS.has(part) ? [part, html`<wbr />`] : [part]));

function Fact({ label, value, mono, copy }) {
  const target = useRef(null);
  return html`<div class=${mono ? 'profile-fact profile-fact-mono' : 'profile-fact'}><span class="profile-fact-label">${label}</span>${mono ? html`<code class="profile-fact-value" ref=${target}>${breakable(value)}</code>` : html`<strong class="profile-fact-value" ref=${target}>${label === 'Runtime' ? String(value).split(' · ').flatMap((part, index) => [index ? ' · ' : '', html`<span class="profile-fact-token">${part}</span>`]) : String(value)}</strong>`}${copy && html`<${CopyButton} value=${value} label=${label} target=${target} />`}</div>`;
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
    const sw = switchStatusOf(agent, harnessName), actions = actionsOf(agent, { down, lifecycle }).filter(action => action.name !== 'clear-context' || api.canControl?.() !== false), keep = keepAliveOf(agent);
    const { src, label } = harnessIcon(agent.tool || '');
    const name = agentLabel(agent) || agent.id, dm = `Message ${agent.name || agent.slug || 'agent'} privately`;
    const roleName = agent.role_label || (agent.role || agent.role_key || 'Agent').replace(/[_-]+/g, ' ').replace(/\b\w/g, letter => letter.toUpperCase());
    const reason = key => actions.find(action => action.name === key), quick = quickActionsOf(agent);
    const toggle = () => api.store.toggleProfile(agent.id);
    const dot = dotOf(agent, state);
    return html`<article class=${expanded ? 'agent-card agent-card-compact expanded' : 'agent-card agent-card-compact'} data-agent-id=${agent.id} data-woken-by=${agent.woken_by || ''} data-live-at=${live.at} data-live-label=${live.label}
      onClick=${event => { if (!event.target.closest('button, a, input, select, textarea, .profile-output')) toggle(); }}>
      <div class="agent-id agent-title"><span class=${`agent-online-dot ${stateDot(state.kind)} agent-dot`} data-state=${state.kind} data-dot=${dot.kind}><${DotWords} store=${api.store} agent=${agent} state=${state} /></span>
        <img class="harness-icon agent-harness-icon" alt="" src=${src} title=${`${agent.model || 'unknown'} · ${label}`} /><span class="agent-emoji" aria-hidden="true" title=${roleName}>${agent.role_emoji || ''}</span>
        <span class="agent-identity"><span class="agent-id-text agent-name" title=${name}>${name}</span></span>
        <div class="agent-state" data-unseen=${state.unseen ? 'true' : null} data-since=${state.since} data-state=${state.kind} title=${state.title}><span class=${`agent-state-pill agent-state-badge agent-state-${stateBadge(state.kind)}`}><span class="agent-state-label">${state.name}</span>${' '}<${StateAge} store=${api.store} since=${state.since} hidden=${state.kind === 'stopped' || (!expanded && state.kind === 'idle')} /></span></div>
        <span class="agent-role" title=${agent.role_updated ? 'Role updated; respawn to apply the project definition.' : ''}>${`${agent.role_emoji || ''} ${agent.role || 'Agent'}${agent.role_deleted ? ' (deleted)' : ''}`.trim()}</span>
        <div class="agent-card-actions agent-quick"><${QuickButton} className="agent-private" label=${dm} icon="dm" onClick=${() => api.onPrivate(agent)} />
          ${quick.includes('pause') && html`<${QuickButton} className="agent-pause" label="Pause" icon="pause" disabled=${reason('act-pause').disabled} title=${reason('act-pause').title} onClick=${() => api.store.control(agent.id, 'pause')} />`}
          ${(quick.includes('start') || quick.includes('resume')) && html`<${QuickButton} className=${quick.includes('start') ? 'agent-start' : 'agent-resume'} label=${reason('reactivate').label} icon="play" disabled=${reason('reactivate').disabled} title=${reason('reactivate').title} onClick=${() => api.store.control(agent.id, 'reactivate')} />`}
          ${agent.desired === 'stopped' && quick.includes('start') && html`<${IconButton} className="agent-start-fresh" name="refresh" label="Start fresh" disabled=${reason('start-fresh').disabled} title=${reason('start-fresh').title} onClick=${() => api.store.control(agent.id, 'start-fresh')} />`}
          ${quick.includes('stop') && html`<${QuickButton} className="agent-stop icon-btn-danger" label="Stop" icon="stop" disabled=${reason('act-stop').disabled} title=${reason('act-stop').title} onClick=${() => api.store.control(agent.id, 'stop')} />`}
          ${!expanded && html`<${IconButton} name="chevron-right" label="Open agent profile" className="agent-expand" aria-expanded="false" onClick=${toggle} />`}</div></div>
      <small class="agent-meta">${`${harnessName(agent.tool) || 'unknown'} · ${agent.model || 'unknown'}`}</small>
      <small class="agent-keepalive" hidden=${(!keep && !agent.warm) || (!expanded && !keep?.warning)} title=${keep?.warning || ''}>${[agent.warm?.state === 'sleeping' ? 'Process: sleeping' : agent.warm?.state === 'warm' ? 'Process: warm' : '', keep?.warning || (keep ? `Cache: ${keep.text.replace(/^Cache /, '')}` : '')].filter(Boolean).join(' · ')}</small>
      <small class="agent-role-update">${agent.role_updated ? 'Role updated · respawn to apply' : ''}</small>
      <div class="agent-health">${(agent.health_flags || []).map((flag, index) => html`<span key=${`${flag.code}${index}`} class=${`health-flag health-${flag.code}`} title=${healthLabel(flag)}>${healthLabel(flag)}</span>`)}</div>
      <p class="agent-activity" title=${[activity.last && `Last agent event: ${new Date(activity.last).toLocaleString()}`, activity.seen && `Supervisor seen: ${new Date(activity.seen).toLocaleString()}`].filter(Boolean).join(' · ') || 'No activity time recorded'}>${activity.text}</p>
      <p class="agent-switch-status" hidden=${sw.hidden} onClick=${sw.inbox ? () => document.querySelector('#inbox-toggle')?.click() : null}>${sw.text}</p>
      ${expanded && agent.switch?.state === 'draining' && html`<div class="agent-switch-actions" aria-label="Switch controls">
        ${agent.switch.can_retry && html`<button type="button" class="btn" onClick=${() => api.switchAction(agent.id, 'retry-handoff')}>Retry handoff</button>`}
        ${agent.switch.can_continue && html`<button type="button" class="btn" onClick=${() => api.switchAction(agent.id, 'continue')}>Continue without handoff</button>`}
        ${agent.switch.can_cancel && html`<button type="button" class="btn" onClick=${() => api.switchAction(agent.id, 'cancel')}>Cancel switch</button>`}
      </div>`}
      <small class="agent-boundary-grants" hidden=${grants.hidden}>${grants.text}</small>
      <div class="agent-context" title=${context.title} aria-label=${`Memory: ${context.title}`}><span>Memory</span><span class="agent-context-track" hidden=${!context.known}><span class="agent-context-fill" style=${`width:${context.width}`}></span></span><span class="agent-context-value">${context.value}</span></div>
      <${LiveChip} store=${api.store} live=${live} said=${state.name} />
      <button type="button" class="btn agent-recovery" title="This agent needs a decision: open it in the Inbox" hidden=${!agent.recovery_block} onClick=${() => api.onRecovery(agent)}><span class="icon-mask icon-mask-health" aria-hidden="true"></span> Review in Inbox</button>
      <div class="agent-profile-facts">${expanded && html`<${Profile} agent=${agent} profile=${profile} output=${output} readOutput=${readOutput} store=${api.store} />`}</div>
      <div class="agent-actions">${expanded && readOutput && html`<${IconButton} name="scroll-text" label="Show agent's logs" className="agent-act agent-logs" onClick=${event => api.openLogs(agent, event.currentTarget)} />`}${actions.map(action => html`<button key=${action.name} type="button" class=${`btn-icon ${action.name === 'act-stop' || action.name === 'remove' ? 'icon-btn-danger ' : ''}agent-act agent-${action.name}`} disabled=${action.disabled} title=${action.title} aria-label=${action.disabled ? action.title : action.label}
        onClick=${() => ({ message: () => api.onPrivate(agent), 'act-pause': () => api.store.control(agent.id, 'pause'), 'act-stop': () => api.store.control(agent.id, 'stop'), switch: () => api.openSwitch(agent.id), reactivate: () => api.store.control(agent.id, 'reactivate'), 'start-fresh': () => api.store.control(agent.id, 'start-fresh'), 'clear-context': () => api.store.clearContext(agent), remove: () => api.store.remove(agent) })[action.name]()}><span class=${`icon-mask icon-mask-${{ message: 'message', 'act-pause': 'pause', 'act-stop': 'stop', switch: 'switch', reactivate: 'play', 'start-fresh': 'refresh', 'clear-context': 'clear-context', remove: 'trash' }[action.name]}`} aria-hidden="true"></span></button>`)}</div>
      </article>`;
  }
}

function Human({ human, kind, api }) {
  const mine = human.id === currentUser;
  // The dot is the server's presence answer: green while one of the person's browser sessions has shown real activity in the last ten minutes,
  // grey after it (a `human_presence_changed` event re-reads the list, so the switch happens live, without a reload).
  const dot = human.online ? 'active' : 'offline';
  return html`<article class=${mine ? 'agent-card agent-card-compact human-card is-you' : 'agent-card agent-card-compact human-card'} data-human-id=${human.id}><div class="agent-id agent-title"><span class=${`agent-online-dot ${dot}`} aria-hidden="true" data-state=${human.online ? 'idle' : 'offline'} title=${human.online ? 'Online · active in the last 10 minutes' : 'Offline · no activity in the last 10 minutes'}></span>
    <img class="harness-icon agent-harness-icon" src="/icons/human.svg" alt="" title="human" aria-hidden="true" /><span class="agent-id-text agent-name human-name" ...${mine ? { title: 'You' } : {}}>${human.login}</span><div class="agent-card-actions agent-quick">${!mine && kind !== 'dm' && html`<${QuickButton} className="human-dm" label=${`Message ${human.login} privately`} icon="dm" onClick=${() => api.onPrivateHuman?.(human)} />`}</div><small class="human-role">${human.role || ''}</small></div></article>`;
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
