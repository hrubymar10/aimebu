package usages

import (
	"context"
	"errors"
	"net/http"
	"time"
)

const usageHTTPTimeout = 10 * time.Second

var usageHTTPClient = newUsageHTTPClient()

func newUsageHTTPClient() *http.Client {
	return &http.Client{Timeout: usageHTTPTimeout}
}

func usageRequestStatus(err error) Status {
	if errors.Is(err, context.DeadlineExceeded) {
		return StatusTimeout
	}
	return StatusFetchError
}
