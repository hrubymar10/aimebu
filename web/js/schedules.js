import { request } from './api.js';

// Convert a wall-clock entry in an explicit IANA zone to its first matching
// instant. A missing DST minute advances to the next valid local minute.
export function zonedOnce(localValue, zone) {
  const match = /^(\d{4})-(\d\d)-(\d\d)T(\d\d):(\d\d)$/.exec(localValue);
  if (!match) throw new Error('Choose a date and time.');
  const [, y, m, d, h, min] = match.map(Number);
  const formatter = new Intl.DateTimeFormat('en-CA', { timeZone: zone, year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hourCycle: 'h23' });
  const requested = `${y}-${String(m).padStart(2,'0')}-${String(d).padStart(2,'0')}T${String(h).padStart(2,'0')}:${String(min).padStart(2,'0')}`;
  const center = Date.UTC(y,m-1,d,h,min);
  for (let instant = center - 18*3600000; instant <= center + 18*3600000; instant += 60000) {
    const parts = Object.fromEntries(formatter.formatToParts(instant).filter(part => part.type !== 'literal').map(part => [part.type, part.value]));
    const wall = `${parts.year}-${parts.month}-${parts.day}T${parts.hour}:${parts.minute}`;
    if (wall >= requested && wall.slice(0,10) === requested.slice(0,10)) return new Date(instant).toISOString();
  }
  throw new Error('The selected date does not exist in that time zone.');
}

export class Schedules {
  constructor(root) {
    this.root = root; this.form = root.querySelector('form'); this.list = root.querySelector('.schedule-list'); this.error = root.querySelector('.schedule-error');
    this.form.elements.timezone.value = Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
    this.form.elements.kind.onchange = () => this.fields();
    this.form.onsubmit = event => { event.preventDefault(); this.create(); };
    this.list.onclick = event => this.action(event);
    this.fields();
  }
  fields() {
    const kind = this.form.elements.kind.value;
    this.root.querySelector('.schedule-once').hidden = kind !== 'once';
    this.root.querySelector('.schedule-interval').hidden = kind !== 'interval';
    this.root.querySelector('.schedule-civil').hidden = !['daily','weekly'].includes(kind);
    this.root.querySelector('.schedule-weekly').hidden = kind !== 'weekly';
  }
  path() { return `/api/rooms/${encodeURIComponent(this.roomID)}/schedules`; }
  reset() { this.generation = (this.generation || 0) + 1; this.ticket = (this.ticket || 0) + 1; this.roomID = null; this.items = []; this.list.replaceChildren(); this.error.textContent = ''; this.root.hidden = true; this.form.reset(); this.form.elements.timezone.value = Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'; this.form.elements.target.replaceChildren(); this.fields(); }
  async load(room) {
    const generation = this.generation;
    this.roomID = room?.id; this.closed = room?.lifecycle !== 'open'; const ticket = this.ticket = (this.ticket || 0) + 1; this.root.hidden = room?.kind !== 'task' && room?.kind !== 'home'; this.list.replaceChildren(); this.error.textContent = '';
    this.form.querySelector('button[type=submit]').disabled = this.closed;
    if (this.root.hidden) return;
    try {
      const [data, agents] = await Promise.all([request(this.path()), request(`/api/rooms/${encodeURIComponent(room.id)}/agents`)]);
      if (generation !== this.generation || ticket !== this.ticket) return;
      this.items = data.schedules;
      const select = this.form.elements.target; select.replaceChildren();
      const lead = document.createElement('option'); lead.value = 'lead'; lead.textContent = 'Current room lead'; select.append(lead);
      for (const agent of agents) { const option = document.createElement('option'); option.value = agent.id; option.textContent = `${agent.slug} (${agent.role_key})`; select.append(option); }
      this.render();
    } catch (error) { if (generation === this.generation && ticket === this.ticket) this.error.textContent = error.message; }
  }
  render() {
    this.list.replaceChildren();
    if (!this.items?.length) { this.list.textContent = 'No schedules'; return; }
    for (const item of this.items) {
      const row = document.createElement('div'); row.className = 'schedule-row';
      const info = document.createElement('span');
      const next = new Date(item.next_run_at).toLocaleString();
      info.textContent = `${item.body} → ${item.target_lead ? 'room lead' : item.target_agent_id} · ${item.kind} · ${item.timezone} · ${item.enabled ? `next ${next} (your time)` : 'paused'}${item.last_outcome ? ` · last ${item.last_outcome}${item.last_reason ? `: ${item.last_reason}` : ''}` : ''}`;
      const toggle = document.createElement('button'); toggle.type = 'button'; toggle.className = 'btn'; toggle.textContent = item.enabled ? 'Pause' : 'Resume'; toggle.dataset.action = 'toggle'; toggle.dataset.id = item.id; toggle.disabled = this.closed && !item.enabled;
      const remove = document.createElement('button'); remove.type = 'button'; remove.className = 'btn btn-danger'; remove.textContent = 'Delete'; remove.dataset.action = 'delete'; remove.dataset.id = item.id;
      row.append(info,toggle,remove); this.list.append(row);
    }
  }
  async create() {
    if (!this.roomID) return;
    const roomID = this.roomID, generation = this.generation;
    const form = this.form.elements; const kind = form.kind.value;
    const body = { body: form.body.value, kind, timezone: form.timezone.value.trim(), target_lead: form.target.value === 'lead', target_agent_id: form.target.value === 'lead' ? '' : form.target.value };
    try {
      if (kind === 'once') body.at = zonedOnce(form.at.value, body.timezone);
      else if (kind === 'interval') body.interval_seconds = Number(form.minutes.value)*60;
      else { body.local_time = form.local_time.value; if (kind === 'weekly') body.weekday = Number(form.weekday.value); }
      const item = await request(this.path(), { method:'POST', body });
      if (generation !== this.generation || this.roomID !== roomID || this.roomID !== item.room_id) return;
      this.items.push(item); this.render(); form.body.value = ''; this.error.textContent = '';
    } catch (error) { if (generation === this.generation && this.roomID === roomID) this.error.textContent = error.message; }
  }
  async action(event) {
    const button = event.target.closest('button[data-action]'); if (!button) return;
    const item = this.items?.find(item => item.id === button.dataset.id); if (!item) return;
    const roomID = this.roomID, generation = this.generation;
    button.disabled = true;
    try {
      if (button.dataset.action === 'delete') { await request(`${this.path()}/${item.id}`, { method:'DELETE' }); if (generation !== this.generation || roomID !== this.roomID) return; this.items = this.items.filter(row => row.id !== item.id); }
      else { const updated = await request(`${this.path()}/${item.id}`, { method:'PATCH', body:{enabled:!item.enabled,rev:item.rev} }); if (generation !== this.generation || roomID !== this.roomID) return; Object.assign(item,updated); }
      this.error.textContent = ''; this.render();
    } catch (error) { if (generation === this.generation && roomID === this.roomID) { this.error.textContent = error.message; button.disabled = false; } }
  }
}
