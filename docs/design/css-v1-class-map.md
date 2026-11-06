# v2 markup → v1 class names

v2 is moving onto v1's stylesheet: `web/v1.css` is v1's `style.css` at v1 commit 466852f (without fleets and the leaderboard), written by
`scripts/sync-v1-css.cjs` (`--check` says whether the file still matches v1). It loads first, in a cascade layer, so an unlayered v2 rule still wins over
it; an area moves over when its markup carries v1's class names and its v2 rules are deleted. Things v1 never had go in `web/v2-only.css`, written with
v1's tokens. This page is the map of the renames and the list of differences from v1 that are kept on purpose, so a review does not flag them.

## Renamed

| Area | v2 name | v1 name | Commit |
|---|---|---|---|
| Buttons | `.icon-btn` | `.btn-icon` | step 1 |
| Visual plans | `.visual-plan*` (same names) | v1's rules as they are; Mermaid, prototype tabs and Markdown in a block stay in `v2-only.css` | visual plans |

## Kept from v2 on purpose (decided by the owner, 2026-11-05)

- The floating message toolbar (not v1's frameless in-bubble controls).
- Own messages at most 75% wide, other people's full width.
- Touch-sized targets on phones (v1 has none).
- A real background on a plain `.btn` (v1 leaves the browser's grey), a hover on it, and a visible focus ring (v1's `.btn` sets `outline: none`).
- Icon-only top bar with tooltips (v1 and v2 already agree).
- v2-only features keep their own rules: threads, the emoji picker, inbox, machines, harnesses, health, import, spawn, schedules, users and
  login, project settings, onboarding.

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
- v2's one button system (`button:not(.row-btn)` and its variants in `theme-v1.css`) still styles every button. It goes area by area; the fixes v1
  needs then go in `v2-only.css`.
