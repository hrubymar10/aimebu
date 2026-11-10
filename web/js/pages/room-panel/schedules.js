import { html, signal, batch } from '../../ui.js';
import { request } from '../../api.js';
import { IconButton } from '../../components/icon-button.js';
import { mountPreactPage } from '../mount-page.js';

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

const zone = () => Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';

// The room's schedules: what is held for the room being shown and the calls that change it. Every call carries the generation of the panel (it moves on a reset)
// and the ticket of the last load, and says nothing once either is gone, or once the room is another one.
export class SchedulesStore {
  constructor() {
    this.generation = 0; this.ticket = 0; this.roomID = null; this.closed = false;
    this.visible = signal(false); this.items = signal(null); this.targets = signal([]); this.error = signal(''); this.kind = signal('once'); this.formKey = signal(0); this.busyIDs = signal(new Set());
  }
  path() { return `/api/rooms/${encodeURIComponent(this.roomID)}/schedules`; }
  reset() {
    this.generation++; this.ticket++; this.roomID = null;
    batch(() => { this.items.value = null; this.targets.value = []; this.error.value = ''; this.visible.value = false; this.kind.value = 'once'; this.formKey.value++; this.busyIDs.value = new Set(); });
  }
  async load(room) {
    const generation = this.generation;
    this.roomID = room?.id; this.closed = room?.lifecycle !== 'open'; const ticket = ++this.ticket;
    const visible = room?.kind === 'task' || room?.kind === 'home';
    batch(() => { this.visible.value = visible; this.items.value = null; this.error.value = ''; this.busyIDs.value = new Set(); this.formKey.value++; this.kind.value = 'once'; });
    if (!visible) return;
    try {
      const [data, agents] = await Promise.all([request(this.path()), request(`/api/rooms/${encodeURIComponent(room.id)}/agents`)]);
      if (generation !== this.generation || ticket !== this.ticket) return;
      batch(() => { this.items.value = data.schedules; this.targets.value = agents; });
    } catch (error) { if (generation === this.generation && ticket === this.ticket) this.error.value = error.message; }
  }
  async create(form) {
    if (!this.roomID) return;
    const roomID = this.roomID, generation = this.generation, elements = form.elements, kind = elements.kind.value;
    const body = { body: elements.body.value, kind, timezone: elements.timezone.value.trim(), target_lead: elements.target.value === 'lead', target_agent_id: elements.target.value === 'lead' ? '' : elements.target.value };
    try {
      if (kind === 'once') body.at = zonedOnce(elements.at.value, body.timezone);
      else if (kind === 'interval') body.interval_seconds = Number(elements.minutes.value) * 60;
      else { body.local_time = elements.local_time.value; if (kind === 'weekly') body.weekday = Number(elements.weekday.value); }
      const item = await request(this.path(), { method: 'POST', body });
      if (generation !== this.generation || this.roomID !== roomID || this.roomID !== item.room_id) return;
      batch(() => { this.items.value = [...(this.items.value || []), item]; this.error.value = ''; });
      elements.body.value = '';
    } catch (error) { if (generation === this.generation && this.roomID === roomID) this.error.value = error.message; }
  }
  setBusy(id, on) { const next = new Set(this.busyIDs.value); if (on) next.add(id); else next.delete(id); this.busyIDs.value = next; }
  async act(id, action) {
    const item = this.items.value?.find(row => row.id === id); if (!item) return;
    const roomID = this.roomID, generation = this.generation;
    this.setBusy(id, true);
    try {
      if (action === 'delete') { await request(`${this.path()}/${item.id}`, { method: 'DELETE' }); if (generation !== this.generation || roomID !== this.roomID) return; this.items.value = this.items.value.filter(row => row.id !== item.id); }
      else { const updated = await request(`${this.path()}/${item.id}`, { method: 'PATCH', body: { enabled: !item.enabled, rev: item.rev } }); if (generation !== this.generation || roomID !== this.roomID) return; this.items.value = this.items.value.map(row => (row.id === item.id ? { ...row, ...updated } : row)); }
      this.error.value = '';
    } catch (error) { if (generation === this.generation && roomID === this.roomID) this.error.value = error.message; }
    finally { if (generation === this.generation) this.setBusy(id, false); }
  }
}

const rowText = item => {
  const next = new Date(item.next_run_at).toLocaleString();
  return `${item.body} → ${item.target_lead ? 'room lead' : item.target_agent_id} · ${item.kind} · ${item.timezone} · ${item.enabled ? `next ${next} (your time)` : 'paused'}${item.last_outcome ? ` · last ${item.last_outcome}${item.last_reason ? `: ${item.last_reason}` : ''}` : ''}`;
};

function ScheduleRow({ store, item }) {
  const busy = store.busyIDs.value.has(item.id);
  return html`<div class="schedule-row"><span>${rowText(item)}</span>
    <${IconButton} name=${item.enabled ? 'pause' : 'play'} label=${item.enabled ? 'Pause' : 'Resume'} data-action="toggle" data-id=${item.id} disabled=${busy || (store.closed && !item.enabled)} onClick=${() => store.act(item.id, 'toggle')} />
    <${IconButton} name="trash" label="Delete" danger data-action="delete" data-id=${item.id} disabled=${busy} onClick=${() => store.act(item.id, 'delete')} /></div>`;
}

const WEEKDAYS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];

export function RoomSchedules({ store }) {
  const items = store.items.value, kind = store.kind.value, error = store.error.value;
  return html`<h3>Schedules</h3>
    <div class="schedule-list">${items === null ? null : items.length ? items.map(item => html`<${ScheduleRow} key=${item.id} store=${store} item=${item} />`) : 'No schedules'}</div>
    <form class="schedule-form" key=${store.formKey.value} onSubmit=${event => { event.preventDefault(); store.create(event.currentTarget); }}>
      <label>Note<textarea name="body" maxlength="2000" required></textarea></label>
      <label>Target<select name="target" required><option value="lead">Current room lead</option>${store.targets.value.map(agent => html`<option key=${agent.id} value=${agent.id}>${agent.slug} (${agent.role_key})</option>`)}</select></label>
      <label>Repeat<select name="kind" value=${kind} onChange=${event => { store.kind.value = event.currentTarget.value; }}><option value="once">Once</option><option value="interval">Interval</option><option value="daily">Daily</option><option value="weekly">Weekly</option></select></label>
      <label>Time zone<input name="timezone" required placeholder="Europe/Prague" defaultValue=${zone()} /></label>
      <label class="schedule-once" hidden=${kind !== 'once'}>At<input name="at" type="datetime-local" /></label>
      <label class="schedule-interval" hidden=${kind !== 'interval'}>Every minutes<input name="minutes" type="number" min="5" step="1" defaultValue="60" /></label>
      <label class="schedule-civil" hidden=${!['daily', 'weekly'].includes(kind)}>Local time<input name="local_time" type="time" /></label>
      <label class="schedule-weekly" hidden=${kind !== 'weekly'}>Day<select name="weekday">${WEEKDAYS.map((day, index) => html`<option key=${index} value=${index}>${day}</option>`)}</select></label>
      <button type="submit" class="btn btn-primary" disabled=${store.closed}>Create schedule</button>
    </form>
    <p class="schedule-error error" role="alert">${error}</p>`;
}

// The settings dialog's Schedules section: the same two calls the dialog always made (load the room, reset), over the Preact view in the section's own element.
export class SchedulesPanel {
  constructor(root) {
    this.root = root; this.store = new SchedulesStore();
    root.replaceChildren(); root.hidden = true;
    this.mount = mountPreactPage(root, () => html`<${RoomSchedules} store=${this.store} />`);
  }
  reset() { this.store.reset(); this.root.hidden = true; }
  async load(room) { const loading = this.store.load(room); this.root.hidden = !this.store.visible.peek(); await loading; }
}
