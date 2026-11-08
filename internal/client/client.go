// Package client is a small HTTP client for the PushWard organization API,
// authenticated with an hlo_ org admin key.
package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultURL is the production API.
const DefaultURL = "https://api.pushward.app"

// The 404 codes that mean a sub-resource itself is gone. Nothing else does:
// org.not_found and orgs.disabled are also what a key that lost its
// organization sees, and must never empty a state.
const (
	CodeMemberNotFound = "org.member_not_found"
	CodeInviteNotFound = "org.invite_not_found"
	CodeGroupNotFound  = "org.group_not_found"
	CodeTagNotFound    = "org.tag_not_found"
	CodeRouteNotFound  = "org.route_not_found"
	CodeKeyNotFound    = "integration_key.not_found"
	CodeDomainNotFound = "org.domain_not_found"
	CodeSSONotFound    = "org.sso_not_found"
)

// The domain and single sign-on problems the provider acts on.
const (
	// CodeDomainExists is the 409 of a domain the organization already has.
	CodeDomainExists = "org.domain_exists"
	// CodeDomainNotVerified is the 422 of a verify that found no TXT record.
	CodeDomainNotVerified = "org.domain_not_verified"
	// CodeDomainTaken is the 409 of a domain another organization verified.
	CodeDomainTaken = "org.domain_taken"
	// CodePermissionDenied is the 403 of what the caller's role may not do.
	CodePermissionDenied = "org.permission_denied"
)

// CodeMemberEmailTaken is the 409 of an invite to an address that a member or
// an open invite of the organization already has.
const CodeMemberEmailTaken = "org.member_email_taken"

// CodeNameTaken is the 409 of a group or tag name the organization already
// uses.
const CodeNameTaken = "org.name_taken"

// codeEmailNotEnabled is the 503 of an invite email on a server that sends
// no mail.
const codeEmailNotEnabled = "email.not_enabled"

// Client calls the API. OrgID is set once the provider has resolved the
// key's organization.
type Client struct {
	BaseURL   string
	Key       string
	UserAgent string
	OrgID     string
	HTTP      *http.Client

	// MaxAttempts is how many times a request is sent, the first included.
	// A 429 is retried past it, within RateLimitBudget.
	MaxAttempts int
	// RateLimitBudget is how long one request may wait on 429s in all. Count
	// alone is not enough: Terraform sends 10 requests at once, and when the
	// key's limit lets one through at a time, a request can lose many rounds.
	RateLimitBudget time.Duration
	// MaxWait caps a single wait between attempts. A server hint longer than
	// that fails the request instead of blocking the run. It covers the flat
	// 60 s hint of the API's minute rate limits.
	MaxWait time.Duration
	// BaseWait is the first wait when the server gives no hint; it doubles
	// on every attempt.
	BaseWait time.Duration
}

// New returns a client for baseURL with the default retry policy.
func New(baseURL, key, version string) *Client {
	return &Client{
		BaseURL:         strings.TrimRight(baseURL, "/"),
		Key:             key,
		UserAgent:       "terraform-provider-pushward/" + version,
		HTTP:            &http.Client{Timeout: 30 * time.Second},
		MaxAttempts:     5,
		RateLimitBudget: 5 * time.Minute,
		MaxWait:         time.Minute,
		BaseWait:        time.Second,
	}
}

// Error is an RFC 9457 problem the API answered with.
type Error struct {
	Status     int
	Code       string
	Detail     string
	RequestID  string
	RetryAfter time.Duration
	// Repeated is set when an earlier attempt of the same call may have been
	// carried out: it got a 503 or a network error.
	Repeated bool
}

func (e *Error) Error() string {
	s := strconv.Itoa(e.Status)
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	if e.RequestID != "" {
		s += " (request " + e.RequestID + ")"
	}
	return s
}

// IsGone reports whether err is a 404 carrying code, the not-found code of
// the resource that was asked for.
func IsGone(err error, code string) bool {
	var e *Error
	return code != "" && errors.As(err, &e) && e.Status == http.StatusNotFound && e.Code == code
}

// HasCode reports whether err is a problem with code.
func HasCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// OrgPath is /orgs/{org_id} followed by the escaped segments.
func (c *Client) OrgPath(segments ...string) string {
	p := "/orgs/" + url.PathEscape(c.OrgID)
	for _, s := range segments {
		p += "/" + url.PathEscape(s)
	}
	return p
}

// Do sends in as JSON and decodes the response into out (either may be nil).
// 429, 503 and network errors are retried for every method. A POST carries a
// random Idempotency-Key, the same on every attempt of one call. The server
// answers a repeated create of a route, group, tag, invite or key with the row
// the first attempt made; a repeated key roll returns another fresh secret,
// and the last one is the one that works; adding and removing tag devices and
// verifying a domain are idempotent anyway. A domain create is not deduped, so
// it carries no key: with one, net/http resends a POST on its own when a
// reused connection drops, and that repeat's 409 would come without Repeated.
// Its repeat here gets 409 org.domain_exists with Repeated set, and the caller
// has to tell its own domain from one someone else added.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	var idempotencyKey string
	if method == http.MethodPost && path != c.OrgPath("domains") {
		idempotencyKey = rand.Text()
	}
	var limited time.Duration // waited on 429s so far
	repeated := false         // an earlier attempt may have been carried out
	for attempt := 1; ; attempt++ {
		err := c.send(ctx, method, path, body, idempotencyKey, out)
		if err == nil {
			return nil
		}
		var apiErr *Error
		if errors.As(err, &apiErr) {
			apiErr.Repeated = repeated
		}
		wait, ok := c.retryWait(ctx, err, attempt, limited)
		if !ok {
			return err
		}
		// A 429 is refused before anything is done; a 503 or a network
		// error may come after the write.
		if rateLimited(err) {
			limited += wait
		} else {
			repeated = true
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
	}
}

func rateLimited(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusTooManyRequests
}

// retryWait is how long to wait before the next attempt, and false when there
// is none. limited is what the request has waited on 429s so far.
func (c *Client) retryWait(ctx context.Context, err error, attempt int, limited time.Duration) (time.Duration, bool) {
	if ctx.Err() != nil || (attempt >= c.MaxAttempts && !rateLimited(err)) {
		return 0, false
	}
	wait := c.BaseWait << (attempt - 1)
	var apiErr *Error
	var netErr *url.Error
	switch {
	case errors.As(err, &apiErr):
		// A server with mail turned off answers email.not_enabled on every try.
		if (apiErr.Status != http.StatusTooManyRequests && apiErr.Status != http.StatusServiceUnavailable) || apiErr.Code == codeEmailNotEnabled {
			return 0, false
		}
		if apiErr.RetryAfter > 0 {
			wait = apiErr.RetryAfter
		}
	case errors.As(err, &netErr):
		// A certificate the client refuses is refused again on every try.
		var certErr *tls.CertificateVerificationError
		if errors.As(err, &certErr) {
			return 0, false
		}
	default:
		return 0, false
	}
	if wait > c.MaxWait {
		return 0, false
	}
	if rateLimited(err) {
		// The hint is when the key's next request gets through, the same for
		// every request waiting on it; a random extra spreads them out.
		wait = min(wait+mrand.N(wait+1), c.MaxWait)
		if limited+wait > c.RateLimitBudget {
			return 0, false
		}
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < wait {
		return 0, false
	}
	return wait, true
}

func (c *Client) send(ctx context.Context, method, path string, body []byte, idempotencyKey string, out any) error {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return &url.Error{Op: method, URL: req.URL.String(), Err: err}
	}
	if resp.StatusCode >= 400 {
		return parseProblem(resp, data)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s %s: decoding the response: %w", method, path, err)
	}
	return nil
}

func parseProblem(resp *http.Response, data []byte) *Error {
	var p struct {
		Title        string `json:"title"`
		Detail       string `json:"detail"`
		Code         string `json:"code"`
		Instance     string `json:"instance"`
		RetryAfterMs int64  `json:"retry_after_ms"`
	}
	_ = json.Unmarshal(data, &p) // a proxy's HTML error page still gets a status
	e := &Error{Status: resp.StatusCode, Code: p.Code, Detail: p.Detail}
	if e.Detail == "" {
		e.Detail = p.Title
	}
	if e.Detail == "" {
		e.Detail = http.StatusText(resp.StatusCode)
	}
	if i := strings.LastIndexByte(p.Instance, '/'); i >= 0 {
		e.RequestID = p.Instance[i+1:]
	}
	switch {
	case p.RetryAfterMs > 0:
		e.RetryAfter = time.Duration(p.RetryAfterMs) * time.Millisecond
	case resp.Header.Get("Retry-After") != "":
		e.RetryAfter = retryAfterHeader(resp.Header.Get("Retry-After"))
	}
	return e
}

// retryAfterHeader reads Retry-After as seconds or as an HTTP date.
func retryAfterHeader(v string) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return time.Until(t)
	}
	return 0
}

// List follows next_cursor from path to the last page.
func List[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var all []T
	cursor := ""
	for {
		p := path
		if cursor != "" {
			p += "?cursor=" + url.QueryEscape(cursor)
		}
		var page struct {
			Items      []T    `json:"items"`
			NextCursor string `json:"next_cursor"`
		}
		if err := c.Do(ctx, http.MethodGet, p, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if page.NextCursor == "" {
			return all, nil
		}
		if page.NextCursor == cursor {
			return nil, fmt.Errorf("GET %s: the server returned the same cursor twice", path)
		}
		cursor = page.NextCursor
	}
}
