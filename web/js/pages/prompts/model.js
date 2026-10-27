// Import-free: scripts/test-prompts-model.cjs runs this module in a vm.

// The parts of an agent's prompt that this screen cannot edit, in the order they sit among the editable sentences.
export const LOCKED = { before: 'Opening line (set by the agent adapter)', afterIdentity: 'Role pointer (adapter)', afterRoleBlock: 'Attachment tool (adapter)', afterRetry: 'Response contract and schema (fixed)', afterInbox: null, afterMentions: 'Acknowledgement rule (adapter)', after: 'Closing line (adapter)' };

export const scopeOptions = projects => [['', 'Global (all projects)'], ...projects.map(project => [project.id, `Project: ${project.name}`])];

// The scope to open on: the one shown before, else global.
export const pickScope = (options, keep) => (options.some(([id]) => id === keep) ? keep : '');

export const layerOf = (slot, scope) => (scope ? slot.project : slot.global);

export function sourceLabel(slot, scope) {
  const text = slot.source === 'default' ? 'Shipped text' : slot.source === 'global' ? 'Global override' : 'Project override';
  return layerOf(slot, scope) ? `${text}${scope ? ' (this project)' : ' (saved here)'}` : text;
}

export const resetLabel = scope => (scope ? 'Reset to global/shipped' : 'Reset to shipped');
export const savedText = slot => `Saved “${slot.title}”.`;
export const resetText = (slot, scope) => `“${slot.title}” is back to the ${scope ? 'next level up' : 'shipped text'}.`;
export const noEditText = scope => (scope ? 'Only a project owner or operator can change this project\'s prompts.' : 'Only the owner or an admin can change the global prompts.');
export const CONFLICT_TEXT = 'This prompt changed elsewhere. The list is reloaded; your text is still in the box, so check it and save again.';
export const countText = (value, max) => `${[...value].length} / ${max}`;

export const savePath = slot => `/api/prompts/${encodeURIComponent(slot.key)}`;
export const saveBody = (slot, scope, value) => ({ project_id: scope, value, rev: layerOf(slot, scope)?.rev || 0 });
export const resetPath = (slot, scope) => `/api/prompts/${encodeURIComponent(slot.key)}?project_id=${encodeURIComponent(scope)}&rev=${layerOf(slot, scope).rev}`;

export function previewText(slots, drafts) {
  const text = key => (drafts.get(key) ?? slots.find(slot => slot.key === key)?.effective ?? '').trim() || slots.find(slot => slot.key === key)?.default || '';
  const part = label => `[${label}]`;
  return [part(LOCKED.before), text('identity'), part(LOCKED.afterIdentity), part(LOCKED.afterRoleBlock), text('retry_notice'), part(LOCKED.afterRetry), text('inbox_ask'), text('mentions'), part(LOCKED.afterMentions), text('human_reply'), text('tail'), part(LOCKED.after)].join('\n\n');
}

// What survives a reload: a draft is kept while it differs from the freshly loaded text, except the slot that was just written.
export function keepDrafts(drafts, slots, except = '') {
  const next = new Map();
  for (const [key, value] of drafts) {
    const slot = slots.find(item => item.key === key);
    if (slot && key !== except && value !== slot.effective) next.set(key, value);
  }
  return next;
}
