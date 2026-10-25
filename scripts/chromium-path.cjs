const fs = require('node:fs');

function chromiumPath() {
  const override = process.env.CHROMIUM_PATH;
  const candidates = override ? [override] : process.platform === 'darwin'
    ? ['/Applications/Chromium.app/Contents/MacOS/Chromium', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome']
    : ['/usr/bin/chromium', '/usr/bin/chromium-browser', '/usr/bin/google-chrome'];
  for (const candidate of candidates) {
    try { fs.accessSync(candidate, fs.constants.X_OK); return candidate; } catch {}
  }
  throw new Error(`Chromium executable not found; set CHROMIUM_PATH to an executable browser path${override ? ` (tried ${override})` : ''}`);
}

module.exports = { chromiumPath };
