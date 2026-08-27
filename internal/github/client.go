package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	defaultBaseURL = "https://api.github.com"
	apiVersion     = "2022-11-28"
	maxAttempts    = 3
	backoffBase    = 500 * time.Millisecond
	maxBodyBytes   = 32 << 20
)

// Client talks to the GitHub REST and GraphQL APIs. It is safe for
// concurrent use.
type Client struct {
	token   string
	version string
	base    string
	hc      *http.Client

	now   func() time.Time
	sleep func(context.Context, time.Duration) error

	rate atomic.Int64
}

// Option customises a Client.
type Option func(*Client)

// WithBaseURL points the client at another API root; both REST and GraphQL
// are served from it, which lets one httptest.Server stand in for both.
func WithBaseURL(u string) Option {
	return func(c *Client) { c.base = strings.TrimRight(u, "/") }
}

// WithHTTPClient replaces the underlying HTTP client.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.hc = h }
}

// WithClock injects the clock and sleep used by the retry logic so tests
// never sleep for real.
func WithClock(now func() time.Time, sleep func(context.Context, time.Duration) error) Option {
	return func(c *Client) {
		if now != nil {
			c.now = now
		}
		if sleep != nil {
			c.sleep = sleep
		}
	}
}

// New returns a Client authenticating with token. version is reported in the
// User-Agent.
func New(token, version string, opts ...Option) *Client {
	c := &Client{
		token:   token,
		version: version,
		base:    defaultBaseURL,
		hc: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				MaxIdleConns:        64,
				MaxIdleConnsPerHost: 16,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
				ForceAttemptHTTP2:   true,
			},
		},
		now:   time.Now,
		sleep: realSleep,
	}
	c.rate.Store(-1)
	for _, o := range opts {
		o(c)
	}
	return c
}

func realSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// RateRemaining reports the last x-ratelimit-remaining value seen, or -1 if
// the API has not told us yet.
func (c *Client) RateRemaining() int { return int(c.rate.Load()) }

type response struct {
	status int
	header http.Header
	body   []byte
}

// do performs one API call with retries. A non-2xx status that is not
// retryable (or is retryable but out of attempts) is returned as *APIError.
func (c *Client) do(ctx context.Context, method, path string, body []byte) (*response, error) {
	url := c.base + path
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, rdr)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", apiVersion)
		req.Header.Set("User-Agent", "forkman/"+c.version)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.hc.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("%s %s: %w", method, path, err)
			slog.Debug("http error", "method", method, "path", path, "attempt", attempt, "err", err)
			if attempt == maxAttempts {
				return nil, lastErr
			}
			if serr := c.sleep(ctx, backoff(attempt)); serr != nil {
				return nil, serr
			}
			continue
		}
		buf, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		resp.Body.Close()
		if rerr != nil {
			return nil, fmt.Errorf("%s %s: read body: %w", method, path, rerr)
		}
		c.recordRate(resp.Header)
		slog.Debug("http response", "method", method, "path", path, "status", resp.StatusCode,
			"attempt", attempt, "rate_remaining", c.RateRemaining())

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return &response{status: resp.StatusCode, header: resp.Header, body: buf}, nil
		}

		apiErr := parseAPIError(resp.StatusCode, resp.Header, buf)
		lastErr = apiErr
		wait, retry := retryDelay(apiErr, resp.Header, attempt, c.now)
		if !retry || attempt == maxAttempts {
			return nil, apiErr
		}
		if serr := c.sleep(ctx, wait); serr != nil {
			return nil, serr
		}
	}
	return nil, lastErr
}

func (c *Client) recordRate(h http.Header) {
	v := h.Get("X-RateLimit-Remaining")
	if v == "" {
		return
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return
	}
	c.rate.Store(int64(n))
}

// retryDelay decides whether an error response should be retried and how long
// to wait first.
func retryDelay(e *APIError, h http.Header, attempt int, now func() time.Time) (time.Duration, bool) {
	if e.RateLimited {
		if d, ok := retryAfter(h); ok {
			return d, true
		}
		if v := h.Get("X-RateLimit-Reset"); v != "" {
			if sec, err := strconv.ParseInt(v, 10, 64); err == nil {
				d := time.Unix(sec, 0).Sub(now())
				if d < 0 {
					d = 0
				}
				return d, true
			}
		}
		return backoff(attempt), true
	}
	switch e.Status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		if d, ok := retryAfter(h); ok {
			return d, true
		}
		return backoff(attempt), true
	}
	return 0, false
}

func retryAfter(h http.Header) (time.Duration, bool) {
	v := h.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	sec, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || sec < 0 {
		return 0, false
	}
	return time.Duration(sec) * time.Second, true
}

// backoff is exponential with full jitter, base 500ms.
func backoff(attempt int) time.Duration {
	d := backoffBase << (attempt - 1)
	if d <= 0 {
		return backoffBase
	}
	return rand.N(d)
}

type errorBody struct {
	Message string `json:"message"`
	DocURL  string `json:"documentation_url"`
}

func parseAPIError(status int, h http.Header, body []byte) *APIError {
	e := &APIError{Status: status, Scopes: h.Get("X-OAuth-Scopes")}
	var eb errorBody
	if err := json.Unmarshal(body, &eb); err == nil {
		e.Message = eb.Message
		e.DocURL = eb.DocURL
	}
	if e.Message == "" {
		e.Message = strings.TrimSpace(http.StatusText(status))
	}
	remaining := h.Get("X-RateLimit-Remaining")
	switch {
	case status == http.StatusTooManyRequests:
		e.RateLimited = true
	case status == http.StatusForbidden && remaining == "0":
		e.RateLimited = true
	}
	if strings.Contains(strings.ToLower(e.Message), "secondary rate limit") {
		e.SecondaryLimit = true
		e.RateLimited = false
	}
	return e
}

// GetUser fetches the authenticated user and the scopes the token advertises.
func (c *Client) GetUser(ctx context.Context) (*User, error) {
	resp, err := c.do(ctx, http.MethodGet, "/user", nil)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(resp.body, &payload); err != nil {
		return nil, fmt.Errorf("decode /user: %w", err)
	}
	u := &User{Login: payload.Login}
	raw := strings.TrimSpace(resp.header.Get("X-OAuth-Scopes"))
	if raw != "" {
		for _, s := range strings.Split(raw, ",") {
			if s = strings.TrimSpace(s); s != "" {
				u.Scopes = append(u.Scopes, s)
			}
		}
		u.ScopesKnown = len(u.Scopes) > 0
	}
	return u, nil
}

// GetOrg checks that the organization is visible to this token.
func (c *Client) GetOrg(ctx context.Context, org string) error {
	_, err := c.do(ctx, http.MethodGet, "/orgs/"+pathSeg(org), nil)
	return err
}

// Compare returns the ahead/behind relationship between the parent branch and
// the fork branch.
func (c *Client) Compare(ctx context.Context, org, repo, parentOwner, parentBranch, forkBranch string) (*Compare, error) {
	path := fmt.Sprintf("/repos/%s/%s/compare/%s:%s...%s:%s",
		pathSeg(org), pathSeg(repo), pathSeg(parentOwner), parentBranch, pathSeg(org), forkBranch)
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var payload struct {
		AheadBy  int    `json:"ahead_by"`
		BehindBy int    `json:"behind_by"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal(resp.body, &payload); err != nil {
		return nil, fmt.Errorf("decode compare: %w", err)
	}
	return &Compare{AheadBy: payload.AheadBy, BehindBy: payload.BehindBy, Status: payload.Status}, nil
}

// MergeUpstream syncs the fork's branch from its parent.
func (c *Client) MergeUpstream(ctx context.Context, org, repo, branch string) (*MergeResult, error) {
	body, err := json.Marshal(map[string]string{"branch": branch})
	if err != nil {
		return nil, fmt.Errorf("encode merge-upstream: %w", err)
	}
	path := fmt.Sprintf("/repos/%s/%s/merge-upstream", pathSeg(org), pathSeg(repo))
	resp, err := c.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Message    string `json:"message"`
		MergeType  string `json:"merge_type"`
		BaseBranch string `json:"base_branch"`
	}
	if err := json.Unmarshal(resp.body, &payload); err != nil {
		return nil, fmt.Errorf("decode merge-upstream: %w", err)
	}
	return &MergeResult{MergeType: payload.MergeType, Message: payload.Message, BaseBranch: payload.BaseBranch}, nil
}

// pathSeg keeps a path segment free of separators without percent-encoding
// the characters GitHub expects to see literally.
func pathSeg(s string) string {
	return strings.ReplaceAll(s, "/", "%2F")
}

var errEmptyGraphQL = errors.New("graphql: empty organization in response")
