package quota

// Report is where the package says that something failed or fell back (a switch that failed, a state that could not be saved, a guard configuration that is
// unreadable). `cause` is a fixed phrase: the error itself is not passed, since it can name an account or a profile, which must not leave the machine. The package
// imports nothing of the program, so it cannot report to Sentry itself: the server sets this to do so. It is set once, before any runner starts; nil (the
// default, and the tests') drops the report. `warning` is true for a degradation the package recovers from by itself.
var Report func(cause string, warning bool)

func report(cause string, warning bool) {
	if Report != nil && cause != "" {
		Report(cause, warning)
	}
}
