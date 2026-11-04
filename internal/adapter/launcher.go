package adapter

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// LauncherArgs adds owner-configured argv before the adapter's protocol flags.
// No shell parses either set of arguments.
func LauncherArgs(spec TurnSpec, protocolArgs []string) []string {
	return append(append([]string(nil), spec.LauncherArgs...), protocolArgs...)
}

// LauncherEnv resolves references only when starting the process. Turn specs
// journal variable names, never the referenced values.
func LauncherEnv(env []string, refs map[string]string) []string {
	for target, source := range refs {
		env = append(env, target+"="+os.Getenv(source))
	}
	return env
}

// CustomProviderNPM is the only package a custom provider may name: OpenCode npm-installs a provider's package at run time, so any other
// name would run someone else's code on the machine.
const CustomProviderNPM = "@ai-sdk/openai-compatible"

// KnownProviderKeys are the environment variables OpenCode's own built-in providers read their keys from, which is also all a native turn
// lets through of the host's environment apart from the custom providers' own key variables.
var KnownProviderKeys = []string{"OLLAMA_API_KEY", "OPENCODE_API_KEY", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY",
	"GEMINI_API_KEY", "GOOGLE_GENERATIVE_AI_API_KEY", "MISTRAL_API_KEY", "GROQ_API_KEY", "DEEPSEEK_API_KEY", "XAI_API_KEY"}

// ProtectedKeyEnv says whether a variable name is one the harness or its launcher sets or reads for itself, which a custom provider's key
// must never be pointed at: it would let a saved setting redirect the executable search path, the agent's own configuration, or what the
// launcher forwards into the container. So are the keys of OpenCode's own providers (KnownProviderKeys): the host's value of one is a key for
// that provider's endpoint, and a custom provider naming it would send that key to a base URL of someone else's choosing. The server refuses
// such a name when the provider is saved; an adapter checks again, since a turn spec arrives over the wire.
func ProtectedKeyEnv(name string) bool {
	for _, known := range KnownProviderKeys {
		if name == known {
			return true
		}
	}
	switch name {
	case "PATH", "HOME", "USER", "LOGNAME", "SHELL", "TERM", "TMPDIR", "LANG", "PWD", "OLDPWD", "IFS", "ENV", "BASH_ENV",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "NODE_OPTIONS", "NODE_PATH", "NODE_EXTRA_CA_CERTS":
		return true
	}
	for _, prefix := range []string{"LC_", "XDG_", "LD_", "DYLD_", "SSL_", "NPM_", "HARNESS_DOCKER_", "OPENCODE_", "AIMEBU", "DOCKER_"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// EnvValue is what a process started for a turn finds in the variable name: the value of the variable the owner's launcher references
// for it (see LauncherEnv), otherwise the value in this process's own environment. Empty means not set.
func EnvValue(name string, refs map[string]string) string {
	if source, ok := refs[name]; ok {
		return os.Getenv(source)
	}
	return os.Getenv(name)
}

// VisiblePath translates a file beneath the host checkout for a launcher
// whose process sees that checkout at a different absolute path.
func VisiblePath(spec TurnSpec, hostPath string) (string, error) {
	if spec.VisibleCheckoutPath == "" {
		return hostPath, nil
	}
	rel, err := filepath.Rel(spec.CheckoutPath, hostPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("launcher file is outside checkout")
	}
	return filepath.Join(spec.VisibleCheckoutPath, rel), nil
}
