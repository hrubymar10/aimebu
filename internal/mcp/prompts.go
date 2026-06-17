package mcp

// BuiltinPromptDefaults returns the compiled-in default body for every
// configurable prompt key owned by the mcp package (bus_etiquette,
// error.not_registered, and the 13 tool.* descriptions). The agent.*
// spawn-prompt defaults live in cmd/aimebu/agent.go:AgentBuiltinSpawnDefaults.
// This map is passed to server.SetPromptDefaults at startup so the HTTP server
// can serve and diff against these values.
func BuiltinPromptDefaults() map[string]string {
	m := make(map[string]string, len(tools)+2)
	m["bus_etiquette"] = busEtiquette
	m["error.not_registered"] = notRegisteredDefault
	for _, t := range tools {
		m["tool."+t.Name] = t.Description
	}
	return m
}
