package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(srv.URL, "hlo_test", "test")
	c.BaseWait = time.Millisecond
	c.OrgID = "org-1"
	return c
}

func writeProblem(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	fmt.Fprint(w, body)
}

func TestProblemParsing(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer hlo_test" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "terraform-provider-pushward/test" {
			t.Errorf("User-Agent = %q", got)
		}
		writeProblem(w, 403, `{"type":"https://pushward.app/errors/forbidden","title":"Forbidden","status":403,
			"code":"org.billing_inactive","detail":"this organization is not active","instance":"/requests/9f86d081884c7d65"}`)
	})
	err := c.Do(context.Background(), http.MethodGet, c.OrgPath("groups"), nil, nil)
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want *Error", err)
	}
	want := Error{Status: 403, Code: "org.billing_inactive", Detail: "this organization is not active", RequestID: "9f86d081884c7d65"}
	if *e != want {
		t.Errorf("got %+v, want %+v", *e, want)
	}
	if got := e.Error(); got != "403 org.billing_inactive: this organization is not active (request 9f86d081884c7d65)" {
		t.Errorf("Error() = %q", got)
	}
}

func TestProblemWithoutJSON(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "<html>bad gateway</html>")
	})
	err := c.Do(context.Background(), http.MethodGet, "/orgs", nil, nil)
	var e *Error
	if !errors.As(err, &e) || e.Status != 502 || e.Code != "" || e.Detail != "Bad Gateway" {
		t.Fatalf("err = %#v", err)
	}
}

func TestIsGone(t *testing.T) {
	codes := []string{CodeMemberNotFound, CodeInviteNotFound, CodeGroupNotFound, CodeTagNotFound, CodeRouteNotFound, CodeKeyNotFound}
	for _, code := range codes {
		if !IsGone(&Error{Status: 404, Code: code}, code) {
			t.Errorf("%s: its own 404 is not gone", code)
		}
		if IsGone(fmt.Errorf("wrapped: %w", &Error{Status: 404, Code: code}), code) != true {
			t.Errorf("%s: wrapped 404 is not gone", code)
		}
		for _, err := range []error{
			&Error{Status: 404, Code: "org.not_found"},
			&Error{Status: 404, Code: "orgs.disabled"},
			&Error{Status: 404},
			&Error{Status: 403, Code: code},
			&Error{Status: 410, Code: code},
			errors.New(code),
			nil,
		} {
			if IsGone(err, code) {
				t.Errorf("%s: %v counts as gone", code, err)
			}
		}
		for _, other := range codes {
			if other != code && IsGone(&Error{Status: 404, Code: other}, code) {
				t.Errorf("%s: another resource's 404 %s counts as gone", code, other)
			}
		}
	}
	if IsGone(&Error{Status: 404}, "") {
		t.Error("a 404 without a code is gone for an empty code")
	}
}

// sequence answers with statuses in order, then 200, and counts the calls.
func sequence(calls *atomic.Int32, statuses ...int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1))
		if n <= len(statuses) {
			writeProblem(w, statuses[n-1], `{"status":0,"code":"x"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"g1"}`)
	}
}

func TestRetries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		method   string
		statuses []int
		calls    int32
		ok       bool
	}{
		{"GET retries 429", http.MethodGet, []int{429}, 2, true},
		{"GET retries 503", http.MethodGet, []int{503, 503}, 3, true},
		{"PUT retries 503", http.MethodPut, []int{503}, 2, true},
		{"PATCH retries 503", http.MethodPatch, []int{503}, 2, true},
		{"DELETE retries 503", http.MethodDelete, []int{503}, 2, true},
		{"POST retries 429", http.MethodPost, []int{429}, 2, true},
		{"POST retries 503", http.MethodPost, []int{503}, 2, true},
		{"GET does not retry 500", http.MethodGet, []int{500}, 1, false},
		{"GET does not retry 409", http.MethodGet, []int{409}, 1, false},
		{"cap reached", http.MethodGet, []int{503, 503, 503, 503, 503, 503}, 5, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c := testClient(t, sequence(&calls, tc.statuses...))
			var out Group
			err := c.Do(context.Background(), tc.method, c.OrgPath("groups"), map[string]string{"name": "a"}, &out)
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v", err)
			}
			if calls.Load() != tc.calls {
				t.Errorf("calls = %d, want %d", calls.Load(), tc.calls)
			}
			if tc.ok && out.ID != "g1" {
				t.Errorf("out = %+v", out)
			}
		})
	}
}

// A POST is retried after a lost connection and a 503 with the key of its
// first attempt; the next call gets a new key, and other methods none.
func TestPOSTRetriedWithOneIdempotencyKey(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		n := len(keys)
		mu.Unlock()
		switch n {
		case 1:
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				conn.Close()
			}
		case 2:
			writeProblem(w, 503, `{"status":503,"code":"service.unavailable"}`)
		default:
			w.WriteHeader(http.StatusCreated)
		}
	})
	if err := c.Do(context.Background(), http.MethodPost, c.OrgPath("tags"), map[string]string{"name": "a"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Do(context.Background(), http.MethodPost, c.OrgPath("tags"), map[string]string{"name": "b"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Do(context.Background(), http.MethodGet, c.OrgPath("tags"), nil, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 5 {
		t.Fatalf("sent %d requests, want 5: %q", len(keys), keys)
	}
	valid := regexp.MustCompile(`^[!-~]{1,128}$`)
	if !valid.MatchString(keys[0]) || keys[1] != keys[0] || keys[2] != keys[0] {
		t.Errorf("the attempts of one POST sent the keys %q, want one valid key", keys[:3])
	}
	if !valid.MatchString(keys[3]) || keys[3] == keys[0] {
		t.Errorf("the second POST sent %q after %q, want a new key", keys[3], keys[0])
	}
	if keys[4] != "" {
		t.Errorf("GET sent Idempotency-Key %q", keys[4])
	}
}

func TestUntrustedCertificateNotRetried(t *testing.T) {
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	c := New(srv.URL, "hlo_test", "test") // trusts the system roots, not the test certificate
	c.BaseWait = time.Millisecond
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		conns.Store(0)
		err := c.Do(context.Background(), method, "/orgs", nil, nil)
		var certErr *tls.CertificateVerificationError
		if !errors.As(err, &certErr) {
			t.Fatalf("%s: err = %v, want a certificate error", method, err)
		}
		if n := conns.Load(); n != 1 {
			t.Errorf("%s: connected %d times, want 1", method, n)
		}
	}
}

func TestRetryAfterHonoured(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		body   string
		min    time.Duration
	}{
		{"retry_after_ms", "", `{"status":429,"code":"rate_limit.exceeded","retry_after_ms":150}`, 150 * time.Millisecond},
		{"Retry-After header", "1", `{"status":503,"code":"service.unavailable"}`, time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					if tc.header != "" {
						w.Header().Set("Retry-After", tc.header)
					}
					writeProblem(w, 429, tc.body)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			})
			start := time.Now()
			if err := c.Do(context.Background(), http.MethodGet, "/orgs", nil, nil); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); d < tc.min {
				t.Errorf("retried after %v, want at least %v", d, tc.min)
			}
		})
	}
}

// The API's minute rate limits answer with a flat 60 s hint, in the header and
// in retry_after_ms; the default policy waits that long instead of failing.
func TestRetryAfterOneMinuteWaited(t *testing.T) {
	c := New(DefaultURL, "hlo_test", "test")
	for _, body := range []string{`{"status":429,"code":"rate_limit.exceeded","retry_after_ms":60000}`, `{"status":429}`} {
		resp := &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"60"}}}
		wait, ok := c.retryWait(context.Background(), parseProblem(resp, []byte(body)), 1, 0)
		if !ok || wait != time.Minute {
			t.Errorf("%s: wait = %v, %v; want 1m0s, true", body, wait, ok)
		}
	}
}

// Ten requests at once against a key that lets one through every 20 ms, with
// the exact wait in retry_after_ms as the API's limiter gives it: every one
// gets through, though most lose more rounds than MaxAttempts.
func TestRateLimitedRequestsAllGetThrough(t *testing.T) {
	const interval = 20 * time.Millisecond
	var mu sync.Mutex
	var next time.Time
	var refused atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		now := time.Now()
		if now.Before(next) {
			wait := max(next.Sub(now).Milliseconds(), 1)
			mu.Unlock()
			refused.Add(1)
			writeProblem(w, 429, fmt.Sprintf(`{"status":429,"code":"rate_limit.exceeded","retry_after_ms":%d}`, wait))
			return
		}
		next = now.Add(interval)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for range 10 {
		wg.Go(func() { errs <- c.Do(context.Background(), http.MethodGet, "/orgs", nil, nil) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if refused.Load() < int32(c.MaxAttempts) {
		t.Errorf("only %d refusals: the limiter did not make requests wait", refused.Load())
	}
}

// A 429 that never clears fails once the request has waited RateLimitBudget.
func TestRateLimitBudget(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeProblem(w, 429, `{"status":429,"code":"rate_limit.exceeded","retry_after_ms":10}`)
	})
	c.RateLimitBudget = 200 * time.Millisecond
	start := time.Now()
	if err := c.Do(context.Background(), http.MethodGet, "/orgs", nil, nil); !HasCode(err, "rate_limit.exceeded") {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second || calls.Load() <= int32(c.MaxAttempts) {
		t.Errorf("%d calls in %v: want more than MaxAttempts, stopping near the budget", calls.Load(), d)
	}
}

func TestRetryAfterBeyondCap(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeProblem(w, 429, `{"status":429,"code":"org.invite_email_rate_limited","retry_after_ms":3600000}`)
	})
	start := time.Now()
	err := c.Do(context.Background(), http.MethodGet, "/orgs", nil, nil)
	if !HasCode(err, "org.invite_email_rate_limited") {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 1 || time.Since(start) > 5*time.Second {
		t.Errorf("waited for a hint beyond the cap: %d calls in %v", calls.Load(), time.Since(start))
	}
}

func TestRetryStopsAtDeadline(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeProblem(w, 503, `{"status":503,"code":"service.unavailable","retry_after_ms":5000}`)
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	if err := c.Do(ctx, http.MethodGet, "/orgs", nil, nil); err == nil {
		t.Fatal("no error")
	}
	if calls.Load() != 1 || time.Since(start) > 900*time.Millisecond {
		t.Errorf("slept past the deadline: %d calls in %v", calls.Load(), time.Since(start))
	}
}

func TestListFollowsCursor(t *testing.T) {
	pages := map[string]string{
		"":    `{"items":[{"user_id":"u1"},{"user_id":"u2"}],"next_cursor":"c 1"}`,
		"c 1": `{"items":[{"user_id":"u3"}],"next_cursor":"c2"}`,
		"c2":  `{"items":[{"user_id":"u4"}]}`,
	}
	var mu sync.Mutex
	var seen []string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		cur := r.URL.Query().Get("cursor")
		mu.Lock()
		seen = append(seen, cur)
		mu.Unlock()
		fmt.Fprint(w, pages[cur])
	})
	got, err := List[Member](context.Background(), c, c.OrgPath("members"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0].UserID != "u1" || got[3].UserID != "u4" {
		t.Errorf("got %+v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(seen) != "[ c 1 c2]" {
		t.Errorf("cursors sent: %q", seen)
	}
}

func TestListStopsOnRepeatedCursor(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"items":[{"user_id":"u1"}],"next_cursor":"same"}`)
	})
	if _, err := List[Member](context.Background(), c, "/x"); err == nil {
		t.Fatal("a cursor that never moves looped or passed")
	}
}

// A server with mail turned off answers an invite with 503 email.not_enabled
// before it stores anything, and answers the same on every try.
func TestEmailNotEnabledNotRetried(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeProblem(w, 503, `{"status":503,"code":"email.not_enabled"}`)
	})
	err := c.Do(context.Background(), http.MethodPost, c.OrgPath("invites"), map[string]string{"email": "ann@example.com"}, nil)
	if !HasCode(err, "email.not_enabled") {
		t.Fatalf("err = %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("sent %d times, want 1", n)
	}
}
