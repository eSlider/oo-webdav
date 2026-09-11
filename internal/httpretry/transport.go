// Package httpretry provides an http.RoundTripper that transparently retries
// requests the ONLYOFFICE portal temporarily refuses: 429 (rate limit),
// 502/503/504 (upstream busy) and 500 whose body reports a rolled-back InnoDB
// deadlock. Retries use exponential backoff with jitter and honour Retry-After.
//
// oo-webdav installs this as http.DefaultClient's transport; the go-onlyoffice
// client captures http.DefaultClient, so every portal API call gets it.
package httpretry

import (
	"bytes"
	"context"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Transport wraps a base RoundTripper and retries retryable responses/errors.
type Transport struct {
	Base       http.RoundTripper
	MaxRetries int
	BaseDelay  time.Duration
	MaxDelay   time.Duration
}

// New returns a Transport wrapping base (http.DefaultTransport when nil).
func New(base http.RoundTripper, maxRetries int) *Transport {
	if base == nil {
		base = http.DefaultTransport
	}
	if maxRetries < 0 {
		maxRetries = 0
	}
	return &Transport{
		Base:       base,
		MaxRetries: maxRetries,
		BaseDelay:  500 * time.Millisecond,
		MaxDelay:   30 * time.Second,
	}
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Buffer the body so it can be replayed on retry (uploads are POSTs).
	var body []byte
	if req.Body != nil && req.Body != http.NoBody {
		b, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		body = b
	}

	for attempt := 0; ; attempt++ {
		r := req.Clone(req.Context())
		if body != nil {
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}

		resp, err := t.Base.RoundTrip(r)
		if err != nil {
			// Retry transient transport errors (reset/refused/EOF), but never
			// past context cancellation or the attempt budget.
			if attempt >= t.MaxRetries || req.Context().Err() != nil {
				return nil, err
			}
			if !sleep(req.Context(), t.backoff(attempt, 0)) {
				return nil, req.Context().Err()
			}
			continue
		}

		retry, wait := t.shouldRetry(resp, attempt)
		if !retry {
			return resp, nil
		}
		// Drain a little so the connection can be reused, then close.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if !sleep(req.Context(), t.backoff(attempt, wait)) {
			return nil, req.Context().Err()
		}
	}
}

// shouldRetry reports whether resp is a temporary refusal worth retrying and,
// if so, any server-requested wait (Retry-After). A 500 is retried only when
// its body reports a deadlock, since the transaction was rolled back and the
// request is safe to repeat.
func (t *Transport) shouldRetry(resp *http.Response, attempt int) (bool, time.Duration) {
	if attempt >= t.MaxRetries {
		return false, 0
	}
	switch resp.StatusCode {
	case http.StatusTooManyRequests, // 429
		http.StatusBadGateway,        // 502
		http.StatusServiceUnavailable, // 503
		http.StatusGatewayTimeout:    // 504
		return true, retryAfter(resp)
	case http.StatusInternalServerError: // 500: only rolled-back deadlocks
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		// Restore the body so the caller can read it if we do not retry.
		resp.Body = io.NopCloser(bytes.NewReader(b))
		if strings.Contains(strings.ToLower(string(b)), "deadlock") {
			return true, 0
		}
	}
	return false, 0
}

// backoff returns the delay before the next attempt: server Retry-After when
// given, otherwise exponential (BaseDelay<<attempt, capped) with equal jitter.
func (t *Transport) backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > t.MaxDelay {
			return t.MaxDelay
		}
		return retryAfter
	}
	d := t.BaseDelay << attempt
	if d <= 0 || d > t.MaxDelay {
		d = t.MaxDelay
	}
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

// retryAfter parses a Retry-After header (delta seconds or HTTP date).
func retryAfter(resp *http.Response) time.Duration {
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

// sleep waits d or until ctx is done. It reports false if ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
