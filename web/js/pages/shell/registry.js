// Import-free: scripts/test-settings-registry.cjs runs this module in a vm.
//
// The one list Settings is built from: the navigation (sections -> categories -> subcategories), the URLs, and search all read it. A setting is added here
// once and shows up in all three. scripts/test-settings-registry.cjs fails when a row a page draws has no entry (or an entry points at nothing).

// Where a person's settings live: what is theirs, what is the server's, what is about agents. A section is shown while one of its categories is.
export const SECTIONS = [
  { id: 'personal', title: 'Personal', description: 'How this browser looks and behaves for you' },
  { id: 'server', title: 'Server', description: 'People, accounts, limits and storage for the whole server' },
  { id: 'agents', title: 'Agents', description: 'The roles, prompts, harnesses and machines agents run on' },
];

// page: the section element the category's content is drawn in (one of today's ten pages; see model.js TABS), rights: the TABS key whose tabHidden
// says who sees the category (page by default), blocks: for a page shared by several categories (General), the ids of the blocks this category shows,
// subs: groups inside the category, each a jump to an anchor.
export const CATEGORIES = [
  { id: 'appearance', section: 'personal', title: 'Appearance', icon: 'palette', page: 'general', blocks: ['settings-appearance'], description: 'Theme and the room list preview' },
  { id: 'chat', section: 'personal', title: 'Chat', icon: 'message', page: 'general', blocks: ['chat-preferences'], description: 'Message layout, system events and the debug inspector' },
  { id: 'notifications', section: 'personal', title: 'Notifications', icon: 'bell', page: 'notifications', description: 'Sound and browser notifications for the inbox and chat' },
  { id: 'users', section: 'server', title: 'Users & invites', icon: 'users', page: 'users', description: 'Who has access to a project and the invitations' },
  { id: 'usages', section: 'server', title: 'Accounts & usage', icon: 'chart', page: 'usages', description: 'Tool accounts, usage limits and automatic switching',
    subs: [{ id: 'refresh', title: 'Refresh and alerts', anchor: 'usage-refresh-interval' }, { id: 'daily-limits', title: 'Daily limits per project', anchor: 'default-cost-cap' }] },
  { id: 'telemetry', section: 'server', title: 'Telemetry', icon: 'activity', page: 'telemetry', description: 'What error and performance data is sent, and where' },
  { id: 'storage', section: 'server', title: 'Storage & performance', icon: 'database', page: 'general', rights: 'general', blocks: ['message-retention', 'prompt-cache'],
    description: 'How long messages reach agents, and keeping the prompt cache warm',
    subs: [{ id: 'message-retention', title: 'Message expiry', anchor: 'message-expiry-amount' }, { id: 'prompt-cache', title: 'Prompt cache', anchor: 'agent-keepalive-enabled' }] },
  { id: 'backup', section: 'server', title: 'Backup & restore', icon: 'archive', page: 'general', rights: 'backup', blocks: ['settings-backup'], description: 'Export the instance settings and import them again' },
  { id: 'roles', section: 'agents', title: 'Roles', icon: 'badge', page: 'roles', description: 'The roles an agent can hold and their instructions' },
  { id: 'prompts', section: 'agents', title: 'Prompts', icon: 'text', page: 'prompts', description: 'The wording agents get around their role instructions' },
  { id: 'macros', section: 'agents', title: 'Macros', icon: 'zap', page: 'macros', description: 'Short keys that expand into longer messages' },
  { id: 'harnesses', section: 'agents', title: 'Harnesses & models', icon: 'terminal', page: 'harnesses', description: 'The tools each machine runs, their providers and models' },
  { id: 'machines', section: 'agents', title: 'Machines', icon: 'server', page: 'machines', description: 'The computers agents run on, and pairing a new one' },
  { id: 'isolation', section: 'agents', title: 'Agent isolation', icon: 'shield', page: 'general', rights: 'general', blocks: ['agent-isolation'], description: 'Docker-only runs, reviewer agents and keeping agents warm' },
];

// anchor: the id of the row (or control) in the page, template: the same id as the page's source writes it when it is built from a variable (a quota row is
// one per tool); group/description are what search and the category show. keywords are words a person may type that the name does not hold.
// hidden: a rights key (a TABS key) when a setting is shown to fewer people than its category.
export const SETTINGS = [
  { anchor: 'theme-select', category: 'appearance', group: 'Appearance', name: 'Theme', description: 'Colour scheme for the web UI, saved to your account.', keywords: ['dark', 'light', 'colour', 'color', 'high contrast', 'mode'] },
  { anchor: 'sidebar-preview', category: 'appearance', group: 'Appearance', name: 'Show the last message under every room', description: 'A one-line preview of the newest message under each room.', keywords: ['preview', 'room list', 'sidebar'] },
  { anchor: 'message-layout', category: 'chat', group: 'Messages', name: 'Message layout', description: 'A keeps your own messages on the right; B puts every message on the left.', keywords: ['layout a', 'layout b', 'bubbles', 'alignment'] },
  { anchor: 'pref-system-events', category: 'chat', group: 'Messages', name: 'Show system events', description: 'A bell in the top bar with a feed of joins, spawns, recoveries and quota limits.', keywords: ['bell', 'events', 'feed', 'joins'] },
  { anchor: 'pref-debug-inspector', category: 'chat', group: 'Messages', name: 'Message debug inspector', description: 'A debug button on each message with its raw record.', keywords: ['debug', 'raw', 'inspect', 'developer'] },
  { anchor: 'notification-muted', category: 'notifications', group: 'Sound and alerts', name: 'Mute inbox sound', description: 'A chime for new inbox items and messages that address you.', keywords: ['sound', 'chime', 'audio', 'mute', 'silent'] },
  { anchor: 'notification-permission', category: 'notifications', group: 'Sound and alerts', name: 'Browser notifications', description: 'Operating-system notifications for new inbox items while the tab is in the background.', keywords: ['desktop', 'push', 'permission', 'os'] },
  { anchor: 'notification-text', category: 'notifications', group: 'Sound and alerts', name: 'Show message text in notifications', description: 'Put the start of a chat message in the notification.', keywords: ['privacy', 'lock screen', 'preview'] },
  { anchor: 'users-project', category: 'users', group: 'People and project access', name: 'Project', description: 'The project whose members and invitations are shown.', keywords: ['members', 'access', 'people'] },
  { anchor: 'users-invite-role', category: 'users', group: 'Invitation', name: 'Invite as', description: 'The access an invited person gets: viewer, operator or owner.', keywords: ['invite', 'invitation', 'link', 'viewer', 'operator', 'owner', 'admin', 'accounts'] },
  { anchor: 'usage-warmup-mode', category: 'usages', group: 'Accounts', name: 'Warm idle Claude and Codex accounts', description: 'Start a five-hour session window for eligible inactive profiles.', keywords: ['warm', 'session window', 'profile'] },
  { anchor: 'usage-refresh-interval', category: 'usages', group: 'Refresh and alerts', name: 'Refresh interval (seconds)', description: 'How often the server re-reads provider limits.', keywords: ['poll', 'refresh', 'limits'] },
  { anchor: 'usage-percent-display', category: 'usages', group: 'Refresh and alerts', name: 'Percentages', description: 'Show usage bars as the share still left or already used.', keywords: ['left', 'used', 'bars'] },
  { anchor: 'usage-alert-warning', category: 'usages', group: 'Refresh and alerts', name: 'Usage warning (% remaining)', description: 'One inbox alert per provider window when this much quota remains.', keywords: ['alert', 'threshold', 'quota'] },
  { anchor: 'usage-alert-critical', category: 'usages', group: 'Refresh and alerts', name: 'Usage critical (% remaining)', description: 'A stronger alert at this lower remaining percentage.', keywords: ['alert', 'threshold', 'quota'] },
  { anchor: 'default-cost-cap', category: 'usages', group: 'Daily limits per project', name: 'Cost cap ($)', description: 'Reported cost per project per day.', keywords: ['budget', 'money', 'spend', 'limit'] },
  { anchor: 'default-turn-cap', category: 'usages', group: 'Daily limits per project', name: 'Turn cap', description: 'Turns per project per day.', keywords: ['limit', 'turns'] },
  { template: 'quota-${tool}-enabled', category: 'usages', group: 'Automatic account switching', name: 'Switch accounts automatically', description: 'Move a tool to the first usable profile when the active one runs out of quota.', keywords: ['switch', 'failover', 'quota', 'claude', 'codex', 'profile'] },
  { template: 'quota-${tool}-dry-run', category: 'usages', group: 'Automatic account switching', name: 'Dry run', description: 'Record what would be switched without switching.', keywords: ['simulate', 'test'] },
  { template: 'quota-${tool}-warmup', category: 'usages', group: 'Automatic account switching', name: 'Warm-up (seconds)', description: 'How long every usable profile must report fresh usage before the server acts.', keywords: ['delay'] },
  { template: 'quota-${tool}-dwell', category: 'usages', group: 'Automatic account switching', name: 'Minimum time between switches (minutes)', description: 'The wait after an automatic switch before another one.', keywords: ['cooldown', 'interval'] },
  { template: 'quota-${tool}-protect-${index}', category: 'usages', group: 'Automatic account switching', name: 'Protect a profile', description: 'Keep a profile out of the automatic order.', keywords: ['exclude', 'order'] },
  { template: 'quota-${tool}-after-reset', category: 'usages', group: 'Automatic account switching', name: 'Use a different order after a reset', description: 'Another profile order once the weekly usage resets.', keywords: ['weekly', 'order'] },
  { template: 'quota-${tool}-watched', category: 'usages', group: 'Automatic account switching', name: 'Watched profile', description: 'The profile whose weekly usage is watched for a reset.', keywords: ['weekly'] },
  { template: 'quota-${tool}-drop', category: 'usages', group: 'Automatic account switching', name: 'Drop (points)', description: 'How far the weekly usage must fall to count as a reset.', keywords: ['weekly', 'reset'] },
  { template: 'guard-${tool}-enabled', category: 'usages', group: 'Limits before 100%', name: 'Hold a tool before it runs out', description: 'Use the thresholds below to hold new turns early.', keywords: ['guard', 'hold', 'limit'] },
  { template: 'guard-${tool}-soft', category: 'usages', group: 'Limits before 100%', name: 'Soft threshold (%)', description: 'No new turns from here, and the switcher moves to another profile.', keywords: ['guard'] },
  { template: 'guard-${tool}-hard', category: 'usages', group: 'Limits before 100%', name: 'Hard threshold (%)', description: 'New turns stay held until usage falls.', keywords: ['guard'] },
  { anchor: 'telemetry-enabled', category: 'telemetry', group: 'Telemetry', name: 'Send telemetry', description: 'Errors and performance traces to Sentry.', keywords: ['sentry', 'errors', 'crash', 'privacy', 'analytics'] },
  { anchor: 'telemetry-dsn', category: 'telemetry', group: 'Telemetry', name: 'Sentry DSN', description: 'Where reports go.', keywords: ['sentry', 'endpoint', 'url'] },
  { anchor: 'telemetry-fingerprint', category: 'telemetry', group: 'Identity', name: 'Fingerprint', description: 'The random id that groups your reports, which you can roll.', keywords: ['identity', 'anonymous', 'id', 'roll'] },
  { anchor: 'telemetry-send-name', category: 'telemetry', group: 'Identity', name: 'Send my name', description: 'Attach your login to the reports you send.', keywords: ['privacy', 'login', 'identity'] },
  { anchor: 'telemetry-facts', category: 'telemetry', group: 'Volume', name: 'Volume', description: 'The trace sample rate and what is not traced.', keywords: ['sample rate', 'spans', 'traces'] },
  { anchor: 'message-expiry-amount', category: 'storage', group: 'Messages', name: 'Message expiry', description: 'Messages older than this are hidden from what AI agents are given; they are never deleted.', keywords: ['retention', 'expire', 'hourglass', 'delete', 'history'] },
  { template: 'agent-keepalive-${field}', anchor: 'agent-keepalive-enabled', category: 'storage', group: 'Prompt cache', name: 'Keep the prompt cache warm', description: 'Ping idle agents so their provider prompt cache stays warm.', keywords: ['cache', 'ping', 'keepalive', 'tokens', 'cost', 'claude', 'codex'] },
  { anchor: 'agent-keepalive-claude-minutes', category: 'storage', group: 'Prompt cache', name: 'Claude Code: ping every (minutes)', description: 'The ping interval for Claude Code agents.', keywords: ['cache', 'ping'] },
  { anchor: 'agent-keepalive-codex-minutes', category: 'storage', group: 'Prompt cache', name: 'Codex: ping every (minutes)', description: 'The ping interval for Codex agents.', keywords: ['cache', 'ping'] },
  { anchor: 'agent-keepalive-stop-after-hours', category: 'storage', group: 'Prompt cache', name: 'Stop pinging after (hours without work)', description: 'How long an idle agent is kept warm before the pings stop.', keywords: ['cache', 'idle'] },
  { anchor: 'settings-backup-export', category: 'backup', group: 'Settings backup', name: 'Export settings', description: 'Download the instance settings as a file.', keywords: ['download', 'json', 'save', 'migrate'] },
  { anchor: 'settings-backup-file', category: 'backup', group: 'Settings backup', name: 'Import settings', description: 'Choose a backup file, review what it changes, and apply it.', keywords: ['upload', 'restore', 'json', 'migrate'] },
  { anchor: 'agent-reviewer-enabled', category: 'isolation', group: 'Agents', name: 'Reviewer agents', description: 'Allow new reviewer agents by default.', keywords: ['review', 'default'] },
  { anchor: 'agent-docker-only', category: 'isolation', group: 'Agents', name: 'Only run agents in docker', description: 'Refuse agents that would run directly on a machine.', keywords: ['container', 'sandbox', 'security', 'isolate'] },
  { anchor: 'agent-warm-idle', category: 'isolation', group: 'Agents', name: 'Keep agents warm (minutes)', description: 'How long an idle agent stays running.', keywords: ['idle', 'timeout', 'warm'] },
  { anchor: 'agent-codex-warm', category: 'isolation', group: 'Agents', name: 'Codex warm (experimental)', description: 'Keep Codex agents running between turns.', keywords: ['experimental', 'idle'] },
  { anchor: 'roles-list', category: 'roles', group: 'Roles', name: 'Roles', description: 'Add, edit or remove the roles an agent can hold.', keywords: ['role', 'leader', 'worker', 'reviewer', 'cardinality', 'extends', 'emoji', 'instructions'] },
  { anchor: 'prompt-scope', category: 'prompts', group: 'Prompt scope', name: 'Scope', description: 'Whose prompts you are looking at: global or a project.', keywords: ['global', 'project', 'override'] },
  { anchor: 'prompt-inline-plan', category: 'prompts', group: 'Inline plans', name: 'Inline plans', description: 'Whether the lead always attaches the full plan under a visual plan.', keywords: ['plan', 'visual plan', 'lead'] },
  { anchor: 'prompt-identity', category: 'prompts', group: 'Prompts', name: 'Identity and roster', description: 'Where the agent finds its identity, role and team roster.', keywords: ['prompt', 'wording'] },
  { anchor: 'prompt-retry_notice', category: 'prompts', group: 'Prompts', name: 'Retry notice', description: 'What an interrupted-attempt note means.', keywords: ['prompt', 'wording', 'retry'] },
  { anchor: 'prompt-inbox_ask', category: 'prompts', group: 'Prompts', name: 'Asking a human', description: 'How to ask the human for an answer through the inbox.', keywords: ['prompt', 'wording', 'question'] },
  { anchor: 'prompt-mentions', category: 'prompts', group: 'Prompts', name: 'Mentions', description: 'How to address another agent so it wakes.', keywords: ['prompt', 'wording', 'broadcast'] },
  { anchor: 'prompt-human_reply', category: 'prompts', group: 'Prompts', name: 'Toward people', description: 'When to message a person, and how to answer one who addresses the agent.', keywords: ['prompt', 'wording'] },
  { anchor: 'prompt-tail', category: 'prompts', group: 'Prompts', name: 'Closing guidance', description: 'The last shared sentence before the adapter\'s own closing.', keywords: ['prompt', 'wording'] },
  { anchor: 'prompt-preview', category: 'prompts', group: 'Preview', name: 'Preview', description: 'The assembled prompt a turn would get.', keywords: ['prompt', 'assembled'] },
  { anchor: 'macros-list', category: 'macros', group: 'Macros', name: 'Macros', description: 'Short keys that expand into longer messages; import, export and search them.', keywords: ['macro', 'snippet', 'shortcut', 'canned', 'quick command', 'import', 'copy all'] },
  { anchor: 'harness-machine', category: 'harnesses', group: 'Harnesses & models', name: 'Machine', description: 'The supervisor whose harnesses and recent changes are shown.', keywords: ['supervisor', 'providers', 'models', 'claude', 'codex', 'cursor', 'role defaults'] },
  { anchor: 'machines-list', category: 'machines', group: 'Machines', name: 'Machines', description: 'The computers agents run on; pair, approve or revoke one.', keywords: ['pair', 'supervisor', 'approve', 'revoke', 'host', 'computer'] },
];

// Settings.open(section) took one of the ten page names; they keep working and land on the category that now holds the page.
export const ALIASES = {
  general: 'personal/appearance', usages: 'server/usages', notifications: 'personal/notifications', telemetry: 'server/telemetry', roles: 'agents/roles',
  users: 'server/users', prompts: 'agents/prompts', macros: 'agents/macros', machines: 'agents/machines', harnesses: 'agents/harnesses',
};

// The ids a page's rows may have without being a setting of their own: the editors and dialogs a category opens (a role, a macro, an import box).
export const TRANSIENT_ID_PREFIXES = ['role-', 'macro-', 'macros-'];

const categoryOf = id => CATEGORIES.find(category => category.id === id);
export const sectionOf = id => SECTIONS.find(section => section.id === id);
export const categoryById = categoryOf;
export const categoriesOf = section => CATEGORIES.filter(category => category.section === section);
export const categoryRights = category => category.rights || category.page;

// The path a category (and a sub) has in the address: settings/<section>/<category>[/<sub>].
export const categoryPath = (category, sub = '') => `settings/${category.section}/${category.id}${sub ? `/${sub}` : ''}`;
export const settingCategory = setting => categoryOf(setting.category);
export const settingPath = setting => `${sectionOf(settingCategory(setting).section).title} › ${settingCategory(setting).title} › ${setting.name}`;

// The hash (or path) a person typed or followed, as {section, category, sub} -- each '' when it is not there; null when it is not a settings path at all.
export function parsePath(value) {
  const text = String(value || '').replace(/^#/, '');
  if (text !== 'settings' && !text.startsWith('settings/')) return null;
  const [, sectionId = '', categoryId = '', sub = ''] = text.split('/').map(part => { try { return decodeURIComponent(part); } catch { return ''; } });
  const category = categoryOf(categoryId);
  if (!category || category.section !== sectionId) return { section: sectionOf(sectionId) ? sectionId : '', category: '', sub: '' };
  return { section: sectionId, category: categoryId, sub };
}

// A page name from an old call (settings.open('usages')) or a path, as {section, category, sub}.
export function resolveTarget(value) {
  const parsed = parsePath(value);
  if (parsed) return parsed;
  const alias = ALIASES[value];
  return alias ? parsePath(`settings/${alias}`) : { section: '', category: '', sub: '' };
}
