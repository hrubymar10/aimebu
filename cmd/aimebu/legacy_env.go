package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
)

var legacyEnvironmentWarning sync.Once

// warnLegacyEnvironment reports obsolete names without reading or printing
// their values. They have no effect on configuration.
func warnLegacyEnvironment(stderr io.Writer) {
	warnLegacyEnvironmentOnce(os.Environ(), stderr, &legacyEnvironmentWarning)
}

func warnLegacyEnvironmentOnce(environ []string, stderr io.Writer, once *sync.Once) {
	var names []string
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "AIMEBU2_") {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return
	}
	sort.Strings(names)
	once.Do(func() {
		fmt.Fprintf(stderr, "aimebu: ignoring AIMEBU2_* environment variables (renamed to AIMEBU_*): %s\n", strings.Join(names, ", "))
	})
}
