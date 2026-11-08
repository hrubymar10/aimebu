package supervisor

import (
	"errors"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

var vmStatPages = regexp.MustCompile(`(?m)^Pages (free|inactive|speculative|purgeable):\s+(\d+)\.`)
var vmStatPageSize = regexp.MustCompile(`page size of (\d+) bytes`)

// freeMemory is what the system can give a new process, and all it has, in bytes: free, inactive, speculative and purgeable pages of vm_stat.
func freeMemory() (free, total int64, err error) {
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0, 0, err
	}
	total, err = strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, 0, err
	}
	stat, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0, 0, err
	}
	size := int64(4096)
	if m := vmStatPageSize.FindSubmatch(stat); m != nil {
		if n, convErr := strconv.ParseInt(string(m[1]), 10, 64); convErr == nil {
			size = n
		}
	}
	var pages int64
	matched := false
	for _, m := range vmStatPages.FindAllSubmatch(stat, -1) {
		n, convErr := strconv.ParseInt(string(m[2]), 10, 64)
		if convErr == nil {
			pages += n
			matched = true
		}
	}
	if !matched {
		return 0, 0, errors.New("vm_stat figures not found")
	}
	return pages * size, total, nil
}
