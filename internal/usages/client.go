package usages

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

const usageHTTPTimeout = 10 * time.Second

var usageHTTPClient = newUsageHTTPClient()

func newUsageHTTPClient() *http.Client {
	return &http.Client{
		Timeout:       usageHTTPTimeout,
		CheckRedirect: stripCrossOriginUsageCredentials,
	}
}

func stripCrossOriginUsageCredentials(req *http.Request, via []*http.Request) error {
	if len(via) == 0 || sameUsageOrigin(req, via[len(via)-1]) {
		return nil
	}
	for name := range req.Header {
		if usageCredentialHeader(name) {
			req.Header.Del(name)
		}
	}
	return nil
}

func sameUsageOrigin(a, b *http.Request) bool {
	if a == nil || b == nil || a.URL == nil || b.URL == nil {
		return false
	}
	return strings.EqualFold(a.URL.Scheme, b.URL.Scheme) && strings.EqualFold(a.URL.Host, b.URL.Host)
}

func usageCredentialHeader(name string) bool {
	lower := strings.ToLower(name)
	switch lower {
	case "authorization", "proxy-authorization", "cookie", "chatgpt-account-id":
		return true
	}
	if strings.Contains(lower, "api-key") || strings.Contains(lower, "apikey") {
		return true
	}
	if strings.HasPrefix(lower, "x-") {
		return strings.Contains(lower, "auth") ||
			strings.Contains(lower, "token") ||
			strings.Contains(lower, "secret")
	}
	return false
}

func usageRequestStatus(err error) Status {
	if errors.Is(err, context.DeadlineExceeded) {
		return StatusTimeout
	}
	return StatusFetchError
}
