import { html } from '../../ui.js';
import { agentState, holderText } from './model.js';

function Group({ title, count, className, label, children }) {
  return html`<div class="room-members-group"><h4 class="room-members-heading">${`${title} (${count})`}</h4><ul class=${`room-members-list ${className}`} aria-label=${label}>${children}</ul></div>`;
}

function Emoji({ value }) {
  return value ? html`<span class="room-member-emoji" aria-hidden="true">${value}</span>` : null;
}

// Read-only: everything the server sends is rendered as text.
export function MembersList({ store }) {
  const members = store.members.value;
  if (!members) return null;
  const humans = members.humans || [], agents = members.agents || [], singletons = members.singletons || [];
  return html`<${Group} title="People" count=${humans.length} className="room-members-people" label="People in this room">${humans.map(human => html`<li class="room-member"><span class="room-member-name">${human.login}</span>${human.role && html`<span class="room-member-role">${human.role}</span>`}</li>`)}<//>
    ${agents.length ? html`<${Group} title="Agents" count=${agents.length} className="room-members-agents" label="Agents in this room">${agents.map(agent => html`<li class="room-member"><${Emoji} value=${agent.role_emoji} /><span class="room-member-name">${agent.slug}</span><span class="room-member-role">${agent.role_label || agent.role_key}</span>${agent.singleton && html`<span class="room-member-badge">singleton</span>`}${agent.role_deleted && html`<span class="room-member-badge">role removed</span>`}<span class="room-member-state">${agentState(agent)}</span></li>`)}<//>` : html`<p class="room-members-empty">No agents in this room.</p>`}
    ${singletons.length > 0 && html`<${Group} title="Singleton roles" count=${singletons.length} className="room-members-singletons" label="Singleton roles">${singletons.map(role => html`<li class="room-member"><${Emoji} value=${role.emoji} /><span class="room-member-name">${role.label || role.role_key}</span><span class="room-member-state">${holderText(role.held_by)}</span></li>`)}<//>`}`;
}
