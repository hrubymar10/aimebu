// Package eval measures how reliably a model, run through a real harness adapter, produces a correct turn result on its first try.
//
// It has three layers (docs/eval.md):
//
//   - replay: a recorded harness stdout stream is fed through the real adapter's parser and the answer checker. Free and deterministic.
//   - mock: a canned stream with a fault (fenced JSON, prose before the JSON, a missing outcome, a crash) is played by a stand-in harness
//     executable, to test what the adapter and the answer parser make of it. Free and deterministic.
//   - live: the scenarios under testdata/live run on real models. The scoring and the report are here and run on canned results; the
//     runner that spends money is not part of this package yet.
//
// Everything in this package that the normal gate runs is free: no credential, no network, no real harness.
package eval
