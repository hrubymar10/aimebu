import { request } from './api.js';

export const teamFlowSource = {
  templates: () => request('/api/team-flows'),
  get: projectID => request(`/api/projects/${encodeURIComponent(projectID)}/team-flow`),
  preview: (projectID, key) => request(`/api/projects/${encodeURIComponent(projectID)}/team-flow/preview`, { method: 'POST', body: { key } }),
  apply: (projectID, key, rev, removeMemberships = []) => request(`/api/projects/${encodeURIComponent(projectID)}/team-flow`, { method: 'PUT', body: { key, rev, ...(removeMemberships.length ? { remove_memberships: removeMemberships } : {}) } }),
  referenced: (no, sourceMessageID) => request(`/api/messages/referenced/${encodeURIComponent(no)}?source_message_id=${encodeURIComponent(sourceMessageID)}`),
};
