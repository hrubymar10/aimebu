# v2 markup → v1 class names

v2 is moving onto v1's stylesheet: `web/v1.css` is v1's `style.css` at v1 commit 466852f (without fleets and the leaderboard), written by
`scripts/sync-v1-css.cjs` (`--check` says whether the file still matches v1). An area moves over when its markup carries v1's class names and its v2
rules are deleted. Things v1 never had go in `web/v2-only.css`, written with v1's tokens. This page is the map of the renames and the list of
differences from v1 that are kept on purpose, so a review does not flag them.

**Cascade order.** `v1.css` (loaded first) declares `@layer v2-base, v1-reset, v1;`. `v2-base` holds the legacy `style.css` and the generic element and
button look in `theme-v1.css`; `v1-reset` holds v1's universal reset, applied only inside the areas that have moved over (the send bar so far); `v1`
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
| Composer | (the form and the rows above it, loose children of `.chat`) | `.send-bar` wraps the reply bar, the pending-file list, the form and `#chat-error`; the form is `.send-form` (`#composer` stays as its id) | composer |
| Composer | `#composer textarea` / `button[aria-label=Send]` | `.msg-input` / `.btn.btn-primary.btn-send` | composer |
| Composer | `.attachment-slot` / `.composer-emoji` | `.btn.btn-md-toggle.btn-icon-sm.attachment-picker-btn` (the emoji button is the same kind of button) | composer |
| Composer | `.reply-chip` / `.reply-chip-jump` / its `.btn-icon` | `.reply-pending` / `.reply-pending-link` / `.reply-pending-clear` | composer |
| Composer | `.pending-attachments` / `.pending-image` | `.attachment-pending-list` / `.attachment-pending-item` (`.attachment-pending-thumb`, `-meta`, `-name`, `-status`, `.attachment-remove-btn`) | composer |
| Composer | `.composer-note` | `.system-room-notice` | composer |
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

## Kept from v2 on purpose (decided by the owner, 2026-11-05)

- The floating message toolbar (not v1's frameless in-bubble controls).
- The composer's formatting toolbar, rich editor, link field and notices (`.composer-toolbar`, `.composer-field`, `.composer-editor`, `.composer-notice`), and the thread composer, which keeps its own markup until its area moves.
- Own messages at most 75% wide, other people's full width.
- Touch-sized targets on phones (v1 has none).
- A real background on a plain `.btn` (v1 leaves the browser's grey), a hover on it, and a visible focus ring (v1's `.btn` sets `outline: none`).
- Icon-only top bar with tooltips (v1 and v2 already agree).
- Top bar (the layout stays v2's, by decision): placed in the app grid and fixed at 53px (49px under 480px, with `--bar-h` following), no bar padding above or
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

## Reverted to v1 on purpose
- Top bar atoms are v1's: the logo (1.15rem, 700, was 16px), the version text (.6rem at .6 opacity, was 8.4px at .9; high contrast keeps full
  opacity), the brand gap (10px, was 7px) and the connection dot (8px with a glow, was 9px with a halo ring). The System events popover's title is a plain
  `strong` now: it was styled as the brand (16px, letter-spaced) and hidden on phones by the bar's `strong` rule, and is 14px and shown like the other
  popovers' titles, so every popover title in the bar shows on a phone now. v1 hides the version under 740px; v2 shows it again from 701px.

- Colour tokens and the nine palettes are v1's, including its muted text and light themes, which are under WCAG AA in places: the `test-ui`
  contrast audit now reports them instead of failing on them (`a11y-audit.cjs --strict` still enforces).
- Read receipts under every message go back to v1's (none) when the messages area moves over.

## Transitional

- `scripts/v1-css-hold.json` lists the classes v2's own markup uses whose v1 rules are held back (v1 uses the same name for another job, and its
  rule would change an unmigrated area wherever v2 sets nothing). A selector naming one is left out of `web/v1.css`; the commit that migrates an area
  removes its classes from the list, which is how v1's rules for it come in.

- `web/v1.css` leaves out v1's universal reset and its `html` / `body` rules until the last area has moved over (`--final` writes them, and drops
  the layer): v2's unmigrated areas rely on the browser's default margins and on their own page layout.
- v2's one button system (`button:not(.row-btn)` in `theme-v1.css`, in the `v2-base` layer) still styles every button v1 has no class for. It goes
  area by area; the fixes v1 needs then go in `v2-only.css`. A plain `.btn` or `.btn-icon` keeps a fixed background (`:where()`, so any other v2
  rule wins over it); `.btn-primary` and `.btn-danger` keep v2's theme colours; the focus ring is v2's (v1 sets `outline: none`).
- Accepted until their areas move: a `.btn` that nothing else sizes now takes v1's 11.2px text and 30.8px height (it was 12px and 34px), and the
  thread composer's B / I / U / S buttons lose their border. Both go away when those areas take v1's classes.
