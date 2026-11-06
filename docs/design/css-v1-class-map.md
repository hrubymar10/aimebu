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

## Kept from v2 on purpose (decided by the owner, 2026-11-05)

- The floating message toolbar (not v1's frameless in-bubble controls).
- The composer's formatting toolbar, rich editor, link field and notices (`.composer-toolbar`, `.composer-field`, `.composer-editor`, `.composer-notice`), and the thread composer, which keeps its own markup until its area moves.
- Own messages at most 75% wide, other people's full width.
- Touch-sized targets on phones (v1 has none).
- A real background on a plain `.btn` (v1 leaves the browser's grey), a hover on it, and a visible focus ring (v1's `.btn` sets `outline: none`).
- Icon-only top bar with tooltips (v1 and v2 already agree).
- v2-only features keep their own rules: threads, the emoji picker, inbox, machines, harnesses, health, import, spawn, schedules, users and
  login, project settings, onboarding.

Left under v2's names for now (their areas move later): the toolbar (`.message-actions*`, owned by the perf branch), `.reaction-pill(s)`,
`.thread-chip`, `.message-receipts` (read receipts come back to v1's `.chat-msg-receipts` in a later step), `.message-images`, `.message-role`.

## Reverted to v1 on purpose

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
