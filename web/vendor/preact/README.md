# Preact, htm and signals (vendored)

The UI's component runtime, loaded through the import map in `web/index.html` and imported only by `web/js/ui.js`
(see `docs/design/frontend-preact.md`). No build step: the files are byte-for-byte copies of the published packages.

| File | Package | Version | Licence | Upstream path | gzip -9 |
| --- | --- | --- | --- | --- | --- |
| `preact.mjs` | preact | 11.0.1 | MIT (`LICENSE-preact`) | `dist/preact.mjs` | 4,960 B |
| `hooks.mjs` | preact | 11.0.1 | MIT (`LICENSE-preact`) | `hooks/dist/hooks.mjs` | 1,582 B |
| `htm.mjs` | htm | 3.1.1 | Apache-2.0 (`LICENSE-htm`) | `dist/htm.mjs` (identical to `dist/htm.module.js`) | 646 B |
| `signals.mjs` | @preact/signals | 2.11.3 | MIT (`LICENSE-signals`) | `dist/signals.mjs` | 1,771 B |
| `signals-core.mjs` | @preact/signals-core | 1.14.4 | MIT (`LICENSE-signals-core`) | `dist/signals-core.mjs` | 1,935 B |

- Sources: `https://registry.npmjs.org/preact/-/preact-11.0.1.tgz` (integrity
  `sha512-TNNwEq++jYNHy747LziCjPRVgEwnLSOIOAw/oYcjI3Tdnubx2EXtt76I0an8PATvUWtx/DX1kFIg1eFN+O/fkw==`) and
  `https://registry.npmjs.org/htm/-/htm-3.1.1.tgz` (integrity
  `sha512-983Vyg8NwUE7JkZ6NmOqpCZ+sh1bKv2iYTlUkzlWmA5JD2acKoxd4KVxbMmxX/85mtfdnDmTFoNKcg5DGAvxNQ==`); the
  tarballs matched these registry hashes. `LICENSE-preact` and `LICENSE-htm` are each package's `LICENSE`.
- Signals sources: `https://registry.npmjs.org/@preact/signals/-/signals-2.11.3.tgz` (integrity
  `sha512-mttwFM98G/JiriTgMwU+UWygL6sfVmYEiNcPSKkUnv4PxPpZhmP2ugGC4qi360+ZIHyQ8NY05bfJT3b4RUIGlw==`) and
  `https://registry.npmjs.org/@preact/signals-core/-/signals-core-1.14.4.tgz` (integrity
  `sha512-HNB6HYeYKhQbJ1aKl+YRjrS4+QWHLKX6qKoUsfS/m0vqzsVaEBiZiaKbG/e+NKk2ch5ALQr/ihWaMHxiCuuWHA==`); the tarballs matched
  these registry hashes. `LICENSE-signals` and `LICENSE-signals-core` are each package's `LICENSE` (the same text).
  `signals.mjs` imports `preact`, `preact/hooks` and `@preact/signals-core`, all resolved by the import map, so it uses the
  one copy of Preact. @preact/signals 2.11.3 was published 2026-09-30 and supports Preact 11 (peer `>=11.0.0-0`);
  @preact/signals-core 1.14.4 on 2026-07-07.
- Checksums: `SHA256SUMS` lists every file (`sha256sum -c SHA256SUMS`); `scripts/test-preact-vendor.cjs` (in `make
  test-web`) checks them, the import map and that only `ui.js` imports the runtime.
- Maintenance (2026-10-21): Preact is active; 11.0.0 was released 2026-09-30 and 11.0.1 on 2026-10-08, so this is a
  new major line, pinned here and tested in the page rather than assumed to behave like 10.x. htm has had no release
  since 3.1.1 (2022-04-26): it is small and stable, but quiet. Neither has runtime dependencies.
- `preact.mjs`, `hooks.mjs`, `signals.mjs` and `signals-core.mjs` end with a `sourceMappingURL` comment; the `.map` files are not shipped, so a browser's
  developer tools report them missing. That is expected.

To update, in a throwaway directory (replace the versions):

```sh
npm pack preact@11.0.1 htm@3.1.1 @preact/signals@2.11.3 @preact/signals-core@1.14.4
mkdir preact htm signals core && tar xzf preact-11.0.1.tgz -C preact && tar xzf htm-3.1.1.tgz -C htm
tar xzf preact-signals-2.11.3.tgz -C signals && tar xzf preact-signals-core-1.14.4.tgz -C core
cp preact/package/dist/preact.mjs <repo>/web/vendor/preact/preact.mjs
cp preact/package/hooks/dist/hooks.mjs <repo>/web/vendor/preact/hooks.mjs
cp htm/package/dist/htm.mjs <repo>/web/vendor/preact/htm.mjs
cp preact/package/LICENSE <repo>/web/vendor/preact/LICENSE-preact
cp htm/package/LICENSE <repo>/web/vendor/preact/LICENSE-htm
cp signals/package/dist/signals.mjs core/package/dist/signals-core.mjs <repo>/web/vendor/preact/
cp signals/package/LICENSE <repo>/web/vendor/preact/LICENSE-signals
cp core/package/LICENSE <repo>/web/vendor/preact/LICENSE-signals-core
cd <repo>/web/vendor/preact && sha256sum preact.mjs hooks.mjs htm.mjs signals.mjs signals-core.mjs \
  LICENSE-preact LICENSE-htm LICENSE-signals LICENSE-signals-core > SHA256SUMS
```

Compare the tarballs with `npm view <package>@<version> dist.integrity` first. Then update this file (versions, sizes,
integrity, dates) and the entry in `web/vendor/README.md`, and run `make test-web` and `make test-ui`. Updating any
package is a dependency change and needs the owner's approval (`AGENTS.md`).
