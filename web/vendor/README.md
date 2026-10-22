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
  file): `inbox.svg`, `health.svg` and `spawn.svg` (the owner's "new-indicator" file), and `left.svg` and `right.svg` (copies of
  the icons of the same names in aimebu v1). The project owner confirmed on 2026-10-19 that all five are **MIT** licensed, and
  that is recorded here as their licence. They were cleaned (the download's header comment, title, fixed size and element ids
  removed, fixed colours changed to `currentColor`); their paths are otherwise unchanged.

- Icons that came from an icon catalogue's downloads (SVG Repo) are MIT or another permissive licence, per the owner. No SVG file in
  the repository carries the catalogue's header comment, and `scripts/test-svg-headers.cjs` (in `make test-web`) fails if one does.

- `web/icons/trash.svg` is the SVG Repo "trash can" icon, MIT per the owner. The paths are unchanged; it was adapted to the
  repository's icon format (the download's XML declaration, header comment and fixed 800 px size removed, the black strokes changed
  to `currentColor`), and it is drawn through a CSS mask like the other glyphs.

- `web/icons/pencil.svg` is a project-drawn outline glyph for editing a harness. It uses `currentColor` through the same CSS mask.

- `web/icons/star.svg` and `web/icons/select-all.svg` are the owner's SVG Repo assets (MIT). Their catalogue headers, fixed sizes,
  fixed colours and unused wrappers were removed. The star keeps its outline path for CSS fill on the selected model; select-all is
  drawn through a `currentColor` mask.

- The close, plus, person-plus, eye, stop, pause, play, arrow-down, arrow-up, arrow-up-down (sort), external-link, switch (arrow-left-right), copy, check,
  reopen (rotate-ccw), more (ellipsis), chevron-right and message (message-square) glyphs in `web/icons/` are Lucide icons
  (ISC licence, Copyright (c) 2026 Lucide Icons and Contributors; the licence text is `web/icons/LICENSE-lucide.txt`), release
  `1.59.0`, from `https://github.com/lucide-icons/lucide/tree/1.59.0/icons`. The only change is dropping the fixed
  `width="24" height="24"` and the line breaks; they are drawn through a CSS mask in the current colour.

