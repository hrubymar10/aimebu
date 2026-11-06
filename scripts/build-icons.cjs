// Builds the raster icons with headless Chromium (no extra dependencies).
//   From the 1024 px masters in assets/{light,dark}.png (Icon Composer exports), downscaled in steps with
//   high-quality smoothing:
//     web/icons/apple-touch-icon.png  180 px, the dark tiled icon with the tile's colour filling its corners (opaque: iOS and macOS apply their own mask)
//     web/icons/icon-192.png, icon-512.png  the dark tiled icon, with its own rounded transparent corners, for the web app manifest (purpose any)
//     web/icons/icon-maskable-512.png  the same at 80% on a square of the tile's colour, so the mark stays in the safe zone a platform mask leaves
//     web/icons/icon-512-dark.png  the dark icon, for the first-run welcome on a dark theme
//     web/icons/logo-light.png, logo-dark.png  50 px (2x of the 25 px header logo)
//   From the tiled favicon (web/icons/favicon.svg: the small mark on the dark squircle tile; the SVG itself switches to a light tile
//   under prefers-color-scheme, an ICO cannot, so the ICO uses the dark tile; the masters are illegible at 16/32 px):
//     web/favicon.ico  16 px and 32 px, stored as PNG-in-ICO
// The large icon sources (SVG and the Icon Composer bundles) live in assets/. Usage: node scripts/build-icons.cjs
'use strict';
const fs = require('node:fs');
const path = require('node:path');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const icons = path.join(__dirname, '..', 'web', 'icons');
const TILE = '#0f1117'; // the dark tile's colour, sampled from assets/dark.png (also the manifest's background_color)
const uri = name => `data:image/svg+xml;base64,${fs.readFileSync(path.join(icons, name)).toString('base64')}`;

async function render(browser, svg, size, background) {
  const page = await browser.newPage();
  await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: 'dark' }]); // the ICO takes the dark tile
  await page.setViewport({ width: size, height: size, deviceScaleFactor: 1 });
  await page.setContent(`<body style="margin:0;background:${background || 'transparent'}"><img src="${uri(svg)}" width="${size}" height="${size}" style="display:block"></body>`);
  await page.evaluate(() => document.querySelector('img').decode());
  const png = await page.screenshot({ type: 'png', omitBackground: !background, clip: { x: 0, y: 0, width: size, height: size } });
  await page.close();
  return png;
}

// Downscales a PNG master by halving until the next step would undershoot, then one final draw, all with high-quality smoothing.
async function downscale(browser, master, size, background) {
  const page = await browser.newPage();
  const data = fs.readFileSync(path.join(__dirname, '..', 'assets', master)).toString('base64');
  const out = await page.evaluate(async (data, size, background) => {
    const image = new Image(); image.src = `data:image/png;base64,${data}`; await image.decode();
    let source = image, width = image.naturalWidth;
    const draw = (from, target) => {
      const canvas = document.createElement('canvas'); canvas.width = canvas.height = target;
      const context = canvas.getContext('2d'); context.imageSmoothingEnabled = true; context.imageSmoothingQuality = 'high';
      if (background) { context.fillStyle = background; context.fillRect(0, 0, target, target); }
      context.drawImage(from, 0, 0, target, target);
      return canvas;
    };
    while (width / 2 >= size) { width /= 2; source = draw(source, width); }
    return draw(source, size).toDataURL('image/png').split(',')[1];
  }, data, size, background || '');
  await page.close();
  return Buffer.from(out, 'base64');
}

// The master at a fraction of the size, centred on a square of the background colour (the maskable icon's safe zone).
async function maskable(browser, master, size, scale, background) {
  const inner = await downscale(browser, master, Math.round(size * scale));
  const page = await browser.newPage();
  const out = await page.evaluate(async (data, size, background) => {
    const image = new Image(); image.src = `data:image/png;base64,${data}`; await image.decode();
    const canvas = document.createElement('canvas'); canvas.width = canvas.height = size;
    const context = canvas.getContext('2d'); context.fillStyle = background; context.fillRect(0, 0, size, size);
    context.drawImage(image, (size - image.naturalWidth) / 2, (size - image.naturalHeight) / 2);
    return canvas.toDataURL('image/png').split(',')[1];
  }, inner.toString('base64'), size, background);
  await page.close();
  return Buffer.from(out, 'base64');
}

// ICO with PNG payloads (supported by every current browser): header, one directory entry per image, then the images.
function ico(images) {
  const header = Buffer.alloc(6); header.writeUInt16LE(1, 2); header.writeUInt16LE(images.length, 4);
  const entries = []; let offset = 6 + 16 * images.length;
  for (const { size, png } of images) {
    const entry = Buffer.alloc(16);
    entry.writeUInt8(size, 0); entry.writeUInt8(size, 1); entry.writeUInt16LE(1, 4); entry.writeUInt16LE(32, 6);
    entry.writeUInt32LE(png.length, 8); entry.writeUInt32LE(offset, 12);
    entries.push(entry); offset += png.length;
  }
  return Buffer.concat([header, ...entries, ...images.map(image => image.png)]);
}

(async () => {
  const browser = await puppeteer.launch({ executablePath: chromiumPath(), headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'] });
  try {
    fs.writeFileSync(path.join(icons, 'apple-touch-icon.png'), await downscale(browser, 'dark.png', 180, TILE));
    for (const size of [192, 512]) fs.writeFileSync(path.join(icons, `icon-${size}.png`), await downscale(browser, 'dark.png', size));
    fs.writeFileSync(path.join(icons, 'icon-maskable-512.png'), await maskable(browser, 'dark.png', 512, 0.8, TILE));
    fs.writeFileSync(path.join(icons, 'icon-512-dark.png'), await downscale(browser, 'dark.png', 512));
    for (const theme of ['light', 'dark']) fs.writeFileSync(path.join(icons, `logo-${theme}.png`), await downscale(browser, `${theme}.png`, 50));
    fs.writeFileSync(path.join(__dirname, '..', 'web', 'favicon.ico'), ico([
      { size: 16, png: await render(browser, 'favicon.svg', 16) },
      { size: 32, png: await render(browser, 'favicon.svg', 32) },
    ]));
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exit(1); });
