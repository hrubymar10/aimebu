Vendored frontend dependency:

- `mermaid.min.js` is Mermaid v11.15.0 (MIT). It is a byte-for-byte copy of
  aimebu v1's `frontend/vendor/mermaid.min.js`, originally downloaded from
  `https://cdn.jsdelivr.net/npm/mermaid@11.15.0/dist/mermaid.min.js`.
- SHA-256: `70137e77bb273bb2ef972b86e8b0400cca8be53cb25bfc45911a186dc98665de`.

- `gemoji/emoji.json` is the emoji database from github/gemoji (MIT, see
  `gemoji/LICENSE`), release tag `v4.1.0` (commit
  `5476a66d2794e0d1551b1f96e449afc72e9f7bec`), source
  `https://raw.githubusercontent.com/github/gemoji/v4.1.0/db/emoji.json`
  (1870 emoji; upstream file SHA-256
  `b174ae2aeb321b52f64adb9ff412f966a7f338839d780784dd15dcad702c2dd6`).
  It is trimmed to the fields the emoji picker and the `:shortcode:`
  suggester use (`emoji`, `description`, `category`, `aliases`, `tags`,
  `skin_tones`) and minified; no custom or image emoji are shipped.
  SHA-256 of the shipped file:
  `a1b07e31b99b20a68785022e17833b5dacd699c9b5d9ae161af4f6d643e446a7`.
  To update, download `db/emoji.json` and `LICENSE` from the new tag, then run
  from this directory:

  ```sh
  node -e 'const d=require("/path/to/emoji.json");process.stdout.write(JSON.stringify(d.map(e=>({emoji:e.emoji,description:e.description,category:e.category,aliases:e.aliases,tags:e.tags,...(e.skin_tones?{skin_tones:true}:{})})))+"\n")' > gemoji/emoji.json
  sha256sum gemoji/emoji.json
  ```

  Update the tag, commit and both hashes above. `node` serialises the same
  bytes as the trim used for the current file.

- The Sign out icon in the top bar (`#logout` in `web/index.html`) is the Phosphor Icons "sign-out" glyph, MIT,
  Copyright (c) 2023 Phosphor Icons (`https://github.com/phosphor-icons/core`, upstream `LICENSE` reviewed). It is inlined in the
  page, so there is no file here; this entry is where its licence notice lives.

- Navbar icons, in `web/icons/` and drawn through a CSS mask in the current colour (so each is replaceable by replacing its
  file): `inbox.svg` and `spawn.svg` (the owner's "new-indicator" file), and `left.svg` and `right.svg` (copies of
  the icons of the same names in aimebu v1). The project owner confirmed on 2026-10-19 that these four are **MIT** licensed, and
  that is recorded here as their licence. They were cleaned (the download's header comment, title, fixed size and element ids
  removed, fixed colours changed to `currentColor`); their paths are otherwise unchanged.

- Icons that came from an icon catalogue's downloads (SVG Repo) are MIT or another permissive licence, per the owner. No SVG file in
  the repository carries the catalogue's header comment, and `scripts/test-svg-headers.cjs` (in `make test-web`) fails if one does.

- `web/icons/trash.svg` is the SVG Repo "trash can" icon, MIT per the owner. The paths are unchanged; it was adapted to the
  repository's icon format (the download's XML declaration, header comment and fixed 800 px size removed, the black strokes changed
  to `currentColor`), and it is drawn through a CSS mask like the other glyphs.

- `web/icons/star.svg` and `web/icons/select-all.svg` are the owner's SVG Repo assets (MIT). Their catalogue headers, fixed sizes,
  fixed colours and unused wrappers were removed. The star keeps its outline path for CSS fill on the selected model; select-all is
  drawn through a `currentColor` mask.

- `web/icons/edit.svg`, `eye-off.svg`, `link.svg`, `archive.svg` and `key-off.svg` are the owner's SVG Repo assets (MIT per the owner:
  the "edit", "eye-off", "link-6", "archive" and "key-no" downloads). Their catalogue headers, doctype, fixed 800 px sizes, embedded
  styles, element ids and fixed colours were removed and the fills use `currentColor`; the paths are unchanged. They are drawn
  through the CSS mask: edit is Edit/Rename on a role card, a macro, a usage profile and an open question; eye-off is Hide room; link
  is Add link in the composer; archive is Dismiss all resolved in the Inbox; key-off is Revoke on a machine.

- `web/icons/dice.svg` is the SVG Repo "dice-outline" icon, MIT per the owner. Its geometry is unchanged; the download's XML
  declaration, header comment and fixed size were removed, and black strokes and pips use `currentColor`.

- The close, plus, person-plus, eye, stop, pause, play, arrow-down, arrow-up, arrow-up-down (sort), external-link, switch (arrow-left-right), copy, check,
  reopen (rotate-ccw), more (ellipsis), chevron-right, message (message-square) and chart-column (the Usage of a message) glyphs in `web/icons/` are Lucide icons
  (ISC licence, Copyright (c) 2026 Lucide Icons and Contributors; the licence text is `web/icons/LICENSE-lucide.txt`), release
  `1.59.0`, from `https://github.com/lucide-icons/lucide/tree/1.59.0/icons`. The only change is dropping the fixed
  `width="24" height="24"` and the line breaks; they are drawn through a CSS mask in the current colour.

- `web/icons/health.svg` is the SVG Repo "heart pulse" icon (svgrepo.com/svg/529638), **CC0** (public domain dedication), chosen by the
  owner. The paths are unchanged; the black strokes and fills use `currentColor`, and the download's header is not carried. It is
  drawn through a CSS mask: it is the Health button in the top bar.

- `web/icons/bell.svg` and `web/icons/bell-disabled.svg` are the SVG Repo "bell" and "bell disabled" icons, MIT per the owner. Their
  geometry is unchanged; the downloads' XML declaration, header comment and fixed size were removed, and the black strokes use
  `currentColor`. The bell marks the room's Notifications settings; the disabled bell marks a muted room in the room list.
- `preact/` is the component runtime: Preact **11.0.1** (MIT; `preact.mjs` and `hooks.mjs`) and htm **3.1.1**
  (Apache-2.0; `htm.mjs`), byte-for-byte copies from the npm registry tarballs, loaded through the import map in
  `web/index.html` and imported only by `web/js/ui.js`. The owner approved both on 2026-10-21 (AMB-70). Sources,
  integrity hashes, maintenance notes and how to update are in `preact/README.md`; `preact/SHA256SUMS` lists every file
  (`preact.mjs` is `3a3201e9ccef7d30ef1273427f1b17ef8323e464b2c8d73b4fd5636af730433e`, `hooks.mjs`
  `6cb79fe9959ad185b8b86134aff47c12fd20216b24837597b19be59a133d9947`, `htm.mjs`
  `ab33dd3f38059b9be4d5f5350128eefb2356639c4e0bbe9d9e8b3ba75847e9e4`). The same folder holds @preact/signals **2.11.3**
  and @preact/signals-core **1.14.4** (MIT; `signals.mjs`, `signals-core.mjs`), approved by the owner on 2026-10-22
  (`signals.mjs` is `19c5ca080f54f3b36d1b687555dc82c0c8ce29eaedc0ca42b237012892861841`, `signals-core.mjs`
  `0676238240715faeb4eb8b0a329837dc06ef7f37e7103601e13af3ed309c042c`).

- `sentry/bundle.tracing.min.js` is the Sentry browser SDK v11.11.0 with Tracing (MIT, Copyright (c) 2019 Functional Software, Inc. dba Sentry;
  `sentry/LICENSE` is the licence of the npm package `@sentry/browser@11.11.0`). It is the unmodified CDN bundle
  `https://browser.sentry-cdn.com/11.11.0/bundle.tracing.min.js` (header: "@sentry/browser (Performance Monitoring) 11.11.0 (eadbabb)"). SHA-256:
  `143375024988ceab2b9425d41c1f4c0ea42e3fd8744e76a304890f12066e721b`. The tracing-only bundle is used on purpose: it contains no Session Replay code
  (the `replayIntegration` it exports is a stub that only warns), so Replay cannot be switched on by a setting until its recording can be scrubbed.
  It is loaded by `web/js/telemetry.js` only while the server has reporting on (`AIMEBU2_SENTRY_DSN` or Settings -> General -> Error reports);
  otherwise the page never requests it (docs/telemetry.md). Added with the owner's approval of 2026-11-06 (bus #39710), together with the Go modules
  `sentry-go`, `sentry-go/otel` and the OpenTelemetry API and SDK. To update, download the new version's file from the same URL pattern and `LICENSE`
  from the npm package, replace both, and update the version and the hash here.
