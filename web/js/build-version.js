// The header shows the version the server was built as (`git describe` of the checkout, or what a release was stamped with), as v1 does,
// read from GET /api/buildinfo after sign-in. Until then, and when it cannot be read, the page keeps the text it was served with.

// What the header says: the version without the program name it is stamped with ("aimebu v2.0.0-3-gabc123" → "v2.0.0-3-gabc123"), and the
// Go toolchain as the tooltip. Nothing (null) when the answer has no version to show.
export function versionLabel(info) {
  const version = typeof info?.version === 'string' ? info.version.trim().replace(/^aimebu(?:\s+|$)/i, '') : '';
  if (!version) return null;
  const go = typeof info.go_version === 'string' && info.go_version ? info.go_version : '';
  return { text: version, title: [version, go].filter(Boolean).join(' · ') };
}

// Resolves to the version string as the server states it ('' when it could not be read), for the caller that wants to know which build served it.
export async function showBuildVersion(request, element, onInfo) {
  try {
    const info = await request('/api/buildinfo');
    const label = versionLabel(info);
    if (element && label) { element.textContent = label.text; element.title = label.title; }
    try { onInfo?.(info); } catch { /* a listener failing never costs the header */ }
    return typeof info?.version === 'string' ? info.version : '';
  } catch { /* the header keeps its default text */ return ''; }
}
