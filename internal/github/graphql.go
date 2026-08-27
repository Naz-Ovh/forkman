package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const forksQuery = `query($org: String!, $cursor: String) {
  organization(login: $org) {
    repositories(first: 100, after: $cursor, isFork: true, orderBy: {field: NAME, direction: ASC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        name
        nameWithOwner
        isArchived
        viewerPermission
        defaultBranchRef { name target { oid } }
        parent {
          nameWithOwner
          defaultBranchRef { name target { oid } }
        }
      }
    }
  }
}`

type gqlRef struct {
	Name   string `json:"name"`
	Target *struct {
		OID string `json:"oid"`
	} `json:"target"`
}

type gqlNode struct {
	Name             string  `json:"name"`
	NameWithOwner    string  `json:"nameWithOwner"`
	IsArchived       bool    `json:"isArchived"`
	ViewerPermission string  `json:"viewerPermission"`
	DefaultBranchRef *gqlRef `json:"defaultBranchRef"`
	Parent           *struct {
		NameWithOwner    string  `json:"nameWithOwner"`
		DefaultBranchRef *gqlRef `json:"defaultBranchRef"`
	} `json:"parent"`
}

type gqlForksResponse struct {
	Data struct {
		Organization *struct {
			Repositories struct {
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				Nodes []gqlNode `json:"nodes"`
			} `json:"repositories"`
		} `json:"organization"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"errors"`
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

// ListForks discovers every fork in org, 100 per GraphQL call, following
// pageInfo until exhausted.
func (c *Client) ListForks(ctx context.Context, org string) ([]Fork, error) {
	var forks []Fork
	cursor := any(nil)
	for {
		var out gqlForksResponse
		vars := map[string]any{"org": org, "cursor": cursor}
		if err := c.graphql(ctx, forksQuery, vars, &out); err != nil {
			return nil, err
		}
		if len(out.Errors) > 0 {
			msgs := make([]string, 0, len(out.Errors))
			for _, e := range out.Errors {
				msgs = append(msgs, e.Message)
			}
			return nil, &APIError{Status: http.StatusOK, Message: strings.Join(msgs, "; ")}
		}
		if out.Data.Organization == nil {
			return nil, errEmptyGraphQL
		}
		repos := out.Data.Organization.Repositories
		for _, n := range repos.Nodes {
			forks = append(forks, forkFromNode(n))
		}
		if !repos.PageInfo.HasNextPage || repos.PageInfo.EndCursor == "" {
			return forks, nil
		}
		cursor = repos.PageInfo.EndCursor
	}
}

func forkFromNode(n gqlNode) Fork {
	f := Fork{
		Name:             n.Name,
		NameWithOwner:    n.NameWithOwner,
		Archived:         n.IsArchived,
		ViewerPermission: n.ViewerPermission,
	}
	if n.DefaultBranchRef != nil {
		f.DefaultBranch = n.DefaultBranchRef.Name
		if n.DefaultBranchRef.Target != nil {
			f.HeadOID = n.DefaultBranchRef.Target.OID
		}
	}
	if n.Parent != nil {
		f.HasParent = true
		f.ParentNameWithOwner = n.Parent.NameWithOwner
		if n.Parent.DefaultBranchRef != nil {
			f.ParentDefaultBranch = n.Parent.DefaultBranchRef.Name
			if n.Parent.DefaultBranchRef.Target != nil {
				f.ParentHeadOID = n.Parent.DefaultBranchRef.Target.OID
			}
		}
	}
	return f
}

// ParentOwner splits "owner/name" and returns owner.
func ParentOwner(nameWithOwner string) string {
	if i := strings.IndexByte(nameWithOwner, '/'); i > 0 {
		return nameWithOwner[:i]
	}
	return ""
}
