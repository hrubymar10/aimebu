package supervisor

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
)

// freeMemory is what the system can give a new process, and all it has, in bytes (MemAvailable of /proc/meminfo).
func freeMemory() (free, total int64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	var haveFree, haveTotal bool
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		kb, convErr := strconv.ParseInt(fields[1], 10, 64)
		if convErr != nil {
			continue
		}
		switch fields[0] {
		case "MemAvailable:":
			free, haveFree = kb*1024, true
		case "MemTotal:":
			total, haveTotal = kb*1024, true
		}
	}
	if !haveFree || !haveTotal {
		return 0, 0, errors.New("memory figures not found")
	}
	return free, total, nil
}
