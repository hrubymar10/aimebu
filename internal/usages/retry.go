package usages

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var retryableStatusCodes = map[int]bool{
	http.StatusRequestTimeout:      true,
	http.StatusTooManyRequests:     true,
	http.StatusInternalServerError: true,
	http.StatusBadGateway:          true,
	http.StatusServiceUnavailable:  true,
	http.StatusGatewayTimeout:      true,
}

var retryableMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodOptions: true,
}

type RetryPolicy struct {
	MaxRetries           int
	RetryableStatusCodes []int
	RetryableMethods     []string
	BaseDelay            time.Duration
	MaxDelay             time.Duration
	sleep                func(context.Context, time.Duration) error
}

func doWithRetry(ctx context.Context, client *http.Client, req *http.Request, policy RetryPolicy) (*http.Response, error) {
	policy = policy.withDefaults()
	attempts := policy.MaxRetries + 1
	var lastErr error

	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		resp, err := client.Do(req.Clone(ctx))
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			lastErr = err
			if attempt == attempts-1 || !policy.shouldRetryError(req, attempt, err) {
				return nil, err
			}
			if err := policy.wait(ctx, policy.delay(attempt, nil)); err != nil {
				return nil, err
			}
			continue
		}

		if attempt == attempts-1 || !policy.shouldRetryStatus(req, attempt, resp.StatusCode) {
			return resp, nil
		}
		delay := policy.delay(attempt, resp)
		drainAndClose(resp)
		if err := policy.wait(ctx, delay); err != nil {
			return nil, err
		}
	}

	return nil, lastErr
}

func (p RetryPolicy) withDefaults() RetryPolicy {
	if p.MaxRetries < 0 {
		p.MaxRetries = 0
	} else if p.MaxRetries == 0 {
		p.MaxRetries = 1
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = time.Second
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = 10 * time.Second
	}
	if p.sleep == nil {
		p.sleep = sleepContext
	}
	return p
}

func (p RetryPolicy) shouldRetryError(req *http.Request, attempt int, err error) bool {
	if !p.retryableMethod(req.Method) || attempt >= p.MaxRetries {
		return false
	}
	return isTransientTransportError(err)
}

func (p RetryPolicy) shouldRetryStatus(req *http.Request, attempt int, statusCode int) bool {
	if !p.retryableMethod(req.Method) || attempt >= p.MaxRetries {
		return false
	}
	return p.retryableStatus(statusCode)
}

func (p RetryPolicy) retryableStatus(statusCode int) bool {
	if len(p.RetryableStatusCodes) > 0 {
		for _, code := range p.RetryableStatusCodes {
			if statusCode == code {
				return true
			}
		}
		return false
	}
	return retryableStatusCodes[statusCode]
}

func (p RetryPolicy) retryableMethod(method string) bool {
	method = strings.ToUpper(method)
	if len(p.RetryableMethods) > 0 {
		for _, allowed := range p.RetryableMethods {
			if method == strings.ToUpper(allowed) {
				return true
			}
		}
		return false
	}
	return retryableMethods[method]
}

func (p RetryPolicy) delay(attempt int, resp *http.Response) time.Duration {
	if resp != nil {
		if delay, ok := retryAfterDelay(resp.Header.Get("Retry-After")); ok {
			return capDuration(delay, p.MaxDelay)
		}
	}
	delay := p.BaseDelay
	for i := 0; i < attempt; i++ {
		delay *= 2
		if delay >= p.MaxDelay {
			return p.MaxDelay
		}
	}
	return capDuration(delay, p.MaxDelay)
}

func (p RetryPolicy) wait(ctx context.Context, delay time.Duration) error {
	return p.sleep(ctx, delay)
}

func retryAfterDelay(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			return 0, true
		}
		return time.Duration(seconds) * time.Second, true
	}
	if when, err := http.ParseTime(value); err == nil {
		delay := time.Until(when)
		if delay < 0 {
			delay = 0
		}
		return delay, true
	}
	return 0, false
}

func capDuration(value, max time.Duration) time.Duration {
	if value > max {
		return max
	}
	return value
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}
