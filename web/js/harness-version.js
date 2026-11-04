// The version line of a harness card. A container launcher that could not say its version carries why (the folder it was asked from is
// not mounted, the container is stopped, no answer in time), and that reason is shown, so "Version unknown" is not a dead end.
export const versionLabel = harness => {
  if (harness.version) return `Version ${harness.version}`;
  return harness.version_problem ? `Version unknown (${harness.version_problem})` : 'Version unknown';
};

// The version part of a detected harness's row in the harness list ("Found on this machine"): the version when it is known; for a container
// launcher that could not say, why (shown, not left blank); nothing for a harness that simply printed none.
export const candidateVersionLabel = candidate => {
  if (candidate.version) return ` · ${candidate.version}`;
  return candidate.version_problem ? ` · version unknown (${candidate.version_problem})` : '';
};

// The name a harness goes by where v1 and bus_register name it: Claude Code is "claude-code" there, while "claude" is only the tool key of v2's
// API. What the web shows of an agent's harness uses this name, so the same agent reads the same in both.
export const harnessName = tool => (tool === 'claude' ? 'claude-code' : tool || '');
