# Frontend: moving `web/` to Preact

Status: plan approved by the owner on 2026-10-21. Since 2026-10-27 pages replace their legacy module directly (section 5):
Settings → Harnesses, including its onboarding step, is the first one moved. Each later step is its own change, with its own approval. Authors: three independent plans, consolidated. Contributor onboarding builds on the `web/` layout described here and in
[../../CONTRIBUTING.md](../../CONTRIBUTING.md).

## 1. Why

`web/js` is 88 hand-written ES modules, about 13,100 lines (`chat.js` 1,289, `import-v1.js` 863, `agents.js` 700,
`app.js` 548, `settings.js` 525, `harness-settings.js` 504). Each module builds its DOM by hand and redraws by clearing a
container and building it again (`this.list.replaceChildren(...)`), carrying by hand whatever the redraw would lose: which
cards are open, what was typed, which request is the latest (`harness-settings.js` keeps `openLaunchers`, `openModels`,
`modelFailures` and a `sequence` for exactly that). A component framework makes that state explicit and the redraws
incremental, and gives contributors one shape to learn instead of one per module.

## 2. Decision

**Preact + htm, vendored as native ES modules, loaded through an import map, with no build step of your own** (the server bundles the modules for the page when it starts, with esbuild as a Go library; see CONTRIBUTING.md, "The `web/` layout").

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
web/js/pages/<page>/           one page: its components, model.js (pure logic) and page.js (the shell's adapter)
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

## 5. Replacing a page

A page is replaced in one change: the Preact page is built, its legacy module is deleted and its `test-ui` checks move to the
new page, with none weakened. There is no experiment, chooser or fallback for it; the pill in `web/js/ui-variant.js` stays for
questions that really are A/B, and `ACTIVE` is empty while none runs.

- The settings shell keeps its hand-written part: the nav, section switching and which tab is shown. A page's `page.js`
  adapter has the interface the shell and the onboarding already used (`tab`, `section`, `open()`, `wanted`, `onReady`,
  `hasUnsavedChanges()`) and renders the page inside `section` through `mountPreactPage` (`pages/mount-page.js`: the error
  boundary, the open token, the callbacks of an unmounted tree ignored).
- The same page can be embedded elsewhere: the onboarding's Harnesses step is `new HarnessSettings(null, { embedded: true, host })`.
  Its element ids carry the `onboarding-` prefix (`IdPrefix` context), so they never repeat Settings' own, and the detect panel
  has no Add or Close button there: the step's Continue adds what is ticked (`tickedCount()`, `addTicked()`).
- The Preact runtime, the import map and `ui.js` load for everyone; if one failed to load the whole app would not start.

### Page lifecycle contract

Every migrated page follows the same lifecycle, so the settings shell (and later the app shell) can treat them alike:

- **Mount** (the constructor): builds the `tab` and the `section`, appends them, and renders an empty page. No request.
- **Open** (`open()`, each time the shell shows the section): reloads from the top (machines, then the selected machine's
  list), closes any open panel and opens on `wanted` when one is set. Card drafts are dropped.
- **Hide** (the shell sets `section.hidden`): nothing is unmounted or cancelled. Drafts stay, requests in flight finish and
  their answers are drawn into the hidden page; nothing reloads until the next open.
- **Clear** (`clear()`, the embedded copy when its step is left) and **destroy** (`destroy()`): unmount the component tree. Answers that arrive later
  are dropped (`useAlive`, generations); requests already sent are not aborted, so a write in flight still reaches the server.
- **Drafts** belong to the component that edits them: a card owns its Edit form, a models panel its model list and new-model
  entry, the page its Add harness and auto-detect panels. They report unsaved input through `useDirty`; the adapter only
  aggregates the keys for `hasUnsavedChanges()`.
- **Settings closing** (`SettingsDialog.release()`): the dialog's `close` event clears the page of every section that reports no unsaved
  changes, and a section is drawn again (`show()`, or its own `open()`) when it is next selected, so a closed Settings keeps no section
  in the page (about a thousand nodes). A page with unsaved drafts keeps its tree: the pages that report them through `useDirty` (Roles, Prompts, Macros, Harnesses, and General's message-expiry window) and Usages, whose uncontrolled fields are marked by any text typed in the section until a read of the server redraws them. A write in flight when the page is cleared still
  reaches the server, but its answer is dropped and the next open reloads from the top.

## 6. Tests

- The existing `make test-web` scripts stay as they are. When a migrated module's test slices its source, the logic moves to
  an import-free module and the test loads that, without weakening an assertion.
- New in step 1: `scripts/test-harness-model.cjs` (the Harnesses page's rules in a vm) and `scripts/test-preact-vendor.cjs`
  (vendored files against `SHA256SUMS`, the import map, the single import point).
- `make test-ui` runs the Settings → Harnesses checks over a mock of the machine's harnesses, and checks that exactly one
  Harnesses page is built, that the tab keeps its place in the nav and that the onboarding step builds its own beside it.
- `scripts/harness-settings.cjs` (run by `make test-ui`, and on its own) drives a scratch server and checks the list beside
  the detail, role defaults per model and the role panel, the sync with Settings → Roles, the stale-save refusal, the
  onboarding step's role defaults and the phone width.
- `make test-ui` also runs `scripts/test-prompts-page.cjs` (Settings → Prompts in Chromium with every request held: a scope
  answered late does not draw over the selected one, rows of a scope still loading cannot write, one write at a time, a
  revision conflict reloads and keeps every draft, a write answered after Clear, reopening or unmount reports and reads
  nothing, inline plans go back to the saved value when saving fails) and `scripts/test-prompts-model.cjs` (its pure rules, in
  `make test-web`).
- `make test-ui` also runs `scripts/test-macros-page.cjs` (Settings → Macros with every request held: every write sends the whole
  map, so a map that failed to load locks all writes; one write at a time; a write answered after a reopen or unmount reports
  nothing and reads the map again; an import preview and apply; copy-all answers out of order; a non-admin sees the list
  only; form and import drafts count as unsaved) and `scripts/test-macros-model.cjs` (the page's pure rules, in `make test-web`;
  the key and size rules stay in `macro-import.js`, tested by `test-macro-import.cjs`).
- `make test-ui` also runs `scripts/test-roles-page.cjs` (Settings → Roles with every request held: cards, chips and the hidden
  select; New role and Edit open the editor on the right field; reads answered out of order; one write at a time; a refused
  save keeps the editor and its text; closing with edits asks first and Escape closes only the editor; delete and reset ask
  and send the stored revision; the Harnesses page's role-defaults event is read while no editor is open and ignored while one
  is; a write answered after destroy draws, reads and announces nothing; the default-agents picker's add, order, pick order,
  remove and unavailable choice) and `scripts/test-roles-model.cjs` (the pure rules, in `make test-web`).
- `make test-ui` also runs `scripts/test-general-page.cjs` (Settings → General with requests held: the sections in their order; a
  person's saved theme and room-preview choice shown and applied, a retired theme falling back to dark, choosing writing the
  page, the browser's storage and the account's preferences; the chat switches driving the bell and the debug button and read
  again when General is shown; the expiry window read each time and left alone while typed in, a refused value sending nothing,
  one save at a time, a failed save keeping the text, a read-only view, a save answered after destroy; the backup shown to the
  owner and admins as they may use it, an export, two files chosen in a row drawing only the second's plan, Apply sending the
  ticked sections once and a stale preview dropped) and `scripts/test-general-model.cjs` (the theme list and storage keys, in
  `make test-web`).
- `make test-ui` also runs `scripts/test-notifications-page.cjs` (Settings → Notifications with requests held: the blocks in order
  with named and described rows; the switches, sound, slider and Preview writing the inbox's store and reading it back; a chosen
  sound that is gone falling back to the chime; the permission button for default, granted, denied, refused and unavailable;
  reads of the sound list answered out of order and a failed read; upload and delete one at a time, each followed by a read,
  with the uploaded sound chosen; a non-admin without the upload row; an answer after destroy) and
  `scripts/test-notifications-model.cjs` (the pure rules, in `make test-web`).
- `make test-ui` also runs `scripts/test-usages-page.cjs` (Settings → Usages' view and switcher, over the app's mock of the usages
  API with calls recorded and chosen ones held: the cards, profiles and limit windows; a person who is not the switcher's owner
  seeing no controls; switching and renaming asking first and naming who is affected, a refused action saying why; two reads
  answered out of order drawing the later; a push from the event stream applied at once, waiting while a field has focus or a
  read runs, and ignored while the section is hidden; the settings form and the "refreshed moments ago" message; token
  auto-refresh polling until the server has converted the logins and the warm-up schedule; GitHub sign-in polling, its link, and
  its stop on destroy; Mistral and Ollama credentials; an answer after destroy) and `scripts/test-usages-model.cjs` (the pure
  rules, in `make test-web`).
- `make test-ui` also runs `scripts/test-quota-page.cjs` (the Usages page's three blocks over sources with calls held: hidden until
  the owner of an enabled switcher asks; the tools, notes and orders; a move keeping focus on its row; a refused limit sending
  nothing and a message that typing leaves and a change clears; a save that waits behind a switch, once, then focus on its note;
  protecting, discard and the watched profile of the after-reset order; re-arm keeping a draft; reads answered out of order, a
  403 or 404 hiding the block and a failed read keeping it; a save answered after destroy; the limit guard's validation, save and
  override; the default daily limits' visibility, validation, saved note and refusal) and `scripts/test-quota-model.cjs` (the pure
  rules, in `make test-web`).
- `make test-ui` also runs `scripts/test-users-page.cjs` (Settings → Users with requests held or answered by rule: the tab holding its
  place until the access check answers; the blocks in order; members and accounts with their badges and actions; a role change and a
  removal writing and reading again; a refused role or admin switch put back; a reset link selected, a cancelled deactivation sending
  nothing; an invitation for the chosen project and role, with the local-address hint; a person who is not an admin seeing only the
  projects they own; two access checks answered out of order; and a destroy while a read or an invite is held sending nothing more) and
  `scripts/test-users-model.cjs` (the pure rules, in `make test-web`).
- **Dialogs** share one modal look (v1's modal pane, in `web/v2-only.css`) and one dismissal rule (`web/js/backdrop-close.js`): a click on the backdrop
  of a modal dialog (a press and a release both on the dialog element, outside its content or its `.settings-pane`) dismisses it like Escape, by
  sending the same cancelable `cancel` event, so a `cancel` listener can veto it. `make test-ui` runs `scripts/test-backdrop-page.cjs` for it.
- `make test-ui` also runs `scripts/test-machines-page.cjs` (Settings → Machines and its pairing dialog with requests held or answered by
  rule: the blocks in order and "Loading machines…" until the list answers; the cards with their status and icon actions; an admin who
  is not the owner seeing the pairing, approve and revoke controls off with the reason, and a 403 hiding Add machine; approve writing
  and reading again, a cancelled revoke sending nothing, the stop note; the pairing dialog opening empty and focused, the trimmed name,
  the copy button and a copy that settles last deciding what shows, Close, Escape and Cancel; two reads answered out of order; and a
  destroy while a read, a pairing or a stop is held) and `scripts/test-machines-model.cjs` (the pure rules, in `make test-web`).
- `make test-ui` also runs `scripts/test-project-page.cjs` (the project's settings form in the home room's settings, and the room's own agent
  control and usage, with requests held or answered by rule: the blocks in order with their values; who may change what (the project's
  owner, an admin, anyone else, an archived project); the details save sending only what changed; the switches saving at once and going
  back when refused; the daily limits (a value box only for Custom, a bad value refused, a stale revision refreshing, a save that did not
  take putting the form back); the agent control asking before it stops and retrying a resume; the default agents of each role; archive,
  reopen and delete with their confirmations; reads answered out of order; and a destroy while a read or an archive is held) and
  `scripts/test-project-model.cjs` (the pure rules, in `make test-web`).
- `make test-ui` also runs `scripts/test-shell-page.cjs` (the settings dialog's header and nav in Chromium: the tabs in order under their two
  captions with General selected, the title and the close button; what the owner, an admin and anyone else may see, and a refused usage
  read taking the Usages tab away; the Users tab holding its place and then showing or going; choosing a tab showing its section alone and
  setting the title and the dialog's section; a destroyed shell drawing nothing more) and `scripts/test-shell-model.cjs` (the pure
  rules, in `make test-web`).
- `make test-ui` also runs `scripts/test-health-page.cjs` (the Health panel in Chromium with requests held; first written against the hand-written
  panel and run unchanged against the page that replaced it: the badge and its labels, the entries and where one goes, the limit rows,
  the limit guard asked once and not again after a refusal, reads answered out of order, the disk read and its race, the dry-run then
  confirmed cleanup, a refresh not wiping an open preview, the failure texts, the daily schedule's saves; and a destroy while a read or a
  cleanup is held sending nothing more) and `scripts/test-health-model.cjs` (the pure rules, in `make test-web`).
- `make test-ui` also runs `scripts/test-knowledge-page.cjs` (the Memory viewer in Chromium with requests held; first written against the hand-written
  viewer and run unchanged against the page that replaced it: the scopes with their status lines and usage bars, the memory switch, add, save,
  delete (asked first, with the revision), the activation of a global record, clearing the view one request each, refusals, a view the
  person has left not drawing, events, the room's task-notes and project-memory button and a failed read; and what was begun on a view
  the person has left not touching the one they are on, and a destroy while reads are held) and `scripts/test-knowledge-model.cjs` (the
  pure rules, in `make test-web`).
- `make test-ui` also runs `scripts/test-inbox-page.cjs` (the Inbox view in Chromium over a source whose calls are held; first written against the hand-written
  view and run unchanged against the page that replaced it: every kind of item with its words and decisions for someone who may and someone
  who may not decide it, the count, the toggle's name and the tab title, cards that keep their nodes and what was typed in them across
  a new list, a changed item replaced and a gone one dropped with its group, the newer of two reads winning, a card waiting for an answer
  not answered twice and a retry keeping its idempotency key, a stale item read again, the raw record, jumping to a room, opening Usages,
  dismissing one and every resolved failure, a change the source announces, and reset; and a destroy while a read, an answer or a bulk
  dismissal is held sending nothing more and the source let go) and `scripts/test-inbox-model.cjs` (the pure rules, in `make test-web`).
- `make test-ui` also runs `scripts/test-claude-import-page.cjs` (the Claude Code sessions import dialog over a source with calls held: it opens on the host
  machine with the native harness, the default model and preset roles; reads answered out of order; starting reads the role boxes then,
  locks the dialog while it runs and the started rows after; and a destroy while a read or a start is held sends nothing more) next to
  the three import steps of `test-ui` itself, and `scripts/test-claude-session-import.cjs` (the pure rules, in `make test-web`).
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
  - a page opened with `wanted` shows that machine, and the page reports the machine it shows (`viewSnapshot`), on a machine other than the first;
  - text typed while a save is under way stays, and still counts as unsaved;
  - the same input node, the focus and the selection survive another card's refresh, and the node survives its own card's
    save;
  - after unmount, a pending answer draws nothing, throws nothing and starts no request;
  - unfinished input (an Edit form, the Add harness form, a model id typed but not added, ticked auto-detect results for
    harnesses or models) counts as unsaved while it is there and is gone with the page; a cancelled Add harness form no longer
    counts as unsaved;
  - removing a harness: the in-app confirm, the DELETE, and the list read again without it;
  - the list beside the detail: the first harness chosen, one card shown, click and arrow keys move the choice, a reload keeps it;
  - a models save sends the revision its draft started from, and one refused as stale drops the models draft (not the Edit form's), reads again and says so;
    a save moves the draft on to the revision it answered with, so an edit made during the save, and the next edit after a
    failed re-read, are not refused as stale;
  - the providers panel of an OpenCode harness works the same way (its own `providers` draft; a `409 stale_providers` drops it and reads again; `409 provider_in_use` lists every provider that would lose models and offers the save again with `remove_models`; any edit withdraws the offer), with the key status read from `key-status` and the import's suggestions joining the unsaved list only;
  - role defaults: a switch per role on every model saves the role on its own, the role panel names its entries (those of
    another machine are read once), a refused save says why and the views show what is stored, and a role save made in
    Settings → Roles is read here while the page is shown and ignored while it is hidden.
  - the shell's `onReady` hears of a harness that can start an agent (the first-run harness offer relies on it).
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

Each step is its own ticket and change, with the gates in section 6. Since 2026-10-27 a step replaces its legacy modules in the
same change (section 5); the experiment, default-flip and removal steps described below were not used.

### Step 1: runtime and Settings → Harnesses (done)

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
  late-answer guards, `card.js`, `models.js`, `add-harness-form.js`, `detect-panel.js`, `detection-details.js`, `activity.js`, `model.js`, `id-prefix.js`, and for layout C and role defaults `master-list.js`, `role-panel.js`, `role-switch.js`), `web/js/settings.js` (uses the
  adapter), `web/js/ui-variant.js` (`ACTIVE`), `web/theme-v1.css` (a two-option pill was 33 px wide between 521 and 700 px,
  under the 40 px tap target; it is 40 px now), `internal/server/static_assets.go`, `.air.toml`, tests and docs.
- It first ran as the `preact-harnesses` experiment; the legacy `web/js/harness-settings.js`, the switch and the experiment are removed,
  and the onboarding step embeds the page.
- Acceptance: `make test-full` and `make test-ui` pass.

### Step 2: the other Settings sections

Done so far: Prompts (`web/js/pages/prompts/`), Macros (`web/js/pages/macros/`), Roles (`web/js/pages/roles/`; its default-agents picker is the shared `components/spawn-defaults.js`, which Project uses too), General (`web/js/pages/general/`: appearance, the chat switches, message expiry and the settings backup; the chat switches read `chat-preferences.js`, which the system-events feed shares, and the expiry window is the shared `retention.js` store), Notifications (`web/js/pages/notifications/`; the page reads and writes the inbox's `InboxNotifications` store, which the sound upload and permission button go through), Settings shell (`web/js/pages/shell/`: the header and nav of the dialog, over a store of the selected section and the account's rights; the Users page owns the state of its tab), Claude Code sessions import (`web/js/pages/claude-import/`: the dialog opened from the Project page, a static `<dialog>` in `index.html` that Preact fills), Project (`web/js/pages/project/`: the project's settings form in the home room's settings as one tree over a `ProjectStore`, with the agent control and the usage summary as shared components in `web/js/components/` that Room settings mounts too), Machines (`web/js/pages/machines/`: the section and the pairing dialog are two static elements of `index.html` that Preact fills, sharing one store), Users (`web/js/pages/users/`: the access check behind the tab, the project's members, the accounts and the invitations; the tab itself stays in the adapter, since it sits in the dialog's nav and holds its place until the check answers), Usages (`web/js/pages/usages/`: the provider cards and the account switcher, the automatic-switching block, the limits before 100% and the default daily limits as one tree in the section; the stores are objects of signals because the shell and the event stream reach them too, and the two per-tool blocks share `ToolStore`: the server's copy, the owner's drafts, and the rule that a draft that was edited, or a field being typed in, is not replaced).

- Scope: the small sections first (prompts, macros, roles, retention, limit guard, quota), then users, machines and project;
  then the settings shell's nav and section switching (done).
- Files: `web/js/*-settings.js`, `settings.js`; new
  `web/js/pages/<section>/`; shared components as they are needed (`Dialog` around `dialogs.js`, `Notice`).
- Each section replaces its legacy module in its own change (section 5), with its `test-ui` checks moved to the Preact page and
  a held-request page test; there is no registry, experiment or A/B step. The shell's interface to each section (`tab`,
  `section`, `open()`) was unchanged until the shell itself moved, last: its header and nav are now Preact, the pages no longer make
  their own tabs, and `settings.js` keeps the open flow, the keyboard shortcuts and the rights it hands to the shell.
- The checkpoint after Prompts (review with the owner before the rest) is superseded: the owner's 2026-10-27 decision is to
  replace unit by unit, with a reviewer per commit.
- Acceptance: the section's existing `test-ui` checks pass against the Preact page, none weakened.

### Step 3: Health, Knowledge, Inbox

Done so far: Health (`web/js/pages/health/`: the popover over a `HealthStore`, with the disk section as `DiskUsage`, a store of its own; the toolbar's toggle and count stay in `index.html` and the adapter is their only writer).; Knowledge (`web/js/pages/knowledge/`: the Memory viewer over a `KnowledgeStore` that fills the static `#knowledge-dialog`; the toolbar's two buttons stay in `index.html`, the room's one written by the adapter); Inbox (`web/js/pages/inbox/`: the view over an `InboxStore`; `inbox-source.js` stays the one place requests and mutations go through, the adapter keeps and calls the unsubscribe it returns, and `answer-controls.js`, which the chat shares, is held in an island inside the card).

- Scope: the Health panel, the Knowledge (memory) viewer, the Inbox and its notifications.
- Files: `health.js`, `knowledge.js`, `inbox.js`, `inbox-source.js` (the source stays a plain store; the page reads it
  through a hook), `inbox-notifications.js`.
- Acceptance: Inbox items keep their identity and focus across live updates; answering, stale-item and read-only checks in
  `test-ui` pass.

### Step 4: agents and members, then the spawn and switch dialogs

Done so far: the panel of agent cards and people, with the profile and its Output section (`web/js/pages/agents/`: an `AgentsStore` of signals,
the cards keyed by agent id, `model.js` for the plain rules; the toolbar, the panel's title, its close button and the Spawn button stay in
`index.html` and the adapter is their only writer). The store keeps the record it holds for an agent whose data is unchanged, so a refresh
renders only the cards that changed and the clock re-renders only the age and the live chip. The Spawn dialog is a page of its own in the same
folder (`SpawnStore` over a `MachineStore` for the machine and its folder; `spawn-model.js` holds the pure rules for the tool and model lists, the
v1 banner, the worktree box and the request) and so is the Switch dialog (`SwitchStore`, sharing `MachineStore`). Switch first previews the
handoff, room history and handbook, then confirms the same target. The agent card polls switch progress and reads its action flags while handoff
is active, showing only the recovery controls the server permits. Every opening, catalog read, preview, call and folder save carries a number
and says nothing once a newer one started, the dialog was closed or the panel is gone.
The agent log popup (Show agent's logs, an icon button on an open profile for a project's owner or an admin) draws into the empty `#agent-log-dialog` of `index.html` (`log-dialog.js` over a `LogStore` in `log-store.js`, with the pure rules in `log-model.js`): the last 50 to 2000 lines of the same output with ANSI codes stripped, a highlight that is literal, case-insensitive and built as text plus `<mark>` elements (at most 2000, run again at most every 150 ms while typing; Enter and Shift+Enter step through the matches), and Follow, which sticks to the end until the person scrolls up and reads again on the agent's `supervisor_activity`, `turn_*` and `answer_*` events at most once in 4 s, every 15 s while the agent is in a turn and nothing else woke it, never while the tab is hidden, and after a 429 not before its `Retry-After`. Reads go through `agent-output-share.js`: a read already out for as many lines answers a second ask, and the tabs of the same person tell each other over a `BroadcastChannel` when they read and what came back, so two tabs following one agent spend one read against the server's 30 a minute. Escape closes the popup and the focus goes back to its button; under 600px it is full screen. The Output section of the profile stays as the short preview. A stream switch chooses which of the agent's two logs the popup shows: **Conversation** (the default, `stream=conversation`) and **Stderr** (what the profile's Output section shows); choosing the other one reads it at once, and the shared reader keeps the two apart. A conversation is drawn a record a line (`conversationRecords` in `log-model.js`): the record's time and a label for its kind (Turn, Prompt, Assistant, Thinking, Tool call, Tool result, Error, Event, Withheld, Turn end; an unknown kind shows its own name) as elements of the page, then its text as text nodes, with the tab-indented lines of the log as the rest of that text. `Withheld` is the server's line for a turn the reader may not see, `Event` an event of the harness the adapter did not know, and the log's `... earlier part cut` line is drawn as a `Cut` record. Lines, Follow, Download (the text as the server sent it) and the highlight work the same on both; the highlight searches the records' text, not the labels, with one count across all records.
The room dialog's member list is a page of its own (`web/js/pages/members/`: a `MembersStore` whose loads take a ticket, with `model.js` for the state words); and so is the read-only profile of a sender that is no longer a live member (`web/js/pages/sender-profile/`: `openSenderProfile` keeps its call and mounts a page in a dialog of its own, over a store whose profile read dies with the dialog).
Done for the Claude Code session picker the Spawn dialog opens (`web/js/pages/claude-sessions/`): `store.js` holds the listing, the line above it and the error, and every read (the folder's sessions, a session pasted by id) carries a number and says nothing once a newer one started, the dialog was closed or the store is gone; `picker-dialog.js` draws into the empty `#cc-sessions-dialog` of `index.html`, as the import dialog does into `#cc-import-dialog`, and the two share the row pieces in `rows.js`; `page.js` keeps the `ClaudeSessionPicker` that `SpawnStore` opens. The id box is left to the browser and read when its form is sent. `claude-sessions.js` keeps only the pure helpers (facts, flags, titles, the listing line).

#### Stop-rule checkpoint after step 4

The design's rule: compare the migrated pages with the legacy ones for size, bugs and test churn; if they are not clearly simpler, stop and
keep the islands. The numbers for `2c5498b..` the end of step 4 (22 commits):

| Measure | Legacy | Preact | Note |
| --- | --- | --- | --- |
| Production lines of the step's modules | 1198 (`agents.js` 967, `room-members.js` 84, `sender-profile.js` 83, `machine-selection.js` 64) | 1427 (1272 in `pages/agents`, `pages/members`, `pages/sender-profile`, 155 added to `spawn-model.js`) | about 19% more; 319 of them are import-free model code run in a vm |
| `git diff --numstat` of `web/js` without `pages/*/model.js` | | +1267 -1202 | |
| Bugs | | 13 `fix(web)` commits for races in the old code (stale answers after a reopen, a room change, a cancel; folder saves; late close events) and 5 more holes found in review of the new code and folded into its commits | each with a test that fails without its guard |
| Test churn | `test-ui.cjs` and the audit fixture +31 -37 (module paths, five steps that called class methods or built a card, the age and member-list steps) | new page tests and vm tests: `scripts` +2012 -38 | the old checks survived |

Assessment: the new modules are not smaller. Their structure is partly simpler: the rules (state words, disabled reasons, the v1 banner, the
Switch and Spawn requests) are pure and tested, a card is no longer rewritten on every refresh, and every stale answer is a numbered, tested
guard. The same number of guards still has to be written by hand, now as tickets, and the store, model and component split adds files. It is
not clearly simpler on size, bugs or churn.

Owner's decision (2026-10-28): continue with the remaining units.

### Step 5: sidebar and navigation

- Scope: the room list (unread counts, ordering, collapse, selection), mobile tabs.
- Files: `sidebar.js`, `sidebar-order.js`, `room-unread.js` (pure, stays), `mobile-tabs.js`, the wiring in `app.js`.
- Acceptance: unread and ordering tests unchanged; selection and collapse survive live updates.
- Done for the room list: `web/js/pages/sidebar/` holds `store.js` (signals for the rooms, projects, filter, sort, manual order and fold state; every read,
  call, archive search, mute timer and sort or order save is tied to a generation, a ticket or an abort controller and says nothing once the list was
  reset or destroyed, and a destroyed list sends no request at all; a sort or order save still queued behind another one when the list is reset is
  dropped, since a reset only follows a sign-in or a workspace reset, which load the preferences afresh, and of several queued saves of one name only the
  newest goes out), `model.js` (import-free: the orderings, the unread rules (`roomUnread`: only a home room and a direct message carry an unread badge, each counting its own messages; a folded home row adds nothing from the rooms under it and no child room shows a badge, while the 🔔 attention bell stays on every row), `ReadMarker`, and `treeOf`, which turns the state into the rows to
  draw; vm-tested), the components (`sidebar-list.js`, `sort-menu.js`) and `page.js`, the adapter that keeps the `Sidebar` API `app.js` uses. A room keeps
  the same record object while its data is unchanged, so a refresh redraws only the rows that changed. The legacy `sidebar.js`, `sidebar-order.js`
  and `room-unread.js` are gone; the page tests written against them first (`test-sidebar-page.cjs`) run unchanged on the new code.
  The first load is streamed: `/api/projects` draws each project as a placeholder (`pending`) in its final place (the manual and by-name orders need no room;
  the by-latest-activity order waits for every list), each project's rooms replace it as its own list lands in any order (so `rooms` holds them in arrival
  order and `firstRoom()` walks the projects in server order), and a project the browser has not seen is drawn folded already, without saving it, except the
  one holding the room the app will open (`expect()`, then `focusRoom()`: the address, the last room, else the first room), so opening that room changes
  nothing that was drawn. `scripts/perf-sidebar-paint.cjs` measures it.
  Under a project's home row `treeOf` puts the system rooms first (`systemRank` in `model.js`: general, routing, other flow rooms, read from `kind`, `purpose` and
  `team_flow_room_key`, never the title), and gives the home view a `divider` index where the rest begin; `Group` draws a `.room-divider` (`role=separator`) there.
- Done for the room header's phone overflow menu: `web/js/pages/room-menu/` (`store.js` reads the header's buttons through a mutation observer and the
  phone breakpoint, `room-menu.js` has the button and the menu, `page.js` mounts them in two box-less hosts, the menu on the body). Its items still click
  the real header buttons. `room-menu.js` is gone.
- Done for the phone's bottom tab bar: `web/js/pages/mobile-tabs/` (`store.js` follows the page's drawer classes with a mutation observer and presses the
  drawers' own buttons, `mobile-tabs.js` draws the three tabs into the `#mobile-tabs` nav, `page.js` mounts them and keeps `initMobileTabs()` for `app.js`).
  `mobile-tabs.js` is gone. What remains of this step is the wiring in `app.js`, which belongs to the shell step.

### Step 6: chat (last)

- Scope: the message list and thread panel. `scroll-anchor.js` and the message window stay; each message becomes a keyed
  component; the composer (ProseMirror) stays imperative inside a ref-mounted host.
- Files: `chat.js`, `message-actions.js`, `visual-plan.js`, `composer*.js` (host only). (`answer-controls.js` is now the Preact component `components/answer-controls.js`: `AnswerControls`, `AnswerHistory` and the Open Questions dialog, a body-level root of its own keyed by item; it survives its row being drawn again and sends through the newest controls of the item, and closes, forgetting the key, once the last controls of the item are gone.)
- Acceptance: the existing message identity, draft and scroll-under-live-update checks pass; no scroll jump with
  late images, Mermaid or attachments.
- Done for the room panel (the header and the settings dialog): `web/js/pages/room-panel/` holds `model.js` (import-free: the header's words, who may
  rename, which controls the dialog shows and how, the worktree, memory and notes sections; vm-tested), `store.js` (the room being shown and the calls
  that change it: title, memory setting, worktree, archive and reopen, leave, remove, hide and export, each taking the panel's session and the room it
  was started for, so an answer for a room that was left, a reset or a destroyed panel changes nothing and starts nothing), `room-panel.js` (the
  components) and `page.js`, which writes the header's elements from an effect and fills the dialog's parts: the rename form, the facts, the controls,
  the error line and the worktree, memory and notes sections. `Chat` keeps its methods and properties for the app and the tests and delegates to it.
  While a worktree is loading its buttons stay hidden, where the old dialog showed the previous room's.
- Done for the message rules: `web/js/pages/chat/model.js` (import-free, vm-tested) holds what a message reads like in a quote, the thread counts and their words,
  how reactions group, what a live refresh does with a message of a page (skip, draw, count as a new reply, count on Back to latest), how far the read mark
  may go, the caps of the list, when a reply answers a question through the Inbox, what a message's answer area shows, and the walk back through the
  viewer's own messages. `Chat` calls it and keeps the DOM work; `plainText` and `messageSnippet` are still exported from `chat.js`.
- Done for the message calls: `web/js/pages/chat/store.js` holds the room's records and cursors and the calls that load them (the newest page, the live
  refresh, older and newer pages of a history window, a jump, the reactions), each carrying the generation of the open view (a reset, an open and a jump move
  it) and saying nothing and starting nothing once it is gone or the chat is destroyed. `Chat` still draws: it gives the store the DOM work as host methods
  and reads the store's fields through accessors. The store also holds the thread's records and calls (the thread read, older replies, a thread reply) and the sending of a message, keyed by the thread's own generation, which a reset, an open and a window all move.
- Done for the thread panel: its entries are a keyed Preact list (`web/js/pages/chat/rows.js`) in `#thread-messages`, fed by `Chat.threadUpdate()`, which renders at once so callers read the rows back. A row draws again only when its record or its page-computed key (author, Markdown mode and mention index, answer state) changes. The imperative leaves (Markdown body, visual plan, answer controls and history, sender presence, time) are islands: Preact renders the leaf's own root element and the leaf's `fill…` function fills it, with no wrapper. Older replies keep the reader's place through a layout-effect restore.
- Done for the message list: `#messages` is the same kind of keyed list (`MessageList` in `rows.js`), fed by `Chat.listUpdate()`. A row's key adds the thread count, the reaction pills, the quoted parent and the expiry marks, which are drawn by the row instead of patched in afterwards; `Chat.nodes` is filled by the rows' refs. Adding a page batches the rows into one update, and dropping rows, prepending a page and the live append keep their scroll compensation (a layout-effect restore, or the same synchronous sequence around the update).
- A message row's toolbar is made when the row is reached, not drawn with the row: `Chat.delegateRows` listens (pointerover, focusin, pointerdown) and `wakeMessageActions` draws the buttons into the row's `.message-actions`, which `Actions` in `rows.js` renders empty; until then the bar holds one visually hidden "Message actions" button, so the keyboard can still find it. The toolbar is a Preact root of its own inside that element (`Toolbar` in `message-actions.js`), drawn at once by `render`, so a caller reads the buttons back; the buttons carry their own click handlers (the delegated `messageAction` is gone), the quick-emoji row's open state and a copy button's "Copied" tooltip are component state, and one document-level click and Escape listener closes the one open pop-up. `releaseMessageActions` goes back to the placeholder for the oldest once more than three are live, keeping one that holds the focus, the pointer, a tap selection or an open pop-up. A built toolbar follows its message and thread label (`drawMessageActions`) and is cleared when its row goes. Rows also leave out the empty reaction-pills container and the hidden thread chip. Tests reach a row first (`page.hover`, or a `pointerover` on it) before they look for its buttons; a change of the toolbar's own state (a pop-up opening) is drawn before the next frame.
- The thread panel shows the conversation as `tree` prints it: `treeLayout` (`pages/chat/model.js`, pure, model-tested) orders the entries depth first (replies in the order they were written) and says, per row, its guide cells (a pipe where an ancestor has replies still to come, blank where it was the last), whether its own connector is a tee or an elbow, and whether it is past the six-level cap (it then adds no step and names the message it answers) or the root of its own because its parent is not loaded. The gutter's lines are drawn by `v2-only.css` in cells of `--tree-step` (narrower under 700px), and an own row spans the panel with its bubble at the right so its gutter lines up with the others'. Because a reply lands under its parent and not at the end, the live paths know it: a reply from someone else that lands anywhere but the end is marked (`Chat.threadInserted`, a `TreeNewButton` pill with the count and the way to it, which goes when the row is in view and highlights it if it came in view), the viewer's own send scrolls to its row and highlights it (`threadShowSent`, instead of the panel's end), and older replies hold the first row in view after the first message (`holdThreadPlace`).
- The emoji picker (`web/js/emoji-picker.js`) is one instance made on the first open and reused, a non-modal popover (no `showModal`), with a virtual grid: rows of fixed height (a heading or a line of cells), the visible ones plus three either side placed in a canvas as tall as all the rows, tabs and search working on the entry list, delegated events, a roving Tab stop that moves across rows that are not drawn. It is removed from the page on close, so between opens it costs nothing. `scripts/test-emoji-page.cjs` holds it to a screenful of cells, reaches every emoji by scrolling, and opens and closes it thirty times.
- The answer controls, the history and the answered marker of a main-list message are a row of the list under the message's card (`AnswerSlot` with `listed`, class `answer-row`), not part of the card, aligned with it on the viewer's side; the scroll anchor watches `.message, .answer-row`. In the thread panel they stay inside the entry.
- The scroll anchor reads who moved a pinned list. A scroll event with no reader input (wheel, touch, the scrolling keys and Tab with focus in the list or on the body, focus moving into a row, the scrollbar for as long as it is held, the momentum that follows, or the 30 seconds after the find shortcut or F3); an input counts until the scroll event it caused has been handled, however late that is (a busy page can deliver it long after), and a quiet list moved far up at its recorded height is the reader's too is the layout's: content that shrinks and grows back under a list pinned to the bottom (Mermaid and code highlighting swap rows for shorter placeholders for a moment) makes the browser clamp `scrollTop`, and the late event reports it against the regrown layout. The list re-pins instead of reading the clamp as the reader leaving the bottom, which had left a room opened at an arbitrary row (seen in Safari, on rooms with Mermaid plans). `scripts/test-ui.cjs` clamps a list that way, and checks a room switch, a pinned live append and a scrolled-up live append.
- Done for the message box: the files a message carries (picking, pasting, dropping, uploading and removing them) are the store's, and the strip of pending files is a Preact root in `#pending-attachments`. `Chat` no longer has its own session counter or `current()`; the store's calls, the sends and the uploads all use the store's. The composer itself (the editor, its autocompletes, the reply chip) stays hand-written and is reached through the chat.
- The store owns the rows, the thread's entries, the per-message maps (records, counts, reactions, quotes), the open questions and the answered set, the pending files, the expiry plan and every transition of them (`add`, the trims, `dropRows`, `markNewThreadReply`, opening and closing a thread). What it asks the chat to do is what has to touch the page, and nothing else: render (`listUpdate`, `threadUpdate`, `renderPending`), scroll and measure (`holdPlace`, `holdThreadPlace`, `listPlace`, `followList`, `keepPlace`, `rowBoxes`, `listBox`, `listAtBottom`, `pageDrawn`, `windowDrawn`, `showNewest`, `showWindow`, `showThreadPage`, `showRow`, `showReply`, `loadOlderIfShort`, `anchorUpdate`, `hasOlder`, `noteRow`), focus and the composer (`beginThread`, `restoreThreadDraft`, `sendInput`, `beginSend`, `sendAsAnswer`, `sentMessage`, `sendFailed`, `sendFinished`, `beginThreadSend`, `sentThreadReply`, `threadToEnd`, `threadSendFailed`), status text and buttons (`say`, `threadError`, `showLatestButton`, `updateLatestLabel`, `showOlderStatus`, `showThreadOlderStatus`, `showThreadCount`, `updateThreadNewMarker`, `hideThreadLatest`, `threadLatestHidden`, `threadClosed`, `beginOpen`, `beginWindow`), the live announcer (`announce`), and the mention highlight timer (`scheduleMentions`). `authorName` and `threadLatestHidden` are queries; a row's view is computed from the store without writing to it.

### Step 7: the shell

- Scope: `app.js` boot, sign-in, setup and onboarding, the top bar.
- Files: `app.js` (the boot stays imperative; see below); `setup.js`, `welcome.js`, `onboarding.js` and `topbar-menu.js` are replaced.
- Acceptance: boot and sign-in checks pass; after this step the legacy modules can be retired one by one.
- Done for sign-in and the invitation card: `web/js/pages/signin/` draws `#login-view` (the sign-in form and the card a link opens) and the dialog a signed-in person gets for a pending link. `model.js` holds the card's texts, `store.js` the state and the three submits, `components.js` the markup, `page.js` the adapter that writes the view's `hidden` and the dialog's open state. Every request takes the store's ticket; `show()`, `hide()`, switching to the ordinary sign-in and `destroy()` start a new one, so a late session, error, inspection or redemption changes nothing, and a second submit while one is in flight sends nothing. `app.js` only constructs the page and calls `show()`, `hide()` and `openDialog()`; routes and API calls are unchanged. `scripts/test-signin-page.cjs` proves the guards against a scratch server, `scripts/test-signin-model.cjs` the rules.
- Done for the first-run setup: `web/js/pages/setup/` draws the welcome, the setup-token step, the Create owner form and the theme step, replacing `setup.js` and `welcome.js`. `model.js` holds the owner form's checks, the throttle sentence and the arrow-key move between themes; `store.js` is the whole run (token, owner, theme) with a ticket per request, so a later submit or `destroy()` makes a late answer change nothing, and a `/setup?token=` check and a token typed meanwhile are decided by the later one; `welcome.js` has the welcome's store and component, which `app.js` also uses for the owner's first sign-in; `components.js` and `page.js` draw each step into its own `main` and write only that view's `hidden` and the page's theme. `ownerSetup()` still returns the session the way `app.js` expects. `scripts/test-setup-page.cjs` proves the guards (the link and a typed token in both orders, the owner created, the welcome recorded, the preferences read and the theme saved after a destroy, the welcome's timer and request), `scripts/test-setup-model.cjs` the rules.
- Done for the onboarding: `web/js/pages/onboarding/` draws `#onboarding-view` (Create your first project, the Harnesses step, How aimebu works), replacing `onboarding.js`. `model.js` holds the project check, the harness status sentences and what the last step offers; `store.js` holds the steps and every transition of them, including the readiness poll and the one-run-at-a-time lock on adding harnesses; `components.js` draws the three sections; `page.js` keeps the `Onboarding` API `app.js` and the tests use (`show`, `showHarnesses`, `close`, `open`, `harnesses`, `continueHarnesses`, `stopWatching`, `watch`). Every call it makes takes the store's ticket, and `show()`, `close()`, opening or leaving the Harnesses step and `destroy()` start a new one, so a creation, a readiness question, a poll, an addition or a confirmed skip that answers late changes nothing and asks nothing further. The embedded Harnesses page (`HarnessSettings`) still mounts into the step's empty `#onboarding-harnesses-host`. `scripts/test-onboarding-page.cjs` proves the guards against a scratch server, `scripts/test-onboarding-model.cjs` the rules.
- The first-run card is a modal: `#onboarding-view` is fixed over the viewport (it scrolls inside itself), `#app-view` is inert while it is open, and it closes with the session. Continue on the Harnesses step adds what is ticked (if anything: a harness set up by hand counts too) and lands on a result step in the same card (the harnesses and models found, then the role defaults, which are not shown before a model exists, and Continue there goes on to the last step, or closes when the step was opened on its own). The embedded page takes `result` and `showRoles` through `HarnessSettings.showResult()`, and the empty-state line is not shown under a detected list.
- Done for the top bar's overflow menu: `web/js/pages/topbar/` draws the items of `#more-menu`, replacing `topbar-menu.js`. `model.js` holds the arrow-key move and `planTuck`, the pure rule for which controls the phone bar gives up (Sign out, System events, Memory, then Health, only as far as needed), `store.js` whether the menu is open, the items (each mirroring its real button's hidden state and badge), the health mark and which item takes focus, `components.js` the items, and `page.js` the adapter. The adapter owns what stays outside the items: the toggle (its `aria-haspopup`, `aria-expanded` and `has-issues`, since the toggle is a bar button), the menu's `hidden`, the observers of the real buttons and the document's click and key listeners, and the measuring: on a phone it reads the bar's room, asks `planTuck` and tucks controls with a class on the real button (`topbar-tucked`), so the menu offers an item only for a control that is out of the bar, and the toggle carries the Health warning only while Health is; a menu with no bar around it offers every item. `destroy()` disconnects the observers and removes the listeners, so a destroyed menu neither follows the buttons nor answers the page (`scripts/test-topbar-page.cjs` proves each, against fixture buttons). The items still forward their click to the real button. Focus never drops on the body when the layout changes under an open menu: when the focused item stops being offered (its control came back to the bar) the adapter moves focus to that control at once, even while the menu stays open for the controls still tucked, and otherwise to the first item still on offer.
- Done for the v1 import wizard: `web/js/pages/import-v1/` draws the children of the wizard's dialog, replacing the hand-built class. `model.js` (import-free, `scripts/test-import-v1-model.cjs`) holds the step list, the page status lines, the people, room and DM wording, the macro cards, the footer totals and the summary's lists, accounting and counts; `store.js` holds the phase, the step, the preview, the mapping (the owner's choices, saved as a draft in the browser under the unchanged `aimebu2.import-draft.<source_key>` key), the New project form, the problems Next found, the dry run (with a generation, so a late answer for a page the owner left is dropped) and the import job's poll (with a generation that closing the dialog, leaving the import phase or a new poll ends); `components.js` draws each page with the hand-built ids, classes, roles and texts and no wrappers. `web/js/import-v1.js` stays the lazy chunk's entry (nothing static imports `pages/import-v1/`) and keeps `ImportV1Wizard` as an adapter: it owns the `<dialog>` (its open state, `data-phase`, the cancel guard), its fields are the store's, its element fields (`content`, `status`, `error`, `actions`, `skipZone`, `progressLabel`…) are the rendered elements, and `render()`/`refresh()` render synchronously, so `test-ui` and the audit fixture drive it as before. The workspace folder field stays an island (`attachFolderField` through a ref); Next's problems are drawn at their fields from the store, and the first is focused through the content element. Redraws patch the DOM in place, so the focused control and the scroll position survive them without the old focus bookkeeping.
- Done for the shared in-app dialog (`confirmDialog`, `promptDialog`, `formDialog`, `noticeDialog`): its body is the Preact `AppDialog` (`web/js/pages/app-dialog/app-dialog.js`) and its field rules (showIf, disabledIf, required, validate, the reset button's state, where the focus starts) are `model.js` there, import-free and vm-tested. `web/js/dialogs.js` keeps the API, the one-at-a-time queue, the generation `cancelAppDialogs()` moves on sign-out and the `<dialog id="app-dialog">`, which it makes on the first call (page tests use the dialogs without `app.js`) and draws with a synchronous `render()` before `showModal()` and the focus. The inputs are uncontrolled and read when the form is sent, so a redraw for showIf or disabledIf leaves what was typed alone; each call is keyed by its number, so it starts from fresh inputs, and the field ids still go by position (`app-dialog-field-N`, `-hint`, `-reset`). The hand-built dialog code is gone.
- Done for the three bars (the update bar that asks for a reload after a deploy, the update banner that reports the server's update check to the admins, and the offer of browser notifications): `web/js/pages/banners/` holds `model.js` (import-free, vm-tested: `reloadIsSafe`, when each bar may show, what the update banner says, and a controller per bar with its timers, the permission and the tab's visibility handed in), `views.js` (the bars' contents; the visibility and permission listeners are a layout effect with its cleanup) and `page.js`, the adapters `app.js` uses (`UpdateBar`, `UpdateBanner`, `NotificationBanner`) with their old methods. An adapter makes its bar's element itself, so `.bar` exists as soon as it is constructed (the update banner is placed after the notification banner's), keeps its hidden state and draws the contents with a synchronous `render()` on every change; the update bar is still made on its first call and put first in the body. `scripts/test-banners-page.cjs` checks the elements, their places and buttons in Chromium. `update-bar.js`, `update-banner.js` and `notification-banner.js` are gone.
- What stays imperative: the rest of the `app.js` boot (routing between views, starting and ending a session, wiring the panels together and the startup state) remains imperative orchestration. This step replaces the pages `app.js` used to build by hand (sign-in, setup, onboarding, the top bar's menu); `app.js` now only constructs them and calls their small interfaces. It is not rewritten as components, and no part of it is planned to be.

## 9. Open questions

- Build: stay on A, or move to B (esbuild as a Go `tool` directive for JSX and TypeScript)? Decide with the incoming frontend
  owner.
- Should the Chromium checks join a CI gate (today CI runs `make test-full` only)?
