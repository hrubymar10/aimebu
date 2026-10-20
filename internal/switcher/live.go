package switcher

// LiveCredentialPath returns the live credentials file the harness reads for
// tool, honouring CLAUDE_CONFIG_DIR and CODEX_HOME. Read-only helpers such as
// `aimebu doctor` use it to compare the live login with a stored profile.
func (m *Manager) LiveCredentialPath(tool string) (string, error) {
	if err := validateTool(tool); err != nil {
		return "", err
	}
	return m.liveCredPath(tool)
}
