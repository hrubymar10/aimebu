package supervisor

import (
	"errors"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var dockerUsage = regexp.MustCompile(`^\s*([\d.]+)\s*([KMGT]?i?B)\b`)

// dockerMemory is what the machine Docker runs on has and the containers use, in bytes: the total memory of the Docker host (the VM, on a
// Mac) from `docker info`, and the sum of what every running container uses from `docker stats`. A warm Docker process's memory is the
// container's, so the supervisor's own figures (the host's) do not show it.
func dockerMemory() (used, total int64, err error) {
	out, err := dockerOutput("info", "--format", "{{.MemTotal}}")
	if err != nil {
		return 0, 0, err
	}
	total, err = strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil || total <= 0 {
		return 0, 0, errors.New("docker did not report its memory")
	}
	stats, err := dockerOutput("stats", "--no-stream", "--format", "{{.MemUsage}}")
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(stats, "\n") {
		if bytes, ok := parseDockerSize(strings.SplitN(line, "/", 2)[0]); ok {
			used += bytes
		}
	}
	return used, total, nil
}

func dockerOutput(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.Output(); close(done) }()
	select {
	case <-done:
		return string(out), err
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		return "", errors.New("docker did not answer in time")
	}
}

// parseDockerSize reads a size as `docker stats` prints it ("123.4MiB", "1.2GiB", "980kB").
func parseDockerSize(text string) (int64, bool) {
	m := dockerUsage.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return 0, false
	}
	value, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	unit := map[string]float64{"B": 1, "KB": 1e3, "KIB": 1 << 10, "MB": 1e6, "MIB": 1 << 20, "GB": 1e9, "GIB": 1 << 30, "TB": 1e12, "TIB": 1 << 40}[strings.ToUpper(m[2])]
	if unit == 0 {
		return 0, false
	}
	return int64(value * unit), true
}
