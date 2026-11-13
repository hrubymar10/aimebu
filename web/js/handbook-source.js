import { request } from './api.js';

const projectPath = projectID => `/api/projects/${encodeURIComponent(projectID)}/handbook`;

export const handbookSource = {
  get: projectID => request(projectPath(projectID)),
  history: (projectID, beforeRev = null) => request(`${projectPath(projectID)}/history?limit=20${beforeRev == null ? '' : `&before_rev=${encodeURIComponent(beforeRev)}`}`),
  revision: (projectID, rev) => request(`${projectPath(projectID)}/history/${encodeURIComponent(rev)}`),
};
