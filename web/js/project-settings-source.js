import { request } from './api.js';

class HttpProjectSettingsSource {
  projects() { return request('/api/projects'); }
  get(projectID) { return request(`/api/projects/${encodeURIComponent(projectID)}/settings`); }
  update(projectID, fields) { return request(`/api/projects/${encodeURIComponent(projectID)}/settings`, { method: 'PUT', body: fields }); }
  edit(projectID, fields) { return request(`/api/projects/${encodeURIComponent(projectID)}`, { method: 'PATCH', body: fields }); }
}

class MockProjectSettingsSource {
  constructor() { this.values = new Map(); }
  projects() { return request('/api/projects'); }
  async get(projectID) { return { auto_resume_after_clean_diagnostic: this.values.get(projectID) || false, worktree_mode: 'auto' }; }
  async edit(projectID, fields) { return request(`/api/projects/${encodeURIComponent(projectID)}`, { method: 'PATCH', body: fields }); }
  async update(projectID, fields) { if ('auto_resume_after_clean_diagnostic' in fields) this.values.set(projectID, fields.auto_resume_after_clean_diagnostic); return this.get(projectID); }
}

export function createProjectSettingsSource() {
  return new URLSearchParams(location.search).has('mock_project_settings') ? new MockProjectSettingsSource() : new HttpProjectSettingsSource();
}
