package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	stdsync "sync"
)

// Fork discovery runs in two phases because GitHub resolves the fields at very
// different speeds. Listing an organization's forks with only their cheap
// fields takes a fraction of a second; adding each fork's default branch and
// its upstream parent to that same query costs seconds, because the parent
// traversal is resolved per repository. Phase two therefore asks for the
// expensive fields in batches that run concurrently, which turns one long
// request into a handful of short ones and lets the caller watch it progress.
const (
	// detailWorkers bounds how many detail queries are in flight at once.
	detailWorkers = 10
	// A detail query costs roughly a fixed second plus a tenth of a second per
	// repository, so the fastest shape is as few waves as possible with
	// batches no larger than they have to be.
	minDetailBatch = 4
	maxDetailBatch = 25
)

const forkListQuery = `query($org: String!, $cursor: String) {
  organization(login: $org) {
    repositories(first: 100, after: $cursor, isFork: true, orderBy: {field: NAME, direction: ASC}) {
      pageInfo { hasNextPage endCursor }
      nodes { name nameWithOwner isArchived viewerPermission }
    }
  }
}`

// detailFields are the per-repository fields phase two resolves.
const detailFields = `{
    defaultBranchRef { name target { oid } }
    parent { nameWithOwner defaultBranchRef { name target { oid } } }
  }`

// detailQuery builds a query that resolves n repositories in one round trip,
// aliased r0…rN-1 and named through variables so no repository name is ever
// interpolated into the query text.
func detailQuery(n int) string {
	var b strings.Builder
	b.WriteString("query($org: String!")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, ", $n%d: String!", i)
	}
	b.WriteString(") {\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "  r%d: repository(owner: $org, name: $n%d) %s\n", i, i, detailFields)
	}
	b.WriteString("}")
	return b.String()
}

type gqlRef struct {
	Name   string `json:"name"`
	Target *struct {
		OID string `json:"oid"`
	} `json:"target"`
}

// gqlDetail is one repository's phase-two payload.
type gqlDetail struct {
	DefaultBranchRef *gqlRef `json:"defaultBranchRef"`
	Parent           *struct {
		NameWithOwner    string  `json:"nameWithOwner"`
		DefaultBranchRef *gqlRef `json:"defaultBranchRef"`
	} `json:"parent"`
}

type gqlError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

type gqlErrors []gqlError

// err joins the messages into one *APIError, or returns nil when the response
// carried no errors. GraphQL reports these with HTTP 200, so status 200 is
// what an error from a GraphQL call looks like.
func (e gqlErrors) err() error {
	if len(e) == 0 {
		return nil
	}
	msgs := make([]string, 0, len(e))
	for _, x := range e {
		msgs = append(msgs, x.Message)
	}
	return &APIError{Status: http.StatusOK, Message: strings.Join(msgs, "; ")}
}

type gqlListResponse struct {
	Data struct {
		Organization *struct {
			Repositories struct {
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				Nodes []struct {
					Name             string `json:"name"`
					NameWithOwner    string `json:"nameWithOwner"`
					IsArchived       bool   `json:"isArchived"`
					ViewerPermission string `json:"viewerPermission"`
				} `json:"nodes"`
			} `json:"repositories"`
		} `json:"organization"`
	} `json:"data"`
	Errors gqlErrors `json:"errors"`
}

type gqlDetailResponse struct {
	Data   map[string]*gqlDetail `json:"data"`
	Errors gqlErrors             `json:"errors"`
}

// graphql posts a query and decodes the reply into out.
func (c *Client) graphql(ctx context.Context, query string, vars map[string]any, out any) error {
	body, err := json.Marshal(struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}{query, vars})
	if err != nil {
		return fmt.Errorf("encode graphql request: %w", err)
	}
	resp, err := c.do(ctx, http.MethodPost, "/graphql", body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(resp.body, out); err != nil {
		return fmt.Errorf("decode graphql response: %w", err)
	}
	return nil
}

// Discovery receives fork-discovery progress so a front-end can show what
// GitHub is spending its time on. Both hooks are optional and are never
// called concurrently.
type Discovery struct {
	// OnListed is called after every page of the fork listing, with the
	// number of forks found so far.
	OnListed func(found int)
	// OnDetail is called as each batch of branch and parent detail lands.
	OnDetail func(done, total int)
}

func (d *Discovery) listed(n int) {
	if d != nil && d.OnListed != nil {
		d.OnListed(n)
	}
}

func (d *Discovery) detail(done, total int) {
	if d != nil && d.OnDetail != nil {
		d.OnDetail(done, total)
	}
}

// ListForks discovers every fork in org, complete with the branch and parent
// detail the planner needs.
func (c *Client) ListForks(ctx context.Context, org string) ([]Fork, error) {
	return c.ListForksProgress(ctx, org, nil)
}

// ListForksProgress is ListForks with progress reporting.
func (c *Client) ListForksProgress(ctx context.Context, org string, d *Discovery) ([]Fork, error) {
	forks, err := c.listForks(ctx, org, d)
	if err != nil {
		return nil, err
	}
	if len(forks) == 0 {
		return forks, nil
	}
	if err := c.addDetail(ctx, org, forks, d); err != nil {
		return nil, err
	}
	return forks, nil
}

// listForks is phase one: every fork in org with only the fields GitHub
// resolves cheaply, 100 at a time.
func (c *Client) listForks(ctx context.Context, org string, d *Discovery) ([]Fork, error) {
	var forks []Fork
	cursor := any(nil)
	for {
		var out gqlListResponse
		vars := map[string]any{"org": org, "cursor": cursor}
		if err := c.graphql(ctx, forkListQuery, vars, &out); err != nil {
			return nil, err
		}
		if err := out.Errors.err(); err != nil {
			return nil, err
		}
		if out.Data.Organization == nil {
			return nil, errEmptyGraphQL
		}
		repos := out.Data.Organization.Repositories
		for _, n := range repos.Nodes {
			forks = append(forks, Fork{
				Name:             n.Name,
				NameWithOwner:    n.NameWithOwner,
				Archived:         n.IsArchived,
				ViewerPermission: n.ViewerPermission,
			})
		}
		d.listed(len(forks))
		if !repos.PageInfo.HasNextPage || repos.PageInfo.EndCursor == "" {
			return forks, nil
		}
		cursor = repos.PageInfo.EndCursor
	}
}

// detailPlan picks the batch size and worker count for n forks. Batches are
// sized to fit in as few concurrent waves as possible, because each query
// carries about a second of fixed cost however few repositories it names.
func detailPlan(n int) (batch, workers int) {
	batch = (n + detailWorkers - 1) / detailWorkers
	switch {
	case batch < minDetailBatch:
		batch = minDetailBatch
	case batch > maxDetailBatch:
		batch = maxDetailBatch
	}
	workers = (n + batch - 1) / batch
	if workers > detailWorkers {
		workers = detailWorkers
	}
	return batch, workers
}

// addDetail is phase two: it fills in each fork's default branch and upstream
// parent, in concurrent batches. forks is updated in place.
func (c *Client) addDetail(ctx context.Context, org string, forks []Fork, d *Discovery) error {
	type span struct{ from, to int }
	batch, workers := detailPlan(len(forks))
	spans := make([]span, 0, (len(forks)+batch-1)/batch)
	for i := 0; i < len(forks); i += batch {
		spans = append(spans, span{i, min(i+batch, len(forks))})
	}

	// A failed batch cancels the rest: the caller gets one error, not a
	// partially resolved plan.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		mu       stdsync.Mutex // guards done, firstErr and the progress hook
		wg       stdsync.WaitGroup
		done     int
		firstErr error
	)
	jobs := make(chan span)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range jobs {
				err := c.detailBatch(runCtx, org, forks[s.from:s.to])
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = err
					cancel()
				}
				done += s.to - s.from
				d.detail(done, len(forks))
				mu.Unlock()
			}
		}()
	}
	for _, s := range spans {
		select {
		case jobs <- s:
		case <-runCtx.Done():
		}
	}
	close(jobs)
	wg.Wait()

	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

// detailBatch resolves one batch of repositories and writes the result back
// into forks.
func (c *Client) detailBatch(ctx context.Context, org string, forks []Fork) error {
	vars := make(map[string]any, len(forks)+1)
	vars["org"] = org
	for i, f := range forks {
		vars[fmt.Sprintf("n%d", i)] = f.Name
	}
	var out gqlDetailResponse
	if err := c.graphql(ctx, detailQuery(len(forks)), vars, &out); err != nil {
		return err
	}
	// A repository that no longer resolves — renamed or deleted since the
	// listing — comes back as a null alias plus a NOT_FOUND error, and is
	// reported as one unresolved row rather than failing the whole discovery.
	// Any other error means the query itself failed.
	for _, e := range out.Errors {
		if e.Type != "NOT_FOUND" {
			return out.Errors.err()
		}
	}
	if out.Data == nil {
		return errEmptyGraphQL
	}
	for i := range forks {
		det, ok := out.Data[fmt.Sprintf("r%d", i)]
		if !ok || det == nil {
			forks[i].Unresolved = true
			continue
		}
		applyDetail(&forks[i], det)
	}
	return nil
}

func applyDetail(f *Fork, d *gqlDetail) {
	if d.DefaultBranchRef != nil {
		f.DefaultBranch = d.DefaultBranchRef.Name
		if d.DefaultBranchRef.Target != nil {
			f.HeadOID = d.DefaultBranchRef.Target.OID
		}
	}
	if d.Parent != nil {
		f.HasParent = true
		f.ParentNameWithOwner = d.Parent.NameWithOwner
		if d.Parent.DefaultBranchRef != nil {
			f.ParentDefaultBranch = d.Parent.DefaultBranchRef.Name
			if d.Parent.DefaultBranchRef.Target != nil {
				f.ParentHeadOID = d.Parent.DefaultBranchRef.Target.OID
			}
		}
	}
}

// ParentOwner splits "owner/name" and returns owner.
func ParentOwner(nameWithOwner string) string {
	if i := strings.IndexByte(nameWithOwner, '/'); i > 0 {
		return nameWithOwner[:i]
	}
	return ""
}
