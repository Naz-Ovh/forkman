// Package github implements the small slice of the GitHub REST and GraphQL
// APIs that forkman needs, using only the standard library.
package github

import "fmt"

// APIError is a non-2xx response from the GitHub API. It never carries the
// token, so it is safe to print.
type APIError struct {
	Status         int
	Message        string
	DocURL         string
	RateLimited    bool
	SecondaryLimit bool
	Scopes         string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("GitHub API %d", e.Status)
	}
	return fmt.Sprintf("GitHub API %d: %s", e.Status, e.Message)
}

// User is the authenticated user plus the scopes the token advertises.
// ScopesKnown is false when the X-OAuth-Scopes header is absent or empty,
// which is what fine-grained PATs do.
type User struct {
	Login       string
	Scopes      []string
	ScopesKnown bool
}

// Fork is one fork discovered in the organization, together with everything
// needed to decide whether it must be synced.
type Fork struct {
	Name             string
	NameWithOwner    string
	Archived         bool
	ViewerPermission string // ADMIN|MAINTAIN|WRITE|TRIAGE|READ

	DefaultBranch string
	HeadOID       string

	ParentNameWithOwner string
	ParentDefaultBranch string
	ParentHeadOID       string
	HasParent           bool

	// Unresolved means GitHub did not return this repository's branch and
	// parent detail — it was renamed or removed while forkman was reading the
	// organization. Nothing can be planned for it, so it is reported rather
	// than quietly treated as a repository without a parent.
	Unresolved bool
}

// Compare is the result of the REST compare endpoint.
type Compare struct {
	AheadBy  int
	BehindBy int
	Status   string // identical|ahead|behind|diverged
}

// MergeResult is the result of POST /repos/{owner}/{repo}/merge-upstream.
type MergeResult struct {
	MergeType  string // "fast-forward"|"merge"|"none"
	Message    string
	BaseBranch string
}
