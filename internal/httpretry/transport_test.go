package httpretry

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// seqRT returns scripted responses, recording how many attempts were made.
type seqRT struct {
	statuses []int
	bodies   []string
	attempts int
	lastBody string
}

func (s *seqRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		s.lastBody = string(b)
	}
	i := s.attempts
	s.attempts++
	if i >= len(s.statuses) {
		i = len(s.statuses) - 1
	}
	body := ""
	if i < len(s.bodies) {
		body = s.bodies[i]
	}
	return &http.Response{
		StatusCode: s.statuses[i],
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
	}, nil
}

func newTestTransport(rt http.RoundTripper) *Transport {
	t := New(rt, 5)
	t.BaseDelay = time.Millisecond
	t.MaxDelay = 5 * time.Millisecond
	return t
}

func TestRetries429ThenSucceeds(t *testing.T) {
	rt := &seqRT{statuses: []int{429, 200}, bodies: []string{"rate", "ok"}}
	resp, err := newTestTransport(rt).RoundTrip(mustReq(t, "GET", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if rt.attempts != 2 {
		t.Fatalf("attempts = %d, want 2", rt.attempts)
	}
}

func TestRetries503ThenSucceeds(t *testing.T) {
	rt := &seqRT{statuses: []int{503, 503, 200}}
	resp, err := newTestTransport(rt).RoundTrip(mustReq(t, "GET", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || rt.attempts != 3 {
		t.Fatalf("status=%d attempts=%d, want 200/3", resp.StatusCode, rt.attempts)
	}
}

func TestRetriesDeadlock500(t *testing.T) {
	rt := &seqRT{
		statuses: []int{500, 200},
		bodies:   []string{"Deadlock found when trying to get lock", "ok"},
	}
	resp, err := newTestTransport(rt).RoundTrip(mustReq(t, "GET", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || rt.attempts != 2 {
		t.Fatalf("status=%d attempts=%d, want 200/2", resp.StatusCode, rt.attempts)
	}
}

func TestDoesNotRetryNonDeadlock500(t *testing.T) {
	rt := &seqRT{statuses: []int{500, 200}, bodies: []string{"User authentication failed", "ok"}}
	resp, err := newTestTransport(rt).RoundTrip(mustReq(t, "GET", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 500 {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if rt.attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (no retry)", rt.attempts)
	}
	// Body must still be readable by the caller.
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "authentication failed") {
		t.Fatalf("body lost: %q", string(b))
	}
}

func TestExhaustsRetries(t *testing.T) {
	rt := &seqRT{statuses: []int{503}}
	resp, err := newTestTransport(rt).RoundTrip(mustReq(t, "GET", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 503 {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if rt.attempts != 6 { // 1 + MaxRetries(5)
		t.Fatalf("attempts = %d, want 6", rt.attempts)
	}
}

func TestReplaysBodyOnRetry(t *testing.T) {
	rt := &seqRT{statuses: []int{429, 201}, bodies: []string{"rate", "ok"}}
	req := mustReq(t, "POST", bytes.NewReader([]byte("payload")))
	resp, err := newTestTransport(rt).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 201 {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if rt.lastBody != "payload" {
		t.Fatalf("replayed body = %q, want %q", rt.lastBody, "payload")
	}
}

func mustReq(t *testing.T, method string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, "http://example.test/x", body)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
