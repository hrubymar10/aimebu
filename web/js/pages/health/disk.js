import { html, batch, signal, useLayoutEffect, useRef } from '../../ui.js';
import { request } from '../../api.js';
import { ACTION_LABELS, STATE_LABELS, formatBytes, previewSummary, removedLine, retentionDays } from './model.js';

export class DiskUsage {
  constructor({ send = request } = {}) {
    this.send = send;
    this.report = signal(null);
    this.plan = signal(null);
    this.alive = true;
    this.ticket = 0;
  }

  async refresh() {
    if (!this.alive) return;
    const ticket = ++this.ticket;
    let report;
    try { report = await this.send('/api/disk'); }
    catch { if (this.alive && ticket === this.ticket) this.report.value = null; return; }
    if (this.alive && ticket === this.ticket) this.report.value = report;
  }

  show(message) { if (this.report.peek()) this.plan.value = { error: message }; }

  async save(fields) {
    if (!this.alive) return;
    try { await this.send('/api/cleanup/settings', { method: 'PUT', body: fields }); } catch (failure) { if (this.alive) this.show(failure.message); }
  }

  // Clean now: ask what would be removed, show it, and only a second press removes it.
  async preview() {
    if (!this.alive) return;
    this.plan.value = { checking: true };
    let result;
    try { result = await this.send('/api/cleanup', { method: 'POST', body: { dry_run: true } }); }
    catch (failure) { if (this.alive) this.show(failure.message); return; }
    if (this.alive) this.plan.value = { result };
  }

  async run() {
    if (!this.alive) return;
    this.plan.value = { cleaning: true };
    try {
      const result = await this.send('/api/cleanup', { method: 'POST', body: { dry_run: false } });
      if (!this.alive) return;
      this.plan.value = { done: removedLine(result.items, result.bytes) };
      await this.refresh();
    } catch (failure) { if (this.alive) this.show(failure.message); }
  }

  cancel() { this.plan.value = null; }

  destroy() { this.alive = false; this.ticket++; }
}

function Schedule({ disk, settings }) {
  const days = settings?.attachment_days ?? 90;
  const changeDays = event => {
    const input = event.currentTarget, value = retentionDays(input.value);
    if (value === null) { input.value = String(days); disk.show('Enter a number of days from 0 to 3650.'); return; }
    input.value = String(value); disk.save({ attachment_days: value });
  };
  return html`<div class="health-disk-settings"><div class="health-disk-schedule">
      <input type="checkbox" id="cleanup-enabled" checked=${!!settings?.enabled} onChange=${event => disk.save({ enabled: event.currentTarget.checked })} />
      <label for="cleanup-enabled">Clean up automatically every day at </label>
      <select id="cleanup-hour" aria-label="Hour of the daily cleanup" value=${String(settings?.hour ?? 3)} onChange=${event => disk.save({ hour: Number(event.currentTarget.value) })}>
        ${Array.from({ length: 24 }, (_, hour) => html`<option value=${String(hour)}>${`${String(hour).padStart(2, '0')}:00`}</option>`)}</select></div>
    <div class="health-disk-retention"><label for="cleanup-attachment-days">Delete attachments older than </label>
      <input type="number" id="cleanup-attachment-days" min="0" max="3650" step="1" aria-describedby="cleanup-attachment-hint" defaultValue=${String(days)} onChange=${changeDays} />
      <span class="health-disk-hint" id="cleanup-attachment-hint"> days (0 keeps them). The message stays and says the attachment was removed.</span></div></div>`;
}

function Preview({ disk, plan }) {
  const result = plan.result, removable = result.items.filter(item => item.action === 'would_remove'), focus = useRef(null);
  useLayoutEffect(() => { focus.current?.focus(); }, []);
  return html`<p class="health-disk-summary">${previewSummary(result.items, result.bytes)}</p>
    <ul class="health-disk-items">${result.items.map(item => html`<li data-action=${item.action} title=${item.target}><span class="health-disk-action">${ACTION_LABELS[item.action] || item.action}</span><span class="health-disk-target">${item.target.split('/').slice(-2).join('/')}</span><span class="health-disk-size">${formatBytes(item.bytes)}</span><span class="health-disk-reason">${item.reason}</span></li>`)}</ul>
    <div class="health-disk-actions"><button type="button" class="btn btn-primary" disabled=${!removable.length} ref=${removable.length ? focus : null} onClick=${() => disk.run()}>${`Remove ${formatBytes(result.bytes)}`}</button>
      <button type="button" class="btn" ref=${removable.length ? null : focus} onClick=${() => disk.cancel()}>${removable.length ? 'Cancel' : 'Close'}</button></div>`;
}

function Plan({ disk, plan }) {
  return html`<div class="health-disk-plan" role="region" aria-label="Cleanup preview" hidden=${!plan}>${plan && (plan.error ? html`<p class="error">${plan.error}</p>` : plan.checking ? html`<p>Checking what can be removed…</p>` : plan.cleaning ? html`<p>Cleaning up…</p>`
    : plan.done ? html`<p class="health-disk-summary">${plan.done}</p>` : html`<${Preview} disk=${disk} plan=${plan} />`)}</div>`;
}

export function DiskSection({ disk }) {
  const report = disk.report.value, plan = disk.plan.value;
  if (!report) return html`<section class="health-disk" id="health-disk" aria-label="Disk usage" hidden></section>`;
  const peak = Math.max(...(report.projects || []).map(project => project.bytes), 1);
  const last = report.last_cleanup ? `Last cleanup ${new Date(report.last_cleanup.created_at).toLocaleString()}: ${report.last_cleanup.reason}` : 'No cleanup has removed anything yet.';
  return html`<section class="health-disk" id="health-disk" aria-label="Disk usage">
    <div class="health-disk-head"><strong>Disk</strong><span class="health-disk-total">${formatBytes(report.total_bytes)}</span><button type="button" class="btn health-disk-clean" onClick=${() => disk.preview()}>Clean now</button></div>
    ${report.low_disk && html`<p class="health-disk-warning">${`Only ${formatBytes(report.free_bytes)} free on this disk.`}</p>`}
    ${(report.warnings || []).map(warning => html`<p class="health-disk-warning">${warning}</p>`)}
    ${report.projects?.length ? html`<ul class="health-disk-projects" aria-label="Disk used by project">${report.projects.map(project => html`<li data-project=${project.project_id} title=${`Attachments ${formatBytes(project.attachments)} · Worktrees ${formatBytes(project.worktrees)}`}>
      <span class="health-disk-name">${project.name || project.project_id}</span><span class="health-disk-bar" style=${`--share:${Math.max(2, Math.round(project.bytes / peak * 100))}%`}></span><span class="health-disk-size">${formatBytes(project.bytes)}</span></li>`)}</ul>` : null}
    <details class="health-disk-details"><summary>Breakdown</summary>
      <ul class="health-disk-kinds">${(report.categories || []).map(category => html`<li><span>${category.label}</span><span class="health-disk-size">${formatBytes(category.bytes)}</span></li>`)}</ul>
      ${report.machines?.length ? html`<h4>Agent logs on other machines</h4><ul class="health-disk-machines" aria-label="Agent logs on other machines">${report.machines.map(machine => html`<li data-machine=${machine.machine_id}><span>${machine.name || machine.machine_id}</span>
        <span class="health-disk-state">${machine.error || `${machine.agent_log_agents || 0} agent${machine.agent_log_agents === 1 ? '' : 's'}`}</span><span class="health-disk-size">${formatBytes(machine.agent_log_bytes)}</span></li>`)}</ul>` : null}
      ${report.worktrees?.length ? html`<h4>Worktrees</h4><ul class="health-disk-worktrees">${report.worktrees.map(tree => html`<li data-state=${tree.state} title=${`${tree.path}${tree.last_activity ? ` · last change ${new Date(tree.last_activity).toLocaleDateString()}` : ''}`}>
        <span>${tree.room || tree.room_id}</span><span class="health-disk-state">${`${STATE_LABELS[tree.state] || tree.state}${tree.dirty_files ? ` (${tree.dirty_files} files)` : ''}`}</span><span class="health-disk-size">${formatBytes(tree.bytes)}</span></li>`)}</ul>` : null}
      <p class="health-disk-foot">${`${formatBytes(report.free_bytes)} free of ${formatBytes(report.volume_bytes)}. ${last}`}</p>
      <${Schedule} disk=${disk} settings=${report.settings} /></details>
    <${Plan} disk=${disk} plan=${plan} /></section>`;
}
