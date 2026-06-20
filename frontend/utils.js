// aimebu – stateless DOM/string utilities shared across frontend modules.
// Loaded before app.js so these names resolve as globals inside the IIFE.

function esc(str) {
  var div = document.createElement('div');
  div.textContent = str || '';
  return div.innerHTML;
}

function escAttr(str) {
  return esc(str);
}

function unescHtml(str) {
  var div = document.createElement('div');
  div.innerHTML = str || '';
  return div.textContent || '';
}

var COPY_CODE_ICON = '<svg class="md-copy-icon" width="14" height="14" viewBox="0 0 24 24" fill="none" aria-hidden="true"><path d="M14 7c0-.9319 0-1.3978-.1522-1.7654-.203-.4901-.5924-.8794-1.0824-1.0824C12.3978 4 11.9319 4 11 4H8c-1.8856 0-2.8284 0-3.4142.5858C4 5.1716 4 6.1144 4 8v3c0 .9319 0 1.3978.1522 1.7654.203.49.5924.8794 1.0824 1.0824C5.6022 14 6.0681 14 7 14" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"></path><rect x="10" y="10" width="10" height="10" rx="2" stroke="currentColor" stroke-width="1.7"></rect></svg>';
var CHECK_CODE_ICON = '<svg class="md-copy-icon md-copy-check" width="14" height="14" viewBox="0 0 24 24" fill="none" aria-hidden="true"><path d="M20 6 9 17l-5-5" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"></path></svg>';

function escRe(s) {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}
