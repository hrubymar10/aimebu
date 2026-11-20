import { request } from './api.js';

export const teamFlowSource = {
  templates: () => request('/api/team-flows'),
  get: projectID => request(`/api/projects/${encodeURIComponent(projectID)}/team-flow`),
  preview: (projectID, key) => request(`/api/projects/${encodeURIComponent(projectID)}/team-flow/preview`, { method: 'POST', body: { key } }),
  apply: (projectID, key, rev, removeMemberships = []) => request(`/api/projects/${encodeURIComponent(projectID)}/team-flow`, { method: 'PUT', body: { key, rev, ...(removeMemberships.length ? { remove_memberships: removeMemberships } : {}) } }),
  recommendedModels: () => request('/api/team-flows/recommended-models'),
  migration: projectID => request(`/api/projects/${encodeURIComponent(projectID)}/team-flow/migration`),
  cancelMigration: projectID => request(`/api/projects/${encodeURIComponent(projectID)}/team-flow/migration/cancel`, { method: 'POST' }),
  interruptMigration: (projectID, agentID) => request(`/api/projects/${encodeURIComponent(projectID)}/team-flow/migration/interrupt`, { method: 'POST', body: { agent_id: agentID } }),
  referenced: (no, sourceMessageID) => request(`/api/messages/referenced/${encodeURIComponent(no)}?source_message_id=${encodeURIComponent(sourceMessageID)}`),
};
