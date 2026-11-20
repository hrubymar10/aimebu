# v2 markup → v1 class names

v2 is moving onto v1's stylesheet: `web/v1.css` started as v1's `style.css` at v1 commit 466852f (without fleets and the leaderboard). Since the
cutover it is this repo's own source of truth: it is edited directly and is no longer synced from, or checked against, v1 (the former
`scripts/sync-v1-css.cjs` and its hold list `scripts/v1-css-hold.json` are gone). An area moves over when its markup carries v1's class names and its v2
rules are deleted. Things v1 never had go in `web/v2-only.css`, written with v1's tokens. This page is the map of the renames and the list of
differences from v1 that are kept on purpose, so a review does not flag them.

**Cascade order.** `v1.css` (loaded first) declares `@layer v2-base, v1-reset, v1;`. `v2-base` holds the legacy `style.css` and the generic element and
button look in `theme-v1.css`; `v1-reset` holds v1's universal reset, applied only inside the areas that have moved over (the send bar, the room rows and the top bar's left side so far); `v1`
holds v1's rules. A later layer beats an earlier one whatever the selectors, so v1's class rules always win over v2's base. Everything unlayered
(the rest of `theme-v1.css`, then `v2-only.css`, loaded last) beats all three layers: that is where v2 states what must stay v2's. A rule that
v1 has a heavier rule against has to be unlayered to stay: the `[hidden]` rule and the other `!important` ones (an `!important` declaration in a lower
layer would beat every other `!important` one), `#back`, and the colours of `.btn-primary` and `.btn-danger`.

## Renamed

| Area | v2 name | v1 name | Commit |
|---|---|---|---|
| Buttons | `.icon-btn` | `.btn-icon` | step 1 |
| Visual plans | `.visual-plan*` (same names) | v1's rules as they are; Mermaid, prototype tabs and Markdown in a block stay in `v2-only.css` | visual plans |
| Messages | `.message` | `.chat-msg` | messages |
| Messages | `.message-meta` / `.message-author-wrap` / `.message-author` | `.chat-msg-header` / `.chat-msg-from` / `.chat-msg-from-name` (`.agent-profile-link` when it opens the profile) | messages |
| Messages | `.message-number` / `.message-bubble` / `.message-body` | `.chat-msg-id` / `.chat-msg-bubble` / `.chat-msg-body` | messages |
| Messages | `.message-attention` / `.message-expired(-mark)` | `.chat-msg-attention-icon` / `.chat-msg-expired(-icon)` | messages |
| Messages | `.system-notice` / `.system-row` | `.chat-msg-system` / `.chat-msg-system-row` | messages |
| Markdown | a rendered body is `.chat-msg-body` | `.chat-msg-body.md-rendered` (not in the raw view; the notice body too) | markdown |
| Markdown | `.row-btn.md-copy` | `.md-copy-btn` | markdown |
| Markdown | table in `.md-table-wrap` | `.md-table-wrap > table.md-table` | markdown |
| Markdown | the raw fenced block as `<pre><code>` | v1's `span.raw-code.raw-code-block` (it wraps, where the pre scrolled sideways) | markdown |
| Composer | (the form and the rows above it, loose children of `.chat`) | `.send-bar` wraps the reply bar, the pending-file list, the form and `#chat-error`; the form is `.send-form` (`#composer` stays as its id) | composer |
| Composer | `#composer textarea` / `button[aria-label=Send]` | `.msg-input` / `.btn.btn-primary.btn-send` | composer |
| Composer | `.attachment-slot` / `.composer-emoji` | `.btn.btn-md-toggle.btn-icon-sm.attachment-picker-btn` (the emoji button is the same kind of button) | composer |
| Composer | `.reply-chip` / `.reply-chip-jump` / its `.btn-icon` | `.reply-pending` / `.reply-pending-link` / `.reply-pending-clear` | composer |
| Composer | `.pending-attachments` / `.pending-image` | `.attachment-pending-list` / `.attachment-pending-item` (`.attachment-pending-thumb`, `-meta`, `-name`, `-status`, `.attachment-remove-btn`) | composer |
| Composer | `.composer-note` | `.system-room-notice` | composer |
| Rooms | `.room-row` / `.room-name` / `.room-preview` / `.unread-count` | `.room-item` / `.room-item-name` / `.room-item-preview` / `.room-item-unread` (the kind glyph is `.room-item-icon` in `.room-item-icon-group`, the name, count, attention and unread sit in `.room-item-top` inside `.room-item-info`, the preview is inside the row) | sidebar |
| Rooms | `.room-pin`, `.room-hide`, the pin and hide buttons | not drawn (rows no longer render them) | sidebar |
| Composer | `#composer.drag-attachment` | `.send-bar.drag-attachment` | composer |
| Settings | `<dialog id=settings-dialog>` grid with `header`, `nav`, sections | `.settings-modal` > `.settings-pane` > `.settings-nav` + `.settings-body` (`.settings-body-header`, sections); still a `<dialog>`, reset in `v2-only.css` | settings |
| Settings | `button.row-btn.settings-selected` | `button.settings-nav-item.active` | settings |
| Settings | `h2#settings-title`, `#settings-close` | `.settings-section-title`, `.settings-close-btn` (kept an icon button) | settings |
| Settings | `section.settings-content` (`[hidden]`) | `section.settings-section` (`.active` when selected, `hidden` kept) | settings |
| Settings | `.settings-section` inside a page (stacked sub-block) | `.settings-subsection` (v2's own; v1's `.settings-section` is a tab body) | settings |
| Settings | `.settings-row` / `-group` / `-label` / `-desc` / `-control` | same names, now v1's rules (the scoped v2 copies are deleted) | settings |
| Top bar | `header.workspace-bar` | `header.topbar` | top bar |
| Top bar | `.workspace-brand` / `img.brand-logo` | `.topbar-left` / `img.logo-mark` (`.brand-logo` and `-light` / `-dark` stay as the light/dark swap hook) | top bar |
| Top bar | `<strong>aimebu</strong>` / `#build-version` | `h1.logo` / `span.build-version#build-version` | top bar |
| Top bar | `nav` | `nav.topbar-right` | top bar |
| Top bar | `span.connection-dot(.connected / .disconnected)` | `span.status-indicator(.connected / .disconnected) > span.status-dot` | top bar |
| Members panel | `.agent-card` | `.agent-card.agent-card-compact` (v1's card box; the class is released from the hold list) | members |
| Members panel | `.agent-title` / `.agent-name` / `.agent-harness-icon` | `.agent-id` / `.agent-id-text` / `.harness-icon` (the v2 names stay on the element as hooks for the code and the tests, and carry no rules) | members |
| Members panel | `.presence-dot.agent-dot` | `.agent-online-dot.active` / `.stale` / `.dead` (6px; `.dead` is v2's red; `data-state` stays as the hook) | members |
| Members panel | `.agent-state > .agent-state-pill` (plain coloured text, a pill only when open) | `.agent-state-pill.agent-state-badge.agent-state-<idle\|thinking\|error\|stopped>`; a working agent is `thinking`; paused and needs-attention are `.agent-state-paused` / `.agent-state-attention` in `v2-only.css` | members |
| Members panel | `.agent-quick` | `.agent-card-actions` (v1's hover overlay, vertically centred) holding v2's icon buttons | members |

## Kept from v2 on purpose (decided by the owner, 2026-11-05)

- The floating message toolbar (not v1's frameless in-bubble controls). It floats at the message's top-right corner, over its top edge, for every
  message: it overlays the message's top-right corner, which the sender line (every sender line sits at the left) leaves free, reserves no space, and
  is too short to reach the bubble's text. Focus within a message shows it as hover does. A touch tablet keeps the toolbar wholly above the bubble
  (it is 46px tall); a phone's is in the bubble's flow. Every message sits on the left, so each one's top right is free for its toolbar.
- The composer's formatting toolbar, rich editor, link field and notices (`.composer-toolbar`, `.composer-field`, `.composer-editor`, `.composer-notice`), and the thread composer, which keeps its own markup until its area moves.
- Every message full width on the left (v1 gives each bubble 75% of the column and right-aligns the viewer's own).
- Touch-sized targets on phones (v1 has none).
- A real background on a plain `.btn` (v1 leaves the browser's grey), a hover on it, and a visible focus ring (v1's `.btn` sets `outline: none`).
- Icon-only top bar with tooltips (v1 and v2 already agree).
- Top bar (the layout stays v2's, by decision): placed in the app grid and fixed at 43px on a desktop (a window over 700px wide that is not a touch screen: `--topbar-h`, the buttons keeping their 30px), and at 53px on a touch tablet and on a phone (49px under 480px, with `--bar-h` following), no bar padding above or
  below; 30px icon buttons (40px on touch and on a phone) with v2's counters (`.badge`, v1 has only a dot), the Rooms/Inbox/Health/Memory/Usages/
  Settings/⋯/Sign out set, the ⋯ menu and the tucking of controls that do not fit on a phone or a touch tablet, the A/B/C experiment pill, the
  light and dark logo swap, and `z-index: auto` (v1 raises the bar to 100, which would lift it over the first-run card). The generic `.topbar button`
  sizes stay in `theme-v1.css`, in place: the popovers (⋯ menu, health, usages, system events) are children of the bar and tie with their own rules.
- Settings keeps what v1 never had in `v2-only.css`: the `<dialog>` reset, the caption gap before the Agents group, the hidden pending Users tab,
  toggle and stacked rows, the actions row, and 40px tabs on touch. The quota, switcher and harness blocks keep v2's own
  rules until their areas move.
- Settings fields: text inputs, number fields, text areas and selects of the Settings pages carry `.settings-text-input` / `.settings-select`
  (a text area adds `.settings-textarea`, v1's input without its fixed height); v1's field is 28px tall, phones get 40px in `v2-only.css`. The
  Harnesses page (and its onboarding copy) still has its own field rules.
- Usages: the provider tile, window rows, pace ticks, credits row, empty and error lines and the switcher tile use v1's rules as written (v2's px copies
  of them are deleted); the popover's own layout (`#usages-popover`, `.usages-open`) and the sparklines stay v2's.
- Colours: v2's own areas (inbox, schedules, agents and thread panels, health, knowledge, the room lists) name v1's tokens directly. The `--legacy-*`
  tokens that stood between them and v1's palette (90 of them, remapped per theme) are gone; the computed styles of the style snapshot are unchanged.
- v2-only features keep their own rules: threads, the emoji picker, inbox, machines, harnesses, health, import, spawn, schedules, users and
  login, project settings, onboarding.

Left under v2's names for now (their areas move later): the toolbar (`.message-actions*`, owned by the perf branch), `.reaction-pill(s)`,
`.thread-chip`, `.message-receipts` (read receipts come back to v1's `.chat-msg-receipts` in a later step), `.message-images`, `.message-role`.

## Markdown: what v2 adds, and where it differs from v1 (accepted)

- v2 keeps what v1 has no rule for: h4–h6, `hr`, `del`, nested lists, task items, `.mention-group` and `role=button` mention states, the touch copy row,
  and the code and copy-button rules inside visual-plan blocks (v1 draws those blocks without them). All of it is in `v2-only.css`.
- Between two blocks v1 writes a paragraph break as `<br><br>`, which on top of the block's own margin gives a blank line and a bit more, so v1's
  messages look looser. v2 draws blocks, and puts exactly one line between two of them (`1.55em`). That is the one visible difference.
- v1's copy button is white on `--bg-tertiary`, which is low contrast in the light themes; it is v1 as it is.

## Reverted to v1 on purpose
- Top bar atoms are v1's: the logo (1.15rem, 700, was 16px), the version text (.6rem at .6 opacity, was 8.4px at .9; high contrast keeps full
  opacity), the brand gap (10px, was 7px) and the connection dot (8px with a glow, was 9px with a halo ring). The System events popover's title is a plain
  `strong` now: it was styled as the brand (16px, letter-spaced) and hidden on phones by the bar's `strong` rule, and is 14px and shown like the other
  popovers' titles, so every popover title in the bar shows on a phone now. v1 hides the version under 740px; v2 shows it again from 701px.
- Member cards: the dot before the name is 6px (was 8px) and takes v1's green and amber (paused, needs a decision) and a v2-only red when the agent is dead (stopped, lost, offline), as the owner asked. The badge is v1's pill (16px, 8.1px mono, tinted) in the collapsed row; the open card
  keeps its readable rounded 12px sans pill (the owner's choice), coloured by the same variant. The name takes v1's `.agent-id` type (11.2px, 600).
  The hover actions sit in v1's overlay, centred on the name row (a collapsed card can have a second line); an open card keeps them at its top. Where there is no hover
  (touch, a stylus) they sit on a line of their own, as v1 shows them at rest. In the light themes the amber badges (working, paused, needs a decision) have darker text, to stay above 4.5:1.

- Colour tokens and the nine palettes are v1's, including its muted text and light themes, which are under WCAG AA in places: the `test-ui`
  contrast audit now reports them instead of failing on them (`a11y-audit.cjs --strict` still enforces).
- Read receipts under every message go back to v1's (none) when the messages area moves over.

## Transitional

- Rules of classes v2's own markup uses under another meaning (v1 used the same name for another job) were left out of `web/v1.css` when it was
  generated; the area that migrates such a class adds the v1 rule it needs by hand.

- `web/v1.css` leaves out v1's universal reset and its `html` / `body` rules until the last area has moved over (adding them then also drops
  the layer): v2's unmigrated areas rely on the browser's default margins and on their own page layout.
- v2's one button system (`button:not(.row-btn)` in `theme-v1.css`, in the `v2-base` layer) still styles every button v1 has no class for. It goes
  area by area; the fixes v1 needs then go in `v2-only.css`. A plain `.btn` or `.btn-icon` keeps a fixed background (`:where()`, so any other v2
  rule wins over it); `.btn-primary` and `.btn-danger` keep v2's theme colours; the focus ring is v2's (v1 sets `outline: none`).
- Accepted until their areas move: a `.btn` that nothing else sizes now takes v1's 11.2px text and 30.8px height (it was 12px and 34px), and the
  thread composer's B / I / U / S buttons lose their border. Both go away when those areas take v1's classes.

- The state classes `active`, `dm` and `pinned` have v1 rules only under the moved areas (`.room-item.active`, `.room-item.dm .room-item-icon`,
  `.room-item.pinned .room-item-name`, and the settings nav and section rules); a bare `.active` elsewhere has none.
- A row's hover controls (the project menu and the + for a new room) are an overlay at the row's end, centred on it and fading in on hover or focus as v1's
  agent card actions do, so showing them changes neither the row's height nor its badges; on a touch screen they are part of the row.
- A row's badges (project chip, health, mute, member count, attention, unread) never shrink and never leave the row. The name is cut with an ellipsis,
  and when even a ten-character name and the badges do not fit side by side (a nested row at the narrowest column) the badges wrap under it.
- The rooms rows show what v1 shows and v2 had hidden or drawn differently: the attention pill (`🔔 N`) is visible, the member count is always drawn (`0`
  when the room has no count), and an unread count makes a row bold only while it is not the open one.
