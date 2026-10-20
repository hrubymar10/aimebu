import { request } from './api.js';

// The Members section of the room dialog: who is in the room (people with their project role, agents with their role
// emoji and label) and who holds each singleton role. Read-only; everything the server sends is rendered as text.
function node(tag, className = '', text = '') {
  const item = document.createElement(tag);
  if (className) item.className = className;
  if (text) item.textContent = text;
  return item;
}

// The same words the Agents panel shows for an agent's control and observed state.
function agentState(agent) {
  if (agent.revoked) return 'Revoked';
  const interrupted = agent.observed === 'interrupting' || (agent.desired === 'stopped' && agent.observed !== 'stopped');
  const control = interrupted ? 'Interrupt requested' : agent.desired === 'stopped' ? 'Stopped' : agent.desired === 'not_accepting' ? 'Not accepting' : 'Active';
  const observed = agent.observed === 'in_turn' ? 'Working' : agent.observed === 'lost' ? 'Lost' : agent.observed === 'stopped' ? 'Stopped' : agent.observed === 'interrupting' ? 'Interrupting' : 'Idle';
  return `${control} · ${observed}`;
}

export class RoomMembers {
  constructor(root) {
    this.root = root;
    this.status = root.querySelector('.room-members-status');
    this.body = root.querySelector('.room-members-body');
    this.generation = 0;
  }
  reset() { this.generation++; this.status.textContent = ''; this.body.replaceChildren(); }

  // Only the newest load for the open room may render: a slow answer for a room that was closed or replaced is dropped.
  async load(roomID) {
    const generation = ++this.generation;
    this.status.textContent = 'Loading members…'; this.body.replaceChildren();
    let members;
    try { members = await request(`/api/rooms/${roomID}/members`); }
    catch (error) { if (generation === this.generation) { this.status.textContent = error.message || 'Could not load the member list'; } return; }
    if (generation !== this.generation) return;
    this.status.textContent = '';
    this.render(members);
  }

  group(title, count, list) {
    const section = node('div', 'room-members-group');
    const heading = node('h4', 'room-members-heading', `${title} (${count})`);
    section.append(heading, list);
    return section;
  }

  render(members) {
    const people = node('ul', 'room-members-list room-members-people'); people.setAttribute('aria-label', 'People in this room');
    for (const human of members.humans || []) {
      const item = node('li', 'room-member');
      item.append(node('span', 'room-member-name', human.login));
      if (human.role) item.append(node('span', 'room-member-role', human.role));
      people.append(item);
    }
    const agents = node('ul', 'room-members-list room-members-agents'); agents.setAttribute('aria-label', 'Agents in this room');
    for (const agent of members.agents || []) {
      const item = node('li', 'room-member');
      if (agent.role_emoji) { const emoji = node('span', 'room-member-emoji', agent.role_emoji); emoji.setAttribute('aria-hidden', 'true'); item.append(emoji); }
      item.append(node('span', 'room-member-name', agent.slug), node('span', 'room-member-role', agent.role_label || agent.role_key));
      if (agent.singleton) item.append(node('span', 'room-member-badge', 'singleton'));
      if (agent.role_deleted) item.append(node('span', 'room-member-badge', 'role removed'));
      item.append(node('span', 'room-member-state', agentState(agent)));
      agents.append(item);
    }
    const children = [this.group('People', (members.humans || []).length, people)];
    if (!(members.agents || []).length) children.push(node('p', 'room-members-empty', 'No agents in this room.'));
    else children.push(this.group('Agents', members.agents.length, agents));
    if ((members.singletons || []).length) {
      const roles = node('ul', 'room-members-list room-members-singletons'); roles.setAttribute('aria-label', 'Singleton roles');
      for (const role of members.singletons) {
        const item = node('li', 'room-member');
        if (role.emoji) { const emoji = node('span', 'room-member-emoji', role.emoji); emoji.setAttribute('aria-hidden', 'true'); item.append(emoji); }
        item.append(node('span', 'room-member-name', role.label || role.role_key));
        const holder = role.held_by;
        item.append(node('span', 'room-member-state', holder ? `held by ${holder.slug}${holder.in_this_room ? '' : ' (another room)'}` : 'vacant'));
        roles.append(item);
      }
      children.push(this.group('Singleton roles', members.singletons.length, roles));
    }
    this.body.replaceChildren(...children);
  }
}
