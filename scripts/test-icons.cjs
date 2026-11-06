// The installable-app icons are the official tiled icon (assets/dark.png) at the sizes the manifest and Safari's Add to Dock use: the right
// dimensions, the touch icon opaque to its corners (macOS and iOS round it themselves), the "any" icons keeping the tile's own transparent
// corners, the maskable one opaque, and the manifest pointing at them with the tile's colour as its background. The files are what
// scripts/build-icons.cjs writes. Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const root = path.join(__dirname, '..', 'web');
const manifest = JSON.parse(fs.readFileSync(path.join(root, 'manifest.webmanifest'), 'utf8'));
const TILE = '#0f1117';

(async () => {
  const browser = await puppeteer.launch({ executablePath: chromiumPath(), headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'] });
  try {
    const page = await browser.newPage();
    const read = async file => page.evaluate(async data => {
      const img = new Image(); img.src = data; await img.decode();
      const canvas = document.createElement('canvas'); canvas.width = img.width; canvas.height = img.height;
      const context = canvas.getContext('2d'); context.drawImage(img, 0, 0);
      const at = (x, y) => Array.from(context.getImageData(x, y, 1, 1).data);
      const w = img.width, h = img.height;
      return { w, h, corners: [at(0, 0), at(w - 1, 0), at(0, h - 1), at(w - 1, h - 1)].map(pixel => pixel[3]), tile: at(Math.round(w / 2), Math.round(h * 0.06)) };
    }, 'data:image/png;base64,' + fs.readFileSync(path.join(root, 'icons', file)).toString('base64'));
    const isTile = pixel => [0x0f, 0x11, 0x17].every((value, index) => Math.abs(pixel[index] - value) <= 3);
    const touch = await read('apple-touch-icon.png');
    assert.deepEqual([touch.w, touch.h, touch.corners], [180, 180, [255, 255, 255, 255]], 'the touch icon is 180px and opaque to its corners');
    assert.ok(isTile(touch.tile), `and it is the dark tile: ${touch.tile}`);
    for (const [file, size] of [['icon-192.png', 192], ['icon-512.png', 512]]) {
      const icon = await read(file);
      assert.deepEqual([icon.w, icon.h], [size, size], `${file} is ${size}px`);
      assert.ok(icon.corners.every(alpha => alpha === 0), `${file} keeps the tile's rounded, transparent corners`);
      assert.ok(isTile(icon.tile), `${file} is the dark tile: ${icon.tile}`);
    }
    const maskable = await read('icon-maskable-512.png');
    assert.deepEqual([maskable.w, maskable.h, maskable.corners], [512, 512, [255, 255, 255, 255]], 'the maskable icon is opaque');
    assert.equal(manifest.background_color.toLowerCase(), TILE, 'the manifest background is the tile colour');
    const icons = Object.fromEntries(manifest.icons.map(icon => [`${icon.sizes}:${icon.purpose}`, icon.src]));
    assert.deepEqual(icons, { '192x192:any': '/icons/icon-192.png', '512x512:any': '/icons/icon-512.png', '512x512:maskable': '/icons/icon-maskable-512.png' });
    console.log('icons test passed');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exit(1); });
