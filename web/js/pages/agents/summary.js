import { html } from '../../ui.js';
import { summaryOf } from './summary-model.js';

// The stuck-or-dead summary of one agent, from the panel's store: its state chip, how long since its last activity (ticking with the store's
// clock), how long its turn runs, its last event, its health flags and what its last process left. It reads the clock and the agent itself, so a
// tick re-renders this line and nothing around it (the log text under it stays as it is). Used at the top of the log popup; any card can show it.
export function AgentSummary({ store, agentID }) {
  const agent = store.agents.value.get(agentID);
  if (!agent) return null;
  const summary = summaryOf(agent, store.now.value);
  return html`<div class="agent-summary" data-state=${summary.chip.kind} role="group" aria-label="Agent status">
    <span class=${`agent-summary-chip agent-summary-${summary.chip.kind}`}>${summary.chip.label}</span>
    <span class="agent-summary-fact agent-summary-activity">${summary.activity}</span>
    ${summary.turn && html`<span class="agent-summary-fact agent-summary-turn">${summary.turn}</span>`}
    ${summary.event && html`<span class="agent-summary-fact agent-summary-event">${`Last event: ${summary.event}`}</span>`}
    ${summary.flags.map(flag => html`<span key=${flag.code} class=${`agent-summary-flag health-flag health-${flag.code}`}>${flag.label}</span>`)}
    ${summary.exit && html`<span class="agent-summary-exit" title=${summary.exitFull}>${`Last exit: ${summary.exit}`}</span>`}
  </div>`;
}
