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

export async function showBuildVersion(request, element) {
  if (!element) return;
  try {
    const label = versionLabel(await request('/api/buildinfo'));
    if (label) { element.textContent = label.text; element.title = label.title; }
  } catch { /* the header keeps its default text */ }
}
