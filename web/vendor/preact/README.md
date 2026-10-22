# Preact and htm (vendored)

The UI's component runtime, loaded through the import map in `web/index.html` and imported only by `web/js/ui.js`
(see `docs/design/frontend-preact.md`). No build step: the files are byte-for-byte copies of the published packages.

| File | Package | Version | Licence | Upstream path | gzip -9 |
| --- | --- | --- | --- | --- | --- |
| `preact.mjs` | preact | 11.0.1 | MIT (`LICENSE-preact`) | `dist/preact.mjs` | 4,960 B |
| `hooks.mjs` | preact | 11.0.1 | MIT (`LICENSE-preact`) | `hooks/dist/hooks.mjs` | 1,582 B |
| `htm.mjs` | htm | 3.1.1 | Apache-2.0 (`LICENSE-htm`) | `dist/htm.mjs` (identical to `dist/htm.module.js`) | 646 B |

- Sources: `https://registry.npmjs.org/preact/-/preact-11.0.1.tgz` (integrity
  `sha512-TNNwEq++jYNHy747LziCjPRVgEwnLSOIOAw/oYcjI3Tdnubx2EXtt76I0an8PATvUWtx/DX1kFIg1eFN+O/fkw==`) and
  `https://registry.npmjs.org/htm/-/htm-3.1.1.tgz` (integrity
  `sha512-983Vyg8NwUE7JkZ6NmOqpCZ+sh1bKv2iYTlUkzlWmA5JD2acKoxd4KVxbMmxX/85mtfdnDmTFoNKcg5DGAvxNQ==`); the
  tarballs matched these registry hashes. `LICENSE-preact` and `LICENSE-htm` are each package's `LICENSE`.
- Checksums: `SHA256SUMS` lists every file (`sha256sum -c SHA256SUMS`); `scripts/test-preact-vendor.cjs` (in `make
  test-web`) checks them, the import map and that only `ui.js` imports the runtime.
- Maintenance (2026-10-21): Preact is active; 11.0.0 was released 2026-09-30 and 11.0.1 on 2026-10-08, so this is a
  new major line, pinned here and tested in the page rather than assumed to behave like 10.x. htm has had no release
  since 3.1.1 (2022-04-26): it is small and stable, but quiet. Neither has runtime dependencies.
- `preact.mjs` and `hooks.mjs` end with a `sourceMappingURL` comment; the `.map` files are not shipped, so a browser's
  developer tools report them missing. That is expected.

To update, in a throwaway directory (replace the versions):

```sh
npm pack preact@11.0.1 htm@3.1.1
mkdir preact htm && tar xzf preact-11.0.1.tgz -C preact && tar xzf htm-3.1.1.tgz -C htm
cp preact/package/dist/preact.mjs <repo>/web/vendor/preact/preact.mjs
cp preact/package/hooks/dist/hooks.mjs <repo>/web/vendor/preact/hooks.mjs
cp htm/package/dist/htm.mjs <repo>/web/vendor/preact/htm.mjs
cp preact/package/LICENSE <repo>/web/vendor/preact/LICENSE-preact
cp htm/package/LICENSE <repo>/web/vendor/preact/LICENSE-htm
cd <repo>/web/vendor/preact && sha256sum preact.mjs hooks.mjs htm.mjs LICENSE-preact LICENSE-htm > SHA256SUMS
```

Compare the tarballs with `npm view <package>@<version> dist.integrity` first. Then update this file (versions, sizes,
integrity, dates) and the entry in `web/vendor/README.md`, and run `make test-web` and `make test-ui`. Updating either
package is a dependency change and needs the owner's approval (`AGENTS.md`).
