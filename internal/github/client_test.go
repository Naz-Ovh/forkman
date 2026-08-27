package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

var testNow = time.Unix(1700000000, 0)

// sleepRecorder captures every retry sleep so tests assert on the durations
// instead of waiting for them.
type sleepRecorder struct {
	mu   sync.Mutex
	dur  []time.Duration
	fail error
}

func (s *sleepRecorder) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dur = append(s.dur, d)
	return s.fail
}

func (s *sleepRecorder) durations() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.dur...)
}

func newTestClient(t *testing.T, h http.Handler) (*Client, *sleepRecorder) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	rec := &sleepRecorder{}
	c := New("s3cret-token", "test",
		WithBaseURL(srv.URL),
		WithHTTPClient(srv.Client()),
		WithClock(func() time.Time { return testNow }, rec.sleep),
	)
	return c, rec
}

func TestRequestHeaders(t *testing.T) {
	var got http.Header
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("X-RateLimit-Remaining", "4832")
		fmt.Fprint(w, `{"login":"octo"}`)
	}))
	if _, err := c.GetUser(context.Background()); err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	for k, want := range map[string]string{
		"Authorization":        "Bearer s3cret-token",
		"Accept":               "application/vnd.github+json",
		"X-Github-Api-Version": apiVersion,
		"User-Agent":           "forkman/test",
	} {
		if got.Get(k) != want {
			t.Errorf("header %s = %q, want %q", k, got.Get(k), want)
		}
	}
	if c.RateRemaining() != 4832 {
		t.Errorf("RateRemaining = %d, want 4832", c.RateRemaining())
	}
}

func TestRateRemainingUnknown(t *testing.T) {
	c := New("t", "test")
	if c.RateRemaining() != -1 {
		t.Errorf("RateRemaining = %d, want -1", c.RateRemaining())
	}
}

func TestMergeUpstream(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		headers    map[string]string
		body       string
		wantType   string
		wantStatus int
		wantMsg    string
		wantSecond bool
		wantLimit  bool
	}{
		{
			name: "fast-forward", status: 200,
			body:     `{"message":"Successfully fetched and fast-forwarded","merge_type":"fast-forward","base_branch":"upstream:main"}`,
			wantType: "fast-forward",
		},
		{
			name: "merge", status: 200,
			body:     `{"message":"Successfully merged","merge_type":"merge","base_branch":"upstream:main"}`,
			wantType: "merge",
		},
		{
			name: "none", status: 200,
			body:     `{"message":"already up to date","merge_type":"none","base_branch":"upstream:main"}`,
			wantType: "none",
		},
		{
			name: "conflict", status: 409,
			body:       `{"message":"There are merge conflicts"}`,
			wantStatus: 409, wantMsg: "There are merge conflicts",
		},
		{
			name: "unprocessable", status: 422,
			body:       `{"message":"refusing to allow an OAuth App to create or update workflow \".github/workflows/ci.yml\" without \"workflow\" scope"}`,
			wantStatus: 422,
			wantMsg:    `refusing to allow an OAuth App to create or update workflow ".github/workflows/ci.yml" without "workflow" scope`,
		},
		{
			name: "forbidden permission", status: 403,
			headers:    map[string]string{"X-RateLimit-Remaining": "4711"},
			body:       `{"message":"Resource not accessible by integration"}`,
			wantStatus: 403, wantMsg: "Resource not accessible by integration",
		},
		{
			name: "secondary rate limit", status: 403,
			headers:    map[string]string{"X-RateLimit-Remaining": "4711"},
			body:       `{"message":"You have exceeded a secondary rate limit"}`,
			wantStatus: 403, wantMsg: "You have exceeded a secondary rate limit", wantSecond: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("method = %s, want POST", r.Method)
				}
				if want := "/repos/acme/tempo/merge-upstream"; r.URL.Path != want {
					t.Errorf("path = %s, want %s", r.URL.Path, want)
				}
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			res, err := c.MergeUpstream(context.Background(), "acme", "tempo", "main")
			if tc.wantStatus == 0 {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if res.MergeType != tc.wantType {
					t.Errorf("MergeType = %q, want %q", res.MergeType, tc.wantType)
				}
				return
			}
			var ae *APIError
			if !errors.As(err, &ae) {
				t.Fatalf("error = %v, want *APIError", err)
			}
			if ae.Status != tc.wantStatus {
				t.Errorf("Status = %d, want %d", ae.Status, tc.wantStatus)
			}
			if ae.Message != tc.wantMsg {
				t.Errorf("Message = %q, want verbatim %q", ae.Message, tc.wantMsg)
			}
			if ae.SecondaryLimit != tc.wantSecond {
				t.Errorf("SecondaryLimit = %v, want %v", ae.SecondaryLimit, tc.wantSecond)
			}
			if ae.RateLimited != tc.wantLimit {
				t.Errorf("RateLimited = %v, want %v", ae.RateLimited, tc.wantLimit)
			}
			if strings.Contains(ae.Error(), "s3cret-token") {
				t.Error("APIError leaked the token")
			}
			if len(rec.durations()) != 0 {
				t.Errorf("slept %v on a non-retryable status", rec.durations())
			}
		})
	}
}

func TestRateLimitSleepsUntilReset(t *testing.T) {
	reset := testNow.Add(90 * time.Second).Unix()
	var calls int
	c, rec := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(reset))
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
	}))
	start := time.Now()
	_, err := c.MergeUpstream(context.Background(), "acme", "tempo", "main")
	elapsed := time.Since(start)

	var ae *APIError
	if !errors.As(err, &ae) || !ae.RateLimited {
		t.Fatalf("error = %v, want rate-limited *APIError", err)
	}
	if calls != maxAttempts {
		t.Errorf("attempts = %d, want %d", calls, maxAttempts)
	}
	want := []time.Duration{90 * time.Second, 90 * time.Second}
	got := rec.durations()
	if len(got) != len(want) {
		t.Fatalf("sleeps = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sleep[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	if elapsed > time.Second {
		t.Errorf("test slept for real: %v", elapsed)
	}
	if c.RateRemaining() != 0 {
		t.Errorf("RateRemaining = %d, want 0", c.RateRemaining())
	}
}

func TestTooManyRequestsHonorsRetryAfter(t *testing.T) {
	var calls int
	c, rec := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"message":"slow down"}`)
			return
		}
		fmt.Fprint(w, `{"merge_type":"fast-forward"}`)
	}))
	res, err := c.MergeUpstream(context.Background(), "acme", "tempo", "main")
	if err != nil {
		t.Fatalf("MergeUpstream: %v", err)
	}
	if res.MergeType != "fast-forward" {
		t.Errorf("MergeType = %q", res.MergeType)
	}
	if got := rec.durations(); len(got) != 1 || got[0] != 2*time.Second {
		t.Errorf("sleeps = %v, want [2s]", got)
	}
}

func TestServerErrorRetriesWithBackoff(t *testing.T) {
	var calls int
	c, rec := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, `{"merge_type":"merge"}`)
	}))
	if _, err := c.MergeUpstream(context.Background(), "acme", "tempo", "main"); err != nil {
		t.Fatalf("MergeUpstream: %v", err)
	}
	got := rec.durations()
	if len(got) != 2 {
		t.Fatalf("sleeps = %v, want 2", got)
	}
	if got[0] >= backoffBase || got[1] >= 2*backoffBase {
		t.Errorf("backoff out of full-jitter range: %v", got)
	}
}

func TestGetUserUnauthorized(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"message":"Bad credentials","documentation_url":"https://docs.github.com/rest"}`)
	}))
	_, err := c.GetUser(context.Background())
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if ae.Status != 401 || ae.Message != "Bad credentials" {
		t.Errorf("got %d %q", ae.Status, ae.Message)
	}
	if ae.DocURL == "" {
		t.Error("DocURL not preserved")
	}
	if got := ae.Error(); got != "GitHub API 401: Bad credentials" {
		t.Errorf("Error() = %q", got)
	}
}

func TestGetUserScopes(t *testing.T) {
	tests := []struct {
		name       string
		header     string
		wantScopes []string
		wantKnown  bool
	}{
		{"classic pat", "repo, workflow, read:org", []string{"repo", "workflow", "read:org"}, true},
		{"fine grained", "", nil, false},
		{"blank header", "   ", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.header != "" {
					w.Header().Set("X-OAuth-Scopes", tc.header)
				}
				fmt.Fprint(w, `{"login":"octo"}`)
			}))
			u, err := c.GetUser(context.Background())
			if err != nil {
				t.Fatalf("GetUser: %v", err)
			}
			if u.ScopesKnown != tc.wantKnown {
				t.Errorf("ScopesKnown = %v, want %v", u.ScopesKnown, tc.wantKnown)
			}
			if strings.Join(u.Scopes, ",") != strings.Join(tc.wantScopes, ",") {
				t.Errorf("Scopes = %v, want %v", u.Scopes, tc.wantScopes)
			}
		})
	}
}

func TestMalformedJSON(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"login": "octo"`)
	}))
	_, err := c.GetUser(context.Background())
	if err == nil {
		t.Fatal("want decode error, got nil")
	}
	var ae *APIError
	if errors.As(err, &ae) {
		t.Fatalf("want a decode error, got *APIError: %v", err)
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("error = %v, want a decode error", err)
	}
}

func TestCompare(t *testing.T) {
	var gotPath string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		fmt.Fprint(w, `{"ahead_by":3,"behind_by":12,"status":"diverged"}`)
	}))
	cmp, err := c.Compare(context.Background(), "acme", "tempo", "grafana", "main", "main")
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if cmp.AheadBy != 3 || cmp.BehindBy != 12 || cmp.Status != "diverged" {
		t.Errorf("got %+v", cmp)
	}
	if want := "/repos/acme/tempo/compare/grafana:main...acme:main"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

func TestGetOrgNotFound(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	}))
	err := c.GetOrg(context.Background(), "ghost")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 404 {
		t.Fatalf("error = %v, want 404 *APIError", err)
	}
}

// discoveryServer serves both phases of fork discovery: the paginated cheap
// listing, then the aliased detail queries. detail maps a repository name to
// its detail payload; a name that is absent comes back as a null alias with a
// NOT_FOUND error, the way GitHub reports a repository that has gone.
type discoveryServer struct {
	pages  [][]string // fork names, one slice per list page
	detail map[string]string

	mu      sync.Mutex
	cursors []any
	batches [][]string
}

func (d *discoveryServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/graphql" {
		http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		return
	}
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.Contains(req.Query, "repositories(") {
		d.serveList(w, req.Query, req.Variables)
		return
	}
	d.serveDetail(w, req.Variables)
}

func (d *discoveryServer) serveList(w http.ResponseWriter, query string, vars map[string]any) {
	d.mu.Lock()
	page := len(d.cursors)
	d.cursors = append(d.cursors, vars["cursor"])
	d.mu.Unlock()

	if !strings.Contains(query, "isFork: true") {
		http.Error(w, "listing query does not filter forks", http.StatusBadRequest)
		return
	}
	if page >= len(d.pages) {
		http.Error(w, "unexpected extra list page", http.StatusBadRequest)
		return
	}
	nodes := make([]string, 0, len(d.pages[page]))
	for _, name := range d.pages[page] {
		nodes = append(nodes, fmt.Sprintf(
			`{"name":%q,"nameWithOwner":%q,"isArchived":false,"viewerPermission":"WRITE"}`,
			name, "acme/"+name))
	}
	last := page == len(d.pages)-1
	fmt.Fprintf(w, `{"data":{"organization":{"repositories":{
		"pageInfo":{"hasNextPage":%t,"endCursor":%q},
		"nodes":[%s]}}}}`, !last, fmt.Sprintf("CUR%d", page+1), strings.Join(nodes, ","))
}

func (d *discoveryServer) serveDetail(w http.ResponseWriter, vars map[string]any) {
	var (
		names   []string
		aliases []string
		errs    []string
	)
	for i := 0; ; i++ {
		v, ok := vars[fmt.Sprintf("n%d", i)]
		if !ok {
			break
		}
		name, _ := v.(string)
		names = append(names, name)
		det, found := d.detail[name]
		if !found {
			aliases = append(aliases, fmt.Sprintf(`"r%d":null`, i))
			errs = append(errs, fmt.Sprintf(
				`{"type":"NOT_FOUND","message":"Could not resolve to a Repository with the name 'acme/%s'."}`, name))
			continue
		}
		aliases = append(aliases, fmt.Sprintf(`"r%d":%s`, i, det))
	}
	d.mu.Lock()
	d.batches = append(d.batches, names)
	d.mu.Unlock()

	errBlock := ""
	if len(errs) > 0 {
		errBlock = fmt.Sprintf(`,"errors":[%s]`, strings.Join(errs, ","))
	}
	fmt.Fprintf(w, `{"data":{%s}%s}`, strings.Join(aliases, ","), errBlock)
}

// requested returns every repository name the detail phase asked about.
func (d *discoveryServer) requested() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, b := range d.batches {
		out = append(out, b...)
	}
	sort.Strings(out)
	return out
}

func detailJSON(branch, oid, parent, parentBranch, parentOID string) string {
	return fmt.Sprintf(`{"defaultBranchRef":{"name":%q,"target":{"oid":%q}},
		"parent":{"nameWithOwner":%q,"defaultBranchRef":{"name":%q,"target":{"oid":%q}}}}`,
		branch, oid, parent, parentBranch, parentOID)
}

func TestListForksDiscoversInTwoPhases(t *testing.T) {
	srv := &discoveryServer{
		pages: [][]string{{"tempo", "orphan"}, {"vault"}},
		detail: map[string]string{
			"tempo":  detailJSON("main", "aaa", "grafana/tempo", "main", "bbb"),
			"orphan": `{"defaultBranchRef":null,"parent":null}`,
			"vault":  detailJSON("master", "ccc", "hashicorp/vault", "master", "ccc"),
		},
	}
	c, _ := newTestClient(t, srv)

	var listed []int
	var detailed [][2]int
	forks, err := c.ListForksProgress(context.Background(), "acme", &Discovery{
		OnListed: func(found int) { listed = append(listed, found) },
		OnDetail: func(done, total int) { detailed = append(detailed, [2]int{done, total}) },
	})
	if err != nil {
		t.Fatalf("ListForksProgress: %v", err)
	}
	if len(forks) != 3 {
		t.Fatalf("got %d forks, want 3", len(forks))
	}
	if got := srv.cursors; len(got) != 2 || got[0] != nil || got[1] != "CUR1" {
		t.Errorf("cursors = %v, want [nil CUR1]", got)
	}
	if got, want := srv.requested(), []string{"orphan", "tempo", "vault"}; !slices.Equal(got, want) {
		t.Errorf("detail phase asked for %v, want %v", got, want)
	}

	want := Fork{
		Name: "tempo", NameWithOwner: "acme/tempo", ViewerPermission: "WRITE",
		DefaultBranch: "main", HeadOID: "aaa",
		ParentNameWithOwner: "grafana/tempo", ParentDefaultBranch: "main", ParentHeadOID: "bbb",
		HasParent: true,
	}
	if forks[0] != want {
		t.Errorf("forks[0] = %+v\nwant %+v", forks[0], want)
	}
	if forks[1].HasParent || forks[1].DefaultBranch != "" || forks[1].Unresolved {
		t.Errorf("forks[1] = %+v, want a resolved fork with no parent", forks[1])
	}
	if forks[2].HeadOID != forks[2].ParentHeadOID {
		t.Errorf("forks[2] should be in sync: %+v", forks[2])
	}

	// Progress is reported per list page and per detail batch, ending complete.
	if !slices.Equal(listed, []int{2, 3}) {
		t.Errorf("OnListed reported %v, want [2 3]", listed)
	}
	if len(detailed) == 0 || detailed[len(detailed)-1] != [2]int{3, 3} {
		t.Errorf("OnDetail ended at %v, want [3 3] last", detailed)
	}
}

func TestListForksMarksVanishedRepoUnresolved(t *testing.T) {
	srv := &discoveryServer{
		pages: [][]string{{"tempo", "renamed"}},
		detail: map[string]string{
			"tempo": detailJSON("main", "aaa", "grafana/tempo", "main", "bbb"),
		},
	}
	c, _ := newTestClient(t, srv)

	forks, err := c.ListForks(context.Background(), "acme")
	if err != nil {
		t.Fatalf("ListForks: %v", err)
	}
	if len(forks) != 2 {
		t.Fatalf("got %d forks, want 2", len(forks))
	}
	if forks[0].Unresolved || !forks[0].HasParent {
		t.Errorf("forks[0] = %+v, want fully resolved", forks[0])
	}
	if !forks[1].Unresolved {
		t.Errorf("forks[1] = %+v, want Unresolved: a NOT_FOUND alias must not fail the run", forks[1])
	}
}

func TestListForksFailsOnDetailError(t *testing.T) {
	var calls int
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		if err := decodeJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if strings.Contains(req.Query, "repositories(") {
			fmt.Fprint(w, `{"data":{"organization":{"repositories":{
				"pageInfo":{"hasNextPage":false,"endCursor":""},
				"nodes":[{"name":"tempo","nameWithOwner":"acme/tempo","isArchived":false,"viewerPermission":"WRITE"}]}}}}`)
			return
		}
		calls++
		fmt.Fprint(w, `{"data":{"r0":null},"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`)
	}))

	_, err := c.ListForks(context.Background(), "acme")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Message != "API rate limit exceeded" {
		t.Fatalf("error = %v, want the GraphQL message verbatim", err)
	}
	if calls != 1 {
		t.Errorf("detail query ran %d times, want 1: a GraphQL error is not retried", calls)
	}
}

func TestListForksGraphQLErrors(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"organization":null},"errors":[{"message":"Could not resolve to an Organization"}]}`)
	}))
	_, err := c.ListForks(context.Background(), "ghost")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Message != "Could not resolve to an Organization" {
		t.Fatalf("error = %v, want the GraphQL message verbatim", err)
	}
}

func TestDetailPlanCoversEveryForkInFewWaves(t *testing.T) {
	for _, n := range []int{1, 3, 9, 46, 101, 250, 1000} {
		batch, workers := detailPlan(n)
		switch {
		case batch < minDetailBatch || batch > maxDetailBatch:
			t.Errorf("n=%d: batch = %d, want within [%d %d]", n, batch, minDetailBatch, maxDetailBatch)
		case workers < 1 || workers > detailWorkers:
			t.Errorf("n=%d: workers = %d, want within [1 %d]", n, workers, detailWorkers)
		}
		batches := (n + batch - 1) / batch
		if batches*batch < n {
			t.Errorf("n=%d: %d batches of %d do not cover it", n, batches, batch)
		}
		// One batch per worker is the whole point: waves cost a round trip each.
		if waves := (batches + workers - 1) / workers; n <= detailWorkers*maxDetailBatch && waves != 1 {
			t.Errorf("n=%d: %d waves of %d batches, want 1", n, waves, batches)
		}
	}
}

func TestListForksEmptyOrgSkipsDetailPhase(t *testing.T) {
	var queries int
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries++
		fmt.Fprint(w, `{"data":{"organization":{"repositories":{
			"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[]}}}}`)
	}))
	forks, err := c.ListForks(context.Background(), "acme")
	if err != nil {
		t.Fatalf("ListForks: %v", err)
	}
	if len(forks) != 0 {
		t.Fatalf("got %d forks, want 0", len(forks))
	}
	if queries != 1 {
		t.Errorf("%d queries, want 1: nothing to add detail to", queries)
	}
}

func TestContextCancellation(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be reached")
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.GetUser(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestParentOwner(t *testing.T) {
	for in, want := range map[string]string{
		"grafana/tempo": "grafana",
		"nope":          "",
		"":              "",
		"/x":            "",
	} {
		if got := ParentOwner(in); got != want {
			t.Errorf("ParentOwner(%q) = %q, want %q", in, got, want)
		}
	}
}
