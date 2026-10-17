// Package credtarget implements the token-only credential model: the switcher
// profile store holds the only FULL credentials (access + refresh token, regular
// files); every file a CLI actually runs on (the live ~/.claude and ~/.codex
// files, plus any registered external file) is a TARGET, rewritten by the server
// with a token-only document. A target never holds a refresh token, so no CLI can
// spend one, and the server is the single refresher.
//
// Design: __others/design-token-only-copies-server.md (rev 3 + §13). Pieces and
// owners (one commit each, all against this skeleton):
//
//	types.go     credential parsing, the full-vs-target rule (C1)         xavi
//	build.go     BuildClaudeTarget / BuildCodexTarget, read-modify-write  nate
//	write.go     atomic target writer + <configdir>/.storage-write.lock   nate
//	registry.go  target registry (live dirs implicit)                     xavi
//	fanout.go    fan-out of a fresh credential to the bound targets       xavi
//	reconcile.go reconciler: detect a refresh token in a target           otto
//	import.go    /login import target order (C3)                          otto
//	migrate.go   migrate-copies with the C2 guard                         xavi
//
// The refresher lives in internal/usages (it needs the account lock and the
// copy-back CAS) and reaches this package only through usages.TargetFanOut, so
// there is no import cycle (switcher -> usages, credtarget -> switcher/usages).
package credtarget
