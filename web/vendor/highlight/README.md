highlight.js, vendored for code highlighting in rendered messages (`web/js/code-highlight.js`).

- Version: highlight.js **11.12.0** (BSD-3-Clause, see `LICENSE`).
- Source: the project's browser build, `https://github.com/highlightjs/cdn-release`, tag `11.12.0`
  (commit `dce7a3dab8f3fd586138ba6c5f29ecc19c02db9f`), files under `build/es/`:
  `core.min.js` is `core.js` here, `languages/<name>.min.js` is `languages/<name>.js`, and `build/LICENSE`
  is `LICENSE`. They are byte-for-byte copies; nothing is rebuilt or edited.
- Checksums: `SHA256SUMS` lists every file (`sha256sum -c SHA256SUMS`). `core.js` is
  `67a339aa68880a40c9803def9c8cebb2225dec76230382db86e349370dee70e9`.
- What is here: the core and 27 grammars (no `diff`: the page renders diffs itself), each its own ES module. The page imports the core and one grammar
  on demand, only when a rendered message has a fenced block whose language it knows, so a chat without code
  blocks loads none of it. The language names and aliases the page accepts are listed in
  `web/js/code-highlight.js` and checked against the grammars' own aliases by `scripts/test-highlight.cjs`.
- Theme: none. highlight.js ships stylesheets with fixed colours; they are not used. The `hljs-*` token
  classes are coloured from the app's theme variables in `web/theme-v1.css`.

To update, from this directory (replace `11.12.0` with the new tag and keep the language list in sync with
`LANGUAGES` in `web/js/code-highlight.js`):

```sh
V=11.12.0; B=https://raw.githubusercontent.com/highlightjs/cdn-release/$V/build
curl -fL $B/es/core.min.js -o core.js; curl -fL $B/LICENSE -o LICENSE
for l in $(ls languages | sed 's/\.js$//'); do curl -fL $B/es/languages/$l.min.js -o languages/$l.js; done
sha256sum core.js LICENSE languages/*.js > SHA256SUMS
```

Then update the version, commit and `core.js` hash above, and run `make test-web` and `make test-ui`.
