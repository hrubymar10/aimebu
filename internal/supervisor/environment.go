package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// EnvironmentFingerprint compares settings that can change a local turn. It
// sends only a digest over the machine socket, never credential values.
func EnvironmentFingerprint(environment []string) string {
	selected := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || !supervisorEnvironmentName(name) {
			continue
		}
		selected = append(selected, entry)
	}
	sort.Strings(selected)
	sum := sha256.Sum256([]byte(strings.Join(selected, "\x00")))
	return hex.EncodeToString(sum[:])
}

func supervisorEnvironmentName(name string) bool {
	switch name {
	case "PATH", "HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "PI_CODING_AGENT_DIR", "PI_CODING_AGENT_SESSION_DIR", "VIBE_HOME", "OPENCODE_DB", "XDG_CONFIG_HOME", "XDG_DATA_HOME":
		return true
	}
	if name == TurnIdleEnv || name == TurnMaxEnv || strings.HasSuffix(name, "_KEY") || strings.HasSuffix(name, "_TOKEN") || strings.HasSuffix(name, "_SECRET") || strings.HasSuffix(name, "_CREDENTIALS") {
		return true
	}
	for _, prefix := range []string{"ANTHROPIC_", "OPENAI_", "MISTRAL_", "OLLAMA_", "AWS_", "AZURE_", "GOOGLE_", "GEMINI_"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
