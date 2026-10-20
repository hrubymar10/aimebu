package providers

import (
	"context"
	"errors"
	"net"
	"syscall"
)

func orderWindows(windows []Window, keys []string) []Window {
	ordered := make([]Window, 0, len(windows))
	for _, key := range keys {
		for _, window := range windows {
			if window.Key == key {
				ordered = append(ordered, window)
			}
		}
	}
	return ordered
}

func isTransientTransportError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	for _, target := range []error{syscall.ECONNABORTED, syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EHOSTDOWN, syscall.EHOSTUNREACH, syscall.ENETDOWN, syscall.ENETUNREACH} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
