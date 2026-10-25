# Frontend: moving `web/` to Preact

Status: plan approved by the owner on 2026-10-21 (AMB-70); step 1 (the runtime and the Settings → Harnesses prototype) is a
prototype under review. Each later step is its own change, with its own approval. Authors: the AMB-70 task room (three independent plans,
consolidated). Related: AMB-69 (contributor onboarding), which builds on the `web/` layout described here and in
[../../CONTRIBUTING.md](../../CONTRIBUTING.md).

## 1. Why

`web/js` is 88 hand-written ES modules, about 13,100 lines (`chat.js` 1,289, `import-v1.js` 863, `agents.js` 700,
`app.js` 548, `settings.js` 525, `harness-settings.js` 504). Each module builds its DOM by hand and redraws by clearing a
container and building it again (`this.list.replaceChildren(...)`), carrying by hand whatever the redraw would lose: which
cards are open, what was typed, which request is the latest (`harness-settings.js` keeps `openLaunchers`, `openModels`,
`modelFailures` and a `sequence` for exactly that). A component framework makes that state explicit and the redraws
incremental, and gives contributors one shape to learn instead of one per module.

## 2. Decision

**Preact + htm, vendored as native ES modules, loaded through an import map, with no build step.**

| Option | For | Against |
| --- | --- | --- |
| **A. Preact + htm, no build (chosen)** | No `node_modules`, no `package.json`, three vendored files; the served code is the source; the release needs no Node; about 7 KB gzip | No JSX or TypeScript diagnostics; templates are strings to the editor; no hot reload; our own code ships unminified |
| B. Preact + JSX/TypeScript compiled by esbuild run as a Go `tool` directive (like air) | Types and JSX; still no Node or `node_modules`; still one binary | One more tool dependency to approve; a generate step before `go build`; generated files to commit or build in CI |
| C. React + Vite (rejected by the owner) | The industry default, the biggest ecosystem, hot reload | Node, npm and `node_modules` in development and CI; a build before `go build`; about ten times the runtime size; many dependency approvals |

Why A: the repository's rules (each dependency needs approval, `AGENTS.md`; one Go binary through `go:embed`; no Node in the
release), the ticket's own framing (Preact, build or no build), the size of the UI (one product, about 13k lines, no SSR or
SEO), and `docs/architecture.md`, which already named vendored Preact + htm as the way out. The owner rejected C: the app is
small, React's runtime and ecosystem are not needed, and Node versions, npm package churn and `node_modules` would cost more
than they give (hot reload being the only real gain).

B stays open: if TypeScript or JSX is wanted, decide it with the incoming frontend owner. Moving from A to B is mechanical
(htm templates become JSX; the components stay), so A does not lock it out.

### Runtime

| File | Package | Version | Licence | gzip -9 |
| --- | --- | --- | --- | --- |
| `web/vendor/preact/preact.mjs` | preact | 11.0.1 | MIT | 4,960 B |
| `web/vendor/preact/hooks.mjs` | preact | 11.0.1 | MIT | 1,582 B |
| `web/vendor/preact/htm.mjs` | htm | 3.1.1 | Apache-2.0 | 646 B |
| `web/vendor/preact/signals.mjs` | @preact/signals | 2.11.3 | MIT | 1,771 B |
| `web/vendor/preact/signals-core.mjs` | @preact/signals-core | 1.14.4 | MIT | 1,935 B |

Sources, integrity hashes, SHA-256 and the update steps are in [web/vendor/preact/README.md](../../web/vendor/preact/README.md).
Preact 11 is a new major line (11.0.0 on 2026-09-30): it is pinned and tested in the page, not assumed to behave like 10.x.
htm has had no release since 2022: small and stable, but quiet. Signals (owner approval 2026-10-22) imports Preact, its
hooks and signals-core through the same import map, so it extends the one copy of Preact. Not used: the htm "standalone"
bundle (it carries its own older Preact), a router, CSS libraries, jsdom or component-test packages.

## 3. How it ships

- `web/index.html` has an import map, before the first module script, that names `preact`, `preact/hooks` and `htm`. The
  hooks import `"preact"` themselves, so the map gives them and `ui.js` the same copy.
- `web/js/ui.js` is the only module that imports the runtime; components import `html`, `render` and the hooks from it.
  `scripts/test-preact-vendor.cjs` fails if any other module imports Preact or htm.
- `embed.go` (`//go:embed web`) is unchanged: the vendored files are part of `web/`, so the binary is still the whole app.
- The server versions the assets by content (`internal/server/static_assets.go`): every relative module import, `./x.js`
  and now `../x.js` from a page's folder, gets `?v=<hash>`, and so do the paths in the import map. One module therefore has
  exactly one URL. That matters: the same file under two URLs runs twice, which would give a page its own copy of `api.js`
  (no session) or of Preact's hooks state. `TestPrepareWebAssetsVersionsParentImportsAndImportMap` covers it.
- `make dev` (air) also rebuilds on `.mjs` changes.

## 4. UI architecture

```
web/js/ui.js                   Preact + htm + hooks: the only import point
web/js/components/             shared pieces, the same markup as the hand-written helpers:
                               IconButton (icon-button.js), SettingsRow and SettingsGroup (settings-row.js)
web/js/hooks/                  shared hooks: useAlive (is the component still mounted after an await), useDirty (report
                               unsaved input to the page)
web/js/pages/<page>/           one page: its components, model.js (pure logic) and switch.js (legacy or Preact)
web/js/*-source.js, *-format.js, other plain modules
                               data and pure logic, import-free, tested in Node's vm
```

Rules:

- One rendering owner per DOM subtree. A container is Preact's (it is only ever changed through `render`) or hand-written
  code's, never both. The settings shell keeps a page's tab and its section's visibility; Preact owns the section's children.
- Props down, callbacks up. A page component owns its requests and state; leaf components show what they are given.
- Pure logic stays out of components, in an import-free module (`pages/<page>/model.js`), so `make test-web` tests it.
- Every async read carries a generation (machine, opening, mount), and its answer is dropped when the generation moved on.
  Nothing is written from a render or a mount effect; nothing is retried automatically on a remount.
- What a person types is a draft kept apart from the server's data. A refresh caused by something else leaves it alone;
  only the action that saved it hands the form back to the server's values.
- Lists are keyed by a stable id (a harness id, a model slug, a message id), never by position or status.
- The Preact page keeps the legacy page's ids, classes, roles and text, so the same CSS styles both and `test-ui` runs the
  same checks against both.

### State

A page's shared state is signals the page owns: created once per page (for Harnesses in `pages/harnesses/use-harnesses.js`)
and read with `.value` by the components that render it, so a change re-renders only what reads it. Shared means what more
than one component reads or what outlives a component: for Harnesses the machines, the selected machine, the rows and
activity, the chosen harness, which cards have Edit open, the drafts, the roles with their default agents, the harnesses of
other machines that a role default names, and the model detection failures. Writes go through the
page's actions, never from render. A flag only one component uses (a button's busy state, a form's error, an input the
component submits itself) stays `useState`. `.peek()` reads a signal without subscribing, in actions and guards.

## 5. Old and new side by side

The legacy UI keeps working until every page has moved. Each migrated page is a UI experiment (`web/js/ui-variant.js`):

- `defineExperiment('<key>', '<name>', ['Legacy', 'Preact'], { hidden: true })` in the page's `switch.js`, and the key in
  `ACTIVE`.
- **Option A is the legacy page and the default.** A hidden experiment runs only for a browser that opted in: its pill
  shows once `?ui=<key>:<a|b>` is in the address, or when the browser has a saved choice from such a visit. Until then the
  pill stays hidden and `activeVariant()` answers `'a'`. `?ui=<key>:b` opens the Preact page and shows the pill, which then
  switches between the two.
- The Preact page's heading carries a small "Preact" badge, so a reviewer can see which option is showing. It is the one
  intended difference in markup between A and B, and goes when the page becomes the default.
- The page's `switch.js` builds exactly one implementation. When the pill changes, it builds the other one, puts its tab and
  section where the first one's were (with the shell's click handler, visibility and selection), unmounts the first one and,
  if the section is showing, opens the new one. The two are never mounted together. Unfinished input goes with the old page
  (an Edit form or model list not saved, the Add harness form, a model id not added, ticked auto-detect results), so the
  switch asks first (the in-app confirm) when there is any; the Preact page knows exactly through `useDirty`, and for the
  legacy page anything typed since it was last opened counts. Cancel sets the pill back (`chooseVariant` in `ui-variant.js`).
- A page's legacy module is removed only in a separate, approved change, after its Preact page has been the default for a
  while with no regressions. Its experiment is then taken out of `ACTIVE`.

- A swap carries the view across: the page being dropped gives its selected machine to the new one.

**Option A is not a full rollback.** Choosing A changes which page renders, nothing else: the Preact runtime, the import
map, `ui.js`, the asset versioning and the page's `switch.js` load for everyone, because `settings.js` imports the switch,
which imports both pages. If the runtime failed to load (a broken vendored file or import map), the module graph would fail
and the whole app would not start, legacy pages included. Follow-up, before step 2: load B with a dynamic `import()` only
when B is chosen, fall back to the legacy page (with a notice) when that load fails, and extend the server's asset
versioning to `import()` specifiers, which today it versions only in `from` imports.

Hidden is the working assumption until the owner decides who sees the pill. Showing it to every signed-in user instead is
dropping `{ hidden: true }` from the page's `defineExperiment` call; the `hidden` option in `ui-variant.js` is the only code
that exists for it.

### Page lifecycle contract

Every migrated page follows the same lifecycle, so the settings shell (and later the app shell) can treat legacy and Preact
pages alike:

- **Mount** (the constructor): builds the `tab` and the `section`, appends them, and renders an empty page. No request.
- **Open** (`open()`, each time the shell shows the section): reloads from the top (machines, then the selected machine's
  list), closes any open panel and opens on `wanted` when one is set. Card drafts are dropped, as on the legacy page.
- **Hide** (the shell sets `section.hidden`): nothing is unmounted or cancelled. Drafts stay, requests in flight finish and
  their answers are drawn into the hidden page; nothing reloads until the next open.
- **Destroy** (`destroy()`, only when the A/B switch drops the page): unmounts the component tree. Answers that arrive later
  are dropped (`useAlive`, generations); requests already sent are not aborted, so a write in flight still reaches the server.
- **Drafts** belong to the component that edits them: a card owns its Edit form, a models panel its model list and new-model
  entry, the page its Add harness and auto-detect panels. They report unsaved input through `useDirty`; the adapter only
  aggregates the keys for `hasUnsavedChanges()`.
- **Writes in flight on close**: closing Settings is a hide, so a write and its re-read complete and the card's write lock is
  held until they do. A swap asks first when there is unsaved input; it does not wait for writes in flight.

## 6. Tests

- The existing `make test-web` scripts stay as they are. When a migrated module's test slices its source, the logic moves to
  an import-free module and the test loads that, without weakening an assertion.
- New in step 1: `scripts/test-harness-model.cjs` (the Harnesses page's rules in a vm) and `scripts/test-preact-vendor.cjs`
  (vendored files against `SHA256SUMS`, the import map, the single import point).
- `make test-ui` runs a migrated page's checks in both options: the Harnesses checks run on A (no pill), the page is opened
  again with `?ui=preact-harnesses:b`, the same checks run on B, and the pill switches back to A in place. It also checks
  that exactly one implementation is built and that the tab keeps its place in the nav.
- `scripts/harness-settings.cjs` (run by `make test-ui`, and on its own with `HARNESS_VARIANT=a|b` to run one page) drives a scratch
  server and checks the list beside the detail, role defaults per model and the role panel, the sync with Settings → Roles,
  the stale-save refusal and the phone width, first on the legacy page and then on the Preact page (`?ui=preact-harnesses:b`).
- `make test-ui` also runs `scripts/test-harness-page.cjs`: the Preact page in real Chromium (the existing Puppeteer tooling,
  `web/` served as is with the page's import map, no Go server), with every request held until the test answers it, so the
  order of answers is chosen. It checks:
  - a late Enabled save on machine A, answered after B was chosen, neither reads A again nor clears B's list, and B's own
    pending read still draws;
  - adding detected harnesses on A failing after B was chosen does not read A again or clear B;
  - an auto-detect answer for A arriving after A → B → A opens no panel;
  - a save whose re-read fails keeps the saved command in the form, and the next save sends it; the same for a model list;
  - one write at a time per card: Save cannot start while Enabled is in flight (its re-read included), nor Enabled while
    Save is, and every write control is disabled meanwhile;
  - model detection runs once at a time: the button, Retry and repeated Enter during a detection start nothing more, so
    answers cannot arrive out of order, and a retry's answer replaces the earlier failure;
  - an A/B swap keeps the selected machine, B to A and A to B, on a machine other than the first;
  - text typed while a save is under way stays, and still counts as unsaved;
  - the same input node, the focus and the selection survive another card's refresh, and the node survives its own card's
    save;
  - after unmount, a pending answer draws nothing, throws nothing and starts no request;
  - the A/B switch asks before throwing unfinished input away (an Edit form, the Add harness form, a model id typed but not
    added, ticked auto-detect results for harnesses or models); for each, Cancel keeps the page and the input, and Switch
    builds the legacy page; a cancelled Add harness form no longer counts as unsaved;
  - removing a harness: the in-app confirm, the DELETE, and the list read again without it;
  - the list beside the detail: the first harness chosen, one card shown, click and arrow keys move the choice, a reload keeps it;
  - a models save sends the revision its draft started from, and one refused as stale drops the draft, reads again and says so;
    a save moves the draft on to the revision it answered with, so an edit made during the save, and the next edit after a
    failed re-read, are not refused as stale;
  - role defaults: a switch per role on every model saves the role on its own, the role panel names its entries (those of
    another machine are read once), a refused save says why and the views show what is stored, and a role save made in
    Settings → Roles is read here while the page is shown and ignored while it is hidden.
  - the shell's `onReady` hears of a harness that can start an agent, through the switch and across a swap (the first-run
    harness offer relies on it).
- `make test-ui` is not part of `make test-full`, and CI runs only `make test-full`; whether `make test-ui` runs in CI is
  to be agreed.

## 7. Risks

| Risk | What could go wrong | How it is handled |
| --- | --- | --- |
| Live lists | A redraw replaces nodes, losing focus, selection or a half-typed value | Keys by stable id; `test-harness-page.cjs` asserts the same input node, focus and selection survive refreshes and saves |
| Scroll anchoring | Keys keep nodes, not the viewport; Preact commits on a later tick, so code that measures right after a change reads the old layout | Chat moves last; `scroll-anchor.js` and the message window stay; the list keeps its direct-child shape; measuring happens in `useLayoutEffect` |
| Drafts | A refresh or a switch of room overwrites what was typed | Drafts are state apart from server data; room and thread semantics stay as they are; sign-out clears session state (`chat.js:187-198`) |
| ProseMirror | Two owners of the composer's DOM | The composer stays an imperative island inside a component that renders an empty container and mounts the editor through a ref; lazy load, the rich/plain switch, IME, undo, paste and Markdown output are untouched |
| CSS and themes | Wrapper elements break direct-child selectors; a page looks different | Same classes and variables; no CSS-in-JS; themes stay `data-theme` on `<html>`; no extra wrappers (a page renders a fragment into its section) |
| Async | A late answer writes into a page that moved on or went away | Generations on every read; a read for a machine that is no longer selected does nothing; page actions carry the view (machine and opening) they started in; `useAlive` after every await, in a layout effect because Preact may run a plain effect's cleanup only after the next paint; a switch unmounts the old page |
| Saves | A save succeeds but the re-read fails, and the form falls back to the old values; text typed during the save is lost | The re-read reports whether it drew; when it did not, the saved values become the card's data; a draft is cleared only when nothing was typed since the save started |
| Stale closures | Two events before the next render overwrite each other (found while building step 1) | State updates that depend on the previous state are functions of it |
| Two models for a while | Contributors maintain both styles | Each step is small and finishes its page; the stop rule below |
| Runtime age | Preact 11 is new; htm is quiet | Pinned by SHA-256; both are small enough to read; a later move to 10.x or a fork is a file copy |

Stop rule: after step 4 (agents panel), compare the migrated pages with the legacy ones (size, bugs, test churn). If the
Preact pages are not clearly simpler, stop there and keep the islands that exist.

## 8. Migration plan

Each step is its own ticket and change, with the gates in section 6, and adds an experiment with A = legacy as the default.
Turning an experiment's default to B, and later removing the legacy module and the experiment, are separate approved changes.

### Step 1: runtime and Settings → Harnesses (AMB-70, prototype under review)

- Scope: vendor Preact and htm; the import map; `ui.js`; the first shared components (`IconButton`, `SettingsRow`,
  `SettingsGroup`) and `useAlive`; asset versioning for `../` imports and the import map; Settings → Harnesses rebuilt in
  Preact with the legacy page's full feature set (add, enable, edit, remove, models by hand and from auto-detect, default
  model, auto-fill of a new harness's models, folder retry). It has since been brought level with the legacy page: the list of
  harnesses beside the chosen one's detail (the only layout; the models are always shown), role defaults per model with the
  role panel and the sync with Settings → Roles (the same `role-defaults.js` store and `role-defaults-logic.js` rules as the
  legacy page), the models save that names the revision it started from and is refused when the list changed meanwhile, and
  Mistral Vibe.
- Files: `web/vendor/preact/`, `web/index.html`, `web/js/ui.js`, `web/js/components/`, `web/js/hooks/` (`use-alive.js`, `use-dirty.js`),
  `web/js/pages/harnesses/` (`page.js` the shell adapter, `harnesses-page.js`, `use-harnesses.js` the page's data and
  late-answer guards, `card.js`, `models.js`, `add-harness-form.js`, `detect-panel.js`, `detection-details.js`, `activity.js`, `model.js`, `switch.js`, and for layout C and role defaults `master-list.js`, `role-panel.js`, `role-switch.js`), `web/js/settings.js` (uses the
  switch), `web/js/ui-variant.js` (`ACTIVE`), `web/theme-v1.css` (a two-option pill was 33 px wide between 521 and 700 px,
  under the 40 px tap target; it is 40 px now), `internal/server/static_assets.go`, `.air.toml`, tests and docs.
- Experiment: `preact-harnesses`. The legacy `web/js/harness-settings.js` is the reference for behaviour; B follows it.
- Acceptance: `make test-full` and `make test-ui` pass; the Harnesses checks pass on A and on B.

### Step 2: the other Settings sections

- Scope: the small sections first (prompts, macros, roles, retention, limit guard, quota), then users, machines and project;
  then the settings shell's nav and section switching.
- Files: `web/js/*-settings.js`, `users.js`, `machines.js`, `project-settings.js`, `settings.js`; new
  `web/js/pages/settings/<section>/`; shared components as they are needed (`Dialog` around `dialogs.js`, `Notice`).
- Before this step: replace the pill-per-page experiments with **one registry** of migrated pages (each page's A/B choice
  in one place, one pill or one Settings panel), so a dozen migrated sections do not mean a dozen pills; and the follow-up
  from section 5 (B loaded on demand, legacy fallback when it fails to load).
- **Checkpoint**: migrate one small section first (Prompts), then stop and review Harnesses plus that section with the owner
  (code size against legacy, bugs found, test churn, the build question) before the rest of the step.
- Experiments: one registry entry per section (for example `preact-settings-prompts`).
- Acceptance: the section's existing `test-ui` checks run on A and B; the shell's interface to each section (`tab`,
  `section`, `open()`) is unchanged until the shell itself moves.

### Step 3: Health, Knowledge, Inbox

- Scope: the Health panel, the Knowledge (memory) viewer, the Inbox and its notifications.
- Files: `health.js`, `knowledge.js`, `inbox.js`, `inbox-source.js` (the source stays a plain store; the page reads it
  through a hook), `inbox-notifications.js`.
- Experiments: `preact-health`, `preact-knowledge`, `preact-inbox`.
- Acceptance: Inbox items keep their identity and focus across live updates; answering, stale-item and read-only checks in
  `test-ui` pass on A and B.

### Step 4: agents and members, then the spawn and switch dialogs

- Scope: the agent cards and member list, the member profile, then the spawn and switch dialogs.
- Files: `agents.js`, `room-members.js`, `agent-source.js`, `sender-profile.js`.
- Experiments: `preact-agents`, then `preact-spawn`.
- Acceptance: identity and stale-room guarantees kept (`agents.js:111-143`); a card updates in place under the event stream;
  the stop rule is applied after this step.

### Step 5: sidebar and navigation

- Scope: the room list (unread counts, ordering, collapse, selection), mobile tabs.
- Files: `sidebar.js`, `sidebar-order.js`, `room-unread.js` (pure, stays), `mobile-tabs.js`, the wiring in `app.js`.
- Experiment: `preact-sidebar`.
- Acceptance: unread and ordering tests unchanged; selection and collapse survive live updates.

### Step 6: chat (last)

- Scope: the message list and thread panel. `scroll-anchor.js` and the message window stay; each message becomes a keyed
  component; the composer (ProseMirror) stays imperative inside a ref-mounted host.
- Files: `chat.js`, `message-actions.js`, `answer-controls.js`, `visual-plan.js`, `composer*.js` (host only).
- Experiment: `preact-chat`.
- Acceptance: the existing message identity, draft and scroll-under-live-update checks pass on A and B; no scroll jump with
  late images, Mermaid or attachments.

### Step 7: the shell

- Scope: `app.js` boot, sign-in, setup and onboarding, the top bar.
- Files: `app.js`, `setup.js`, `welcome.js`, `onboarding.js`, `topbar-menu.js`.
- Acceptance: boot and sign-in checks pass; after this step the legacy modules can be retired one by one.

## 9. Open questions

- Build: stay on A, or move to B (esbuild as a Go `tool` directive for JSX and TypeScript)? Decide with the incoming frontend
  owner.
- Pill visibility: should a running migration experiment show its pill to every user, or only when `?ui=` names it (the
  current default, `{ hidden: true }`)?
- Should the Chromium checks join a CI gate (today CI runs `make test-full` only)?
- When does a page's default switch to B, and who signs that off?
