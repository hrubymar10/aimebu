// Sender identity for a message header: harness icon, model tooltip and a
// liveness dot. Historical messages can retain the sender's harness and model.
const TOOL_ICONS = { claude: 'claude-code', 'claude-code': 'claude-code', codex: 'codex', pi: 'pi', opencode: 'opencode', cursor: 'cursor', cline: 'cline', vibe: 'vibe' };
const TOOL_LABELS = { claude: 'Claude Code', 'claude-code': 'Claude Code', codex: 'Codex', pi: 'Pi', opencode: 'OpenCode', cursor: 'Cursor', cline: 'Cline', vibe: 'Mistral Vibe' };

// An AI agent's name is always written with its project, "slug@project" (a human is just the login): the project names which of several
// same-named agents is meant. The project's name comes from the app (setProjectNames); an agent whose project is unknown is its slug.
let projectName = () => '';
export function setProjectNames(lookup) { projectName = lookup; }
export function agentLabel(agent) {
  const slug = agent?.slug || agent?.name || '';
  const project = slug && agent?.project_id ? projectName(agent.project_id) : '';
  return project ? `${slug}@${project}` : slug;
}

// The one place a tool name becomes an icon and a label: member cards, profiles and message headers all read it.
// A tool nobody has heard of gets the unknown icon; the icon is never left out.
export function harnessIcon(tool) {
  return { src: TOOL_ICONS[tool] ? `/icons/${TOOL_ICONS[tool]}.svg` : '/icons/unknown.svg', label: TOOL_LABELS[tool] || tool || 'unknown' };
}

// The dot answers "will this agent respond?": stopped/lost are red, an agent
// waiting on the owner (recovery or boundary approval) is amber, the rest green.
export function livenessDot(agent) {
  if (!agent) return { cls: 'dot-departed', title: 'departed' };
  if (agent.observed === 'stopped' || agent.observed === 'lost') return { cls: 'dot-offline', title: agent.observed };
  if (agent.recovery_block || agent.boundary_pending) return { cls: 'dot-stale', title: agent.boundary_pending ? 'awaiting approval' : 'needs recovery' };
  return { cls: 'dot-active', title: agent.observed === 'in_turn' ? 'working' : 'idle' };
}

// A sender with no live record is either one that was in a v2 room and left (its id is the native "agt_..." one), or one that only
// exists in imported v1 history (its id is the readable name v1 used). The first is "departed", the second "historical".
export function senderKind(message, agent) {
  if (agent || message.author_kind !== 'agent') return 'live';
  return String(message.author_id).startsWith('agt_') ? 'departed' : 'historical';
}

export function senderIdentity(message, agent) {
  if (message.author_kind === 'user') return { icon: '/icons/human.svg', alt: 'human', title: 'human', dot: null };
  const tool = agent?.tool || message.author_harness || '';
  const { src: icon, label: harness } = harnessIcon(tool);
  const kind = senderKind(message, agent);
  const known = message.author_model || message.author_harness;
  const title = agent ? `${agent.model || 'unknown'} · ${harness}` : known ? `${message.author_model || 'unknown'} · ${harness} (${kind})` : `unknown (${kind})`;
  const dot = kind === 'historical' ? { cls: 'dot-historical', title: 'historical' } : livenessDot(agent);
  return { icon, alt: harness, title, dot, kind };
}

// Create the icon + dot elements once; applySenderIdentity updates them in place
// so a liveness change never re-renders the message.
export function createSenderPresence() {
  const wrap = document.createElement('span'); wrap.className = 'msg-sender-presence';
  const icon = document.createElement('img'); icon.className = 'msg-sender-icon'; icon.width = 14; icon.height = 14;
  const dot = document.createElement('span'); dot.className = 'msg-sender-dot';
  wrap.append(icon, dot);
  return wrap;
}

export function applySenderIdentity(wrap, identity) {
  const icon = wrap.querySelector('.msg-sender-icon'), dot = wrap.querySelector('.msg-sender-dot');
  icon.src = identity.icon; icon.alt = identity.alt; icon.title = identity.title;
  dot.hidden = !identity.dot;
  if (identity.dot) { dot.className = `msg-sender-dot ${identity.dot.cls}`; dot.title = identity.dot.title; }
}
