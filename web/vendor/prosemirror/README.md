# ProseMirror (vendored)

`prosemirror.js` is one ES module with the ProseMirror pieces the composer's editor option
uses (`web/js/composer-editor.js`). It is built from the exports listed in `entry.js`.

| Package | Version | License |
| --- | --- | --- |
| prosemirror-model | 1.25.12 | MIT |
| prosemirror-state | 1.4.4 | MIT |
| prosemirror-view | 1.42.6 | MIT |
| prosemirror-transform | 1.12.2 | MIT |
| prosemirror-keymap | 1.2.3 | MIT |
| prosemirror-commands | 1.7.2 | MIT |
| prosemirror-history | 1.5.1 | MIT |
| prosemirror-inputrules | 1.5.1 | MIT |
| prosemirror-schema-list | 1.5.1 | MIT |
| orderedmap | 2.1.1 | MIT |
| w3c-keyname | 2.2.8 | MIT |
| rope-sequence | 1.3.4 | MIT |

Source: <https://prosemirror.net/> (packages from the npm registry). `LICENSE` holds the licence text of
every package above. `prosemirror-markdown` is deliberately not included (it pulls in markdown-it);
`web/js/composer-markdown.js` serialises the document to Markdown instead.

- Size: about 211 KB minified, 67 KB gzip.
- SHA-256 of `prosemirror.js`: `a0b4ee1520ef675eaf0dec1fc7685108119538d9d53d0146f8494c98b095b5bb`.

## Rebuilding or updating

esbuild is only used to build this file; it is not a dependency of the repository. In a throwaway directory:

```sh
mkdir pm && cd pm && npm init -y
npm install --ignore-scripts prosemirror-model prosemirror-state prosemirror-view prosemirror-transform \
  prosemirror-keymap prosemirror-commands prosemirror-history prosemirror-inputrules prosemirror-schema-list esbuild
cp <repo>/web/vendor/prosemirror/entry.js .
./node_modules/.bin/esbuild entry.js --bundle --format=esm --minify --legal-comments=none --outfile=prosemirror.js
sha256sum prosemirror.js
```

Copy `prosemirror.js` back, refresh the versions, SHA-256 and `LICENSE` here, and run `make test-ui`.
Pin versions with `npm install <package>@<version>` to reproduce a specific build.
