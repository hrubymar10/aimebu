import { request } from './api.js';

export class HttpLimitGuardSource {
  load() { return request('/api/limit-guard'); }
  save(tool, config) { return request(`/api/limit-guard/${tool}`, { method: 'PUT', body: config }); }
}
