import { request } from './api.js';

// The harness and model choices a default agent can name, across the machines of the project (or all of them).
export async function loadSpawnOptions(projectID = '') {
  const query = projectID ? `?project_id=${encodeURIComponent(projectID)}` : '';
  const machines = (await request(`/api/machines${query}`)).machines || [];
  const rows = await Promise.all(machines.map(async machine => {
    try {
      const catalog = await request(`/api/spawn-catalog?machine_id=${encodeURIComponent(machine.id)}`);
      return (catalog.tools || []).flatMap(tool => (tool.models || []).filter(model => tool.launcher_id && model.id).map(model => ({
        launcher_id: tool.launcher_id, model: model.id,
        label: `${machine.name} · ${tool.label} · ${model.label || model.id}`,
      })));
    } catch { return []; }
  }));
  return rows.flat();
}
