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
		{"POST does not retry 503", http.MethodPost, []int{503}, 1, false},
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

func TestPOSTNotRetriedOnNetworkError(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	})
	if err := c.Do(context.Background(), http.MethodPost, c.OrgPath("tags"), map[string]string{"name": "a"}, nil); err == nil {
		t.Fatal("no error")
	}
	if calls.Load() != 1 {
		t.Errorf("POST sent %d times", calls.Load())
	}
	calls.Store(0)
	if err := c.Do(context.Background(), http.MethodGet, c.OrgPath("tags"), nil, nil); err == nil {
		t.Fatal("no error")
	}
	if calls.Load() != int32(c.MaxAttempts) {
		t.Errorf("GET sent %d times, want %d", calls.Load(), c.MaxAttempts)
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
	err := c.Do(context.Background(), http.MethodGet, "/orgs", nil, nil)
	var certErr *tls.CertificateVerificationError
	if !errors.As(err, &certErr) {
		t.Fatalf("err = %v, want a certificate error", err)
	}
	if n := conns.Load(); n != 1 {
		t.Errorf("connected %d times, want 1", n)
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
