// Disk usage in the Health panel: a total, the breakdown by project and by kind, the worktrees with their state, and Clean now, which
// always shows what it would remove before it removes anything. Only the owner or an admin can read it; for everyone else the section stays out.
import { request } from './api.js';

export function formatBytes(bytes) {
  const value = Number(bytes) || 0;
  if (value < 1024) return `${value} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let size = value / 1024, unit = 0;
  while (size >= 1024 && unit < units.length - 1) { size /= 1024; unit++; }
  return `${size >= 100 ? size.toFixed(0) : size >= 10 ? size.toFixed(1) : size.toFixed(2)} ${units[unit]}`;
}

const STATE_LABELS = { active: 'In use', dirty: 'Uncommitted work', merged: 'Merged', orphan: 'Room closed, unmerged', idle: 'Idle', ready: 'Ready', unknown: 'Not checked' };
const ACTION_LABELS = { would_remove: 'Will remove', removed: 'Removed', kept: 'Kept' };

function node(tag, className = '', text = '') { const item = document.createElement(tag); if (className) item.className = className; if (text) item.textContent = text; return item; }

export class DiskUsage {
  constructor({ send = request } = {}) {
    this.send = send;
    this.node = node('section', 'health-disk'); this.node.id = 'health-disk'; this.node.hidden = true;
    this.node.setAttribute('aria-label', 'Disk usage');
    this.ticket = 0;
  }
  async refresh() {
    const ticket = ++this.ticket;
    let report;
    try { report = await this.send('/api/disk'); }
    catch (error) { if (ticket === this.ticket) this.node.hidden = true; return; }
    if (ticket !== this.ticket) return;
    this.report = report; this.node.hidden = false;
    this.render();
  }
  render() {
    const report = this.report;
    const head = node('div', 'health-disk-head');
    head.append(node('strong', '', 'Disk'), node('span', 'health-disk-total', formatBytes(report.total_bytes)));
    const clean = node('button', 'btn health-disk-clean', 'Clean now'); clean.type = 'button'; clean.onclick = () => this.plan();
    head.append(clean);
    const parts = [head];
    if (report.low_disk) parts.push(node('p', 'health-disk-warning', `Only ${formatBytes(report.free_bytes)} free on this disk.`));
    for (const warning of report.warnings || []) parts.push(node('p', 'health-disk-warning', warning));
    if (report.projects?.length) {
      const list = node('ul', 'health-disk-projects'); list.setAttribute('aria-label', 'Disk used by project');
      const peak = Math.max(...report.projects.map(project => project.bytes), 1);
      for (const project of report.projects) {
        const row = node('li'); row.dataset.project = project.project_id;
        const bar = node('span', 'health-disk-bar'); bar.style.setProperty('--share', `${Math.max(2, Math.round(project.bytes / peak * 100))}%`);
        row.append(node('span', 'health-disk-name', project.name || project.project_id), bar, node('span', 'health-disk-size', formatBytes(project.bytes)));
        row.title = `Attachments ${formatBytes(project.attachments)} · Worktrees ${formatBytes(project.worktrees)}`;
        list.append(row);
      }
      parts.push(list);
    }
    const details = node('details', 'health-disk-details'); details.append(node('summary', '', 'Breakdown'));
    const kinds = node('ul', 'health-disk-kinds');
    for (const category of report.categories || []) { const row = node('li'); row.append(node('span', '', category.label), node('span', 'health-disk-size', formatBytes(category.bytes))); kinds.append(row); }
    details.append(kinds);
    if (report.machines?.length) {
      details.append(node('h4', '', 'Agent logs on other machines'));
      const machines = node('ul', 'health-disk-machines'); machines.setAttribute('aria-label', 'Agent logs on other machines');
      for (const machine of report.machines) {
        const row = node('li'); row.dataset.machine = machine.machine_id;
        row.append(node('span', '', machine.name || machine.machine_id), node('span', 'health-disk-state', machine.error || `${machine.agent_log_agents || 0} agent${machine.agent_log_agents === 1 ? '' : 's'}`), node('span', 'health-disk-size', formatBytes(machine.agent_log_bytes)));
        machines.append(row);
      }
      details.append(machines);
    }
    if (report.worktrees?.length) {
      details.append(node('h4', '', 'Worktrees'));
      const trees = node('ul', 'health-disk-worktrees');
      for (const tree of report.worktrees) {
        const row = node('li'); row.dataset.state = tree.state;
        const when = tree.last_activity ? ` · last change ${new Date(tree.last_activity).toLocaleDateString()}` : '';
        row.append(node('span', '', tree.room || tree.room_id), node('span', 'health-disk-state', `${STATE_LABELS[tree.state] || tree.state}${tree.dirty_files ? ` (${tree.dirty_files} files)` : ''}`), node('span', 'health-disk-size', formatBytes(tree.bytes)));
        row.title = `${tree.path}${when}`;
        trees.append(row);
      }
      details.append(trees);
    }
    const foot = node('p', 'health-disk-foot');
    const last = report.last_cleanup ? `Last cleanup ${new Date(report.last_cleanup.created_at).toLocaleString()}: ${report.last_cleanup.reason}` : 'No cleanup has removed anything yet.';
    foot.textContent = `${formatBytes(report.free_bytes)} free of ${formatBytes(report.volume_bytes)}. ${last}`;
    details.append(foot, this.scheduleControls(report.settings));
    parts.push(details);
    // A refresh that lands while a preview is on screen must not wipe it, or the person would confirm something they can no longer see.
    if (!this.panel || this.panel.hidden) { this.panel = node('div', 'health-disk-plan'); this.panel.hidden = true; this.panel.setAttribute('role', 'region'); this.panel.setAttribute('aria-label', 'Cleanup preview'); }
    parts.push(this.panel);
    const open = this.node.querySelector('details')?.open;
    this.node.replaceChildren(...parts);
    if (open) this.node.querySelector('details').open = true;
  }
  scheduleControls(settings) {
    const form = node('div', 'health-disk-schedule');
    const enabled = node('input'); enabled.type = 'checkbox'; enabled.checked = !!settings?.enabled; enabled.id = 'cleanup-enabled';
    const label = node('label', '', 'Clean up automatically every day at '); label.htmlFor = 'cleanup-enabled';
    const hour = node('select'); hour.id = 'cleanup-hour'; hour.setAttribute('aria-label', 'Hour of the daily cleanup');
    for (let h = 0; h < 24; h++) { const option = node('option', '', `${String(h).padStart(2, '0')}:00`); option.value = String(h); hour.append(option); }
    hour.value = String(settings?.hour ?? 3);
    const keep = node('div', 'health-disk-retention');
    const keepLabel = node('label', '', 'Delete attachments older than '); keepLabel.htmlFor = 'cleanup-attachment-days';
    const days = node('input'); days.type = 'number'; days.id = 'cleanup-attachment-days'; days.min = '0'; days.max = '3650'; days.step = '1'; days.value = String(settings?.attachment_days ?? 90);
    days.setAttribute('aria-describedby', 'cleanup-attachment-hint');
    const hint = node('span', 'health-disk-hint', ' days (0 keeps them). The message stays and says the attachment was removed.'); hint.id = 'cleanup-attachment-hint';
    keep.append(keepLabel, days, hint);
    const save = async fields => { try { await this.send('/api/cleanup/settings', { method: 'PUT', body: fields }); } catch (error) { this.show(error.message); } };
    enabled.onchange = () => save({ enabled: enabled.checked });
    hour.onchange = () => save({ hour: Number(hour.value) });
    days.onchange = () => { const value = Math.round(Number(days.value)); if (!Number.isFinite(value) || value < 0 || value > 3650) { days.value = String(settings?.attachment_days ?? 90); this.show('Enter a number of days from 0 to 3650.'); return; } days.value = String(value); save({ attachment_days: value }); };
    const wrap = node('div', 'health-disk-settings'); form.append(enabled, label, hour); wrap.append(form, keep);
    return wrap;
  }
  show(message) { if (this.panel) { this.panel.hidden = false; this.panel.replaceChildren(node('p', 'error', message)); } }
  // Clean now: ask what would be removed, show it, and only a second click removes it.
  async plan() {
    this.panel.hidden = false; this.panel.replaceChildren(node('p', '', 'Checking what can be removed…'));
    let result;
    try { result = await this.send('/api/cleanup', { method: 'POST', body: { dry_run: true } }); }
    catch (error) { this.show(error.message); return; }
    const removable = result.items.filter(item => item.action === 'would_remove');
    const list = node('ul', 'health-disk-items');
    for (const item of result.items) {
      const row = node('li'); row.dataset.action = item.action;
      row.append(node('span', 'health-disk-action', ACTION_LABELS[item.action] || item.action), node('span', 'health-disk-target', item.target.split('/').slice(-2).join('/')), node('span', 'health-disk-size', formatBytes(item.bytes)), node('span', 'health-disk-reason', item.reason));
      row.title = item.target;
      list.append(row);
    }
    const summary = node('p', 'health-disk-summary', removable.length ? `${removable.length} item${removable.length === 1 ? '' : 's'}, ${formatBytes(result.bytes)} would be removed. Nothing has been removed yet.` : 'Nothing to remove right now.');
    const go = node('button', 'btn btn-primary', `Remove ${formatBytes(result.bytes)}`); go.type = 'button'; go.disabled = !removable.length;
    go.onclick = () => this.run();
    const cancel = node('button', 'btn', removable.length ? 'Cancel' : 'Close'); cancel.type = 'button'; cancel.onclick = () => { this.panel.hidden = true; };
    const actions = node('div', 'health-disk-actions'); actions.append(go, cancel);
    this.panel.replaceChildren(summary, list, actions); (removable.length ? go : cancel).focus();
  }
  async run() {
    this.panel.replaceChildren(node('p', '', 'Cleaning up…'));
    try {
      const result = await this.send('/api/cleanup', { method: 'POST', body: { dry_run: false } });
      const removed = result.items.filter(item => item.action === 'removed');
      this.panel.replaceChildren(node('p', 'health-disk-summary', `Removed ${removed.length} item${removed.length === 1 ? '' : 's'}, ${formatBytes(result.bytes)}.`));
      await this.refresh();
      if (this.panel) { this.panel.hidden = false; this.panel.replaceChildren(node('p', 'health-disk-summary', `Removed ${removed.length} item${removed.length === 1 ? '' : 's'}, ${formatBytes(result.bytes)}.`)); }
    } catch (error) { this.show(error.message); }
  }
}
