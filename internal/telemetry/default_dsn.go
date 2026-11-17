package telemetry

// The maintainers' Sentry project, used when nobody chose a DSN. It is the only place a default lives, in its own file, so that deleting this
// file (or reverting its commit) leaves a build that is off until a DSN comes from AIMEBU_SENTRY_DSN or Settings -> Telemetry.
// One project serves both the Go server and the browser; it is the EU ingest host.
func init() {
	DefaultDSN = "https://f6d48ca3b920b6b2cb3cb0d719305c81@o4512387147563008.ingest.de.sentry.io/4512387159425104"
}
