// v1's phone navigation: a bottom bar with Rooms, Chat and Agents. It drives the drawers the phone layout already has
// (the rooms drawer, the members drawer, the Inbox view) through their own buttons, which stay in the page but are not
// shown on a phone, so there is one way to open each and no second state to keep in step.
const PHONE = '(max-width: 700px)';

export function initMobileTabs() {
  const bar = document.querySelector('#mobile-tabs');
  if (!bar) return;
  const app = document.querySelector('#app-view'), chat = document.querySelector('.chat'), agentsPanel = document.querySelector('#agents-panel');
  const tabs = new Map([...bar.querySelectorAll('[data-tab]')].map(button => [button.dataset.tab, button]));
  const state = () => app.classList.contains('rooms-open') ? 'rooms' : agentsPanel.classList.contains('mobile-open') ? 'agents' : 'chat';
  const sync = () => {
    const current = app.classList.contains('inbox-open') ? '' : state();
    for (const [name, button] of tabs) { const on = name === current; button.classList.toggle('active', on); if (on) button.setAttribute('aria-current', 'page'); else button.removeAttribute('aria-current'); }
  };
  const closeInbox = () => { if (!document.querySelector('#inbox-view').hidden) document.querySelector('#inbox-toggle').click(); };
  const show = name => {
    if (!matchMedia(PHONE).matches) return;
    closeInbox();
    const now = state();
    if (name === 'rooms' && now !== 'rooms') { if (now === 'agents') document.querySelector('#agents-close').click(); document.querySelector('#rooms-toggle').click(); }
    else if (name === 'chat') { if (now === 'rooms') document.querySelector('#back').click(); if (now === 'agents') document.querySelector('#agents-close').click(); }
    else if (name === 'agents' && now !== 'agents') { if (now === 'rooms') document.querySelector('#back').click(); document.querySelector('#agents-toggle').click(); }
    sync();
  };
  for (const [name, button] of tabs) button.onclick = () => show(name);
  const watch = new MutationObserver(sync);
  watch.observe(app, { attributes: true, attributeFilter: ['class'] });
  watch.observe(agentsPanel, { attributes: true, attributeFilter: ['class'] });
  sync();
}
