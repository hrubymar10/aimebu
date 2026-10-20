// Package credtarget implements the token-only credential model: the switcher
// profile store holds the only FULL credentials (access + refresh token, regular
// files); every file a CLI actually runs on (the live ~/.claude and ~/.codex
// files, plus any registered external file) is a TARGET, rewritten by the server
// with a token-only document. A target never holds a refresh token, so no CLI can
// spend one, and the server is the single refresher.
//
// Files:
//
//	types.go     credential parsing, the full-vs-target rule
//	build.go     BuildClaudeTarget / BuildCodexTarget, read-modify-write
//	write.go     atomic target writer + <configdir>/.storage-write.lock
//	registry.go  target registry (live dirs implicit)
//	fanout.go    fan-out of a fresh credential to the bound targets
//	reconcile.go reconciler: detect a refresh token in a target
//	import.go    /login import target order
//	migrate.go   migrate-copies
//
// The refresher lives in internal/usages (it needs the account lock and the
// copy-back CAS) and reaches this package only through usages.TargetFanOut, so
// there is no import cycle (switcher -> usages, credtarget -> switcher/usages).
package credtarget
