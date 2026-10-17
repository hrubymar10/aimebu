package usages

// C1: v1's loaders assume every credential is a FULL one. After migration the
// live files are token-only targets, so the rule is split:
//
//	full   (profile store)       access token AND refresh token required
//	target (live/external file)  access token (codex: + account id) only; a
//	                              refresh token, if present, is not required and
//	                              is not what the usage fetch needs
//
// Code to change (owner: xavi):
//   - codex.go:161  parseCodexAuthData returns "login required" when refresh_token
//     is empty: take a mode, targets must not require it.
//   - codex.go:47   reloadCodexAuthIfChanged only runs when a refresh token exists:
//     run it regardless.
//   - claude.go     loadClaudeAuth must not require refreshToken for targets.
//   - switcher/switch.go:364 validateStoredCred: store files use full, live files
//     use target.
type credentialMode int

const (
	credentialModeFull credentialMode = iota
	credentialModeTarget
)
