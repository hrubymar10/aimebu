import { request } from './api.js';

export class HttpQuotaSource {
  load() { return request('/api/quota-switcher'); }
  save(tool, config) { return request(`/api/quota-switcher/${tool}`, { method: 'PUT', body: config }); }
  rearm(tool) { return request(`/api/quota-switcher/${tool}/rearm`, { method: 'POST', body: {} }); }
  reenable(tool) { return request(`/api/quota-switcher/${tool}/reenable`, { method: 'POST', body: {} }); }
}
