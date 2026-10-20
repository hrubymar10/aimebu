// The opt-in mock uses the same list/answer/subscribe interface as the HTTP
// source, so browser smoke tests exercise the inbox view independently.
import { request } from './api.js';
const fixture = [
  { id: 'mock_approval', project_id: '', room_id: '', message_id: '', kind: 'approval', question: 'Approve the proposed change?', options: ['Approve', 'Revise'], free_text: true, state: 'open' },
  { id: 'mock_question', project_id: '', room_id: '', message_id: '', kind: 'question', question: 'Which option should the team use?', options: ['Option A', 'Option B'], free_text: true, state: 'open' },
];
const recoveryFixture = [
  { id: 'mock_recovery', project_id: '', room_id: '', kind: 'recovery', room_lifecycle: 'closed', question: 'Agent needs recovery', evidence: 'Work may have partially completed.', diagnostic_report: { clean: true, observed: ['files inspected'] }, turn_id: 'turn_mock', rev: 1, state: 'open' },
  { id: 'mock_budget', project_id: '', room_id: '', kind: 'recovery', question: 'Agent reached the turn limit', evidence: '20 turns in ten minutes.', rev: 1, state: 'open' },
];
const quiescenceFixture = [
  { id: 'mock_quiescence', project_id: '', room_id: '', kind: 'recovery', recovery_phase: 'quiescence_wait', question: 'Waiting for the old process to be confirmed gone', turn_id: 'turn_lost', rev: 1, state: 'open' },
];
const deliveryFixture = [
  { id: 'mock_delivery', project_id: '', room_id: '', kind: 'delivery_failed', question: 'An agent reply could not be delivered.', held_reason: 'The target room was closed.', candidate_preview: '{"messages":[{"body":"The requested review is ready."}]}', can_decide: true, rev: 1, state: 'open' },
];
// Delivery failures whose payloads are raw JSON, two of them in an archived room: the Inbox must show plain text and offer a bulk dismiss.
const resolvedFixture = [
  { id: 'mock_resolved_a', project_id: '', room_id: '', kind: 'delivery_failed', room_lifecycle: 'closed', question: '{"messages":[{"body":"The summary you asked for is ready."}]}', delivery_error: '{"error":"validation_failed","code":3}', held_reason: 'invalid_after_correction', can_decide: true, rev: 1, state: 'open' },
  { id: 'mock_resolved_b', project_id: '', room_id: '', kind: 'delivery_failed', room_lifecycle: 'closed', question: 'An agent reply could not be delivered.', evidence: '{"exit":17,"signal":null,"reason":"checkout missing"}', can_decide: true, rev: 1, state: 'open' },
  { id: 'mock_open_failure', project_id: '', room_id: '', kind: 'delivery_failed', room_lifecycle: 'open', question: 'An agent reply could not be delivered.', can_decide: true, rev: 1, state: 'open' },
];
const providerLimitFixture = [
  { id: 'mock_provider_limit', project_id: '', room_id: '', kind: 'usage_info', question: 'Codex reached its provider limit.', options: ['Retry now'], rev: 0, state: 'open' },
];
const boundaryFixture = [
  { id: 'mock_boundary', project_id: '', room_id: '', kind: 'approval', boundary_pending: true,
    boundary_machine: 'Remote workstation', boundary_path: '/work/project', boundary_requester: 'lead',
    question: 'Approve agent on remote machine?', can_decide: true, rev: 0, state: 'open' },
];

export class MockInboxSource {
  constructor(seed = false) { this.items = seed ? fixture.map(item => ({ ...item })) : []; this.listeners = new Set(); }
  async list() { return this.items.filter(item => item.state === 'open'); }
  async answer(id, value) { const item = this.items.find(entry => entry.id === id); if (!item || item.state !== 'open') throw new Error('This item is already resolved'); item.state = 'resolved'; item.answer = value; this.notify(); }
  async decision(id, choice, requestKey, rev) {
    const item = this.items.find(entry => entry.id === id);
    if (!item || item.state !== 'open') throw new Error('This item is already resolved');
    if (!requestKey || rev !== item.rev) throw new Error('Missing decision key or revision');
    if (this.staleOnce) {
      this.staleOnce = false; this.rejectedKey = requestKey; item.rev += 1;
      const error = new Error('The item changed'); error.code = 'stale_rev'; throw error;
    }
    if (requestKey === this.rejectedKey) throw new Error('Reused key after stale revision');
    item.state = 'resolved'; item.choice = choice; this.notify();
  }
  async dismiss(id, rev) {
    const item = this.items.find(entry => entry.id === id);
    if (!item || item.state !== 'open' || item.rev !== rev) throw new Error('This item changed');
    item.state = 'resolved'; this.notify();
  }
  setLocation(projectID, roomID, messageID) { for (const item of this.items) { item.project_id = projectID; item.room_id = roomID; item.message_id = messageID; } this.notify(); }
  subscribe(listener) { this.listeners.add(listener); return () => this.listeners.delete(listener); }
  notify() { for (const listener of this.listeners) listener(); }
}
export class HttpInboxSource {
  constructor() { this.listeners = new Set(); }
  list() { return request('/api/inbox'); }
  async answer(id, value, requestKey, rev) {
    const result = await request(`/api/inbox/${id}/answer`, { method: 'POST', headers: { 'Idempotency-Key': requestKey }, body: { body: value, rev } });
    await this.changed(); return result;
  }
  async decision(id, choice, requestKey, rev) {
    const result = await request(`/api/inbox/${id}/decision`, { method: 'POST', headers: { 'Idempotency-Key': requestKey }, body: { choice, rev } });
    await this.changed(); return result;
  }
  async dismiss(id, rev) {
    const result = await request(`/api/inbox/${id}/dismiss`, { method: 'POST', body: { rev } });
    await this.changed(); return result;
  }
  subscribe(listener) { this.listeners.add(listener); return () => this.listeners.delete(listener); }
  changed() { return Promise.all([...this.listeners].map(listener => listener())); }
}
export function createInboxSource() {
  const query = new URLSearchParams(location.search);
  if (query.has('mock_readonly_recovery')) { const source = new MockInboxSource(); source.items = [{ ...recoveryFixture[0], can_decide: false }]; return source; }
  if (query.has('mock_quiescence')) { const source = new MockInboxSource(); source.items = quiescenceFixture.map(item => ({ ...item })); return source; }
  if (query.has('mock_resolved')) { const source = new MockInboxSource(); source.items = resolvedFixture.map(item => ({ ...item })); window.__mockInbox = source; return source; }
  if (query.has('mock_delivery')) { const source = new MockInboxSource(); source.items = deliveryFixture.map(item => ({ ...item })); return source; }
  if (query.has('mock_provider_limit')) { const source = new MockInboxSource(); source.items = providerLimitFixture.map(item => ({ ...item })); return source; }
  if (query.has('mock_boundary')) { const source = new MockInboxSource(); source.items = boundaryFixture.map(item => ({ ...item })); return source; }
  if (query.has('mock_recovery')) { const source = new MockInboxSource(); source.items = recoveryFixture.map(item => ({ ...item })); source.staleOnce = query.has('mock_stale'); return source; }
  if (query.has('mock_inbox')) {
    const source = new MockInboxSource(true);
    // A held answer for the mock reviewer, so the health list has a delivery item to open.
    if (query.has('mock_held')) source.items.push({ ...deliveryFixture[0], id: 'mock_held', agent_id: 'agt_mock_reviewer' });
    return source;
  }
  return new HttpInboxSource();
}
