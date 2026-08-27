package sync

import (
	"fmt"
	"sort"

	"github.com/naz-ovh/forkman/internal/config"
	"github.com/naz-ovh/forkman/internal/github"
)

// writable permissions that allow pushing to the fork, whether the push goes
// through merge-upstream or plain git.
var writable = map[string]bool{"WRITE": true, "MAINTAIN": true, "ADMIN": true}

// Plan classifies every discovered fork exactly once. The returned order is
// name-ascending and is THE display order for both the TUI and plain output.
//
// kind matters: syncing pushes to the fork, so a repository the viewer cannot
// write to is skipped, and a fork whose head already equals its parent's is
// nothing to do. Cloning only reads, so neither rule applies — every fork with
// a parent is cloned.
func Plan(forks []github.Fork, cfg *config.Config, kind Kind) []Task {
	tasks := make([]Task, 0, len(forks))
	for _, f := range forks {
		t := Task{Fork: f}
		pattern, excluded := "", false
		if cfg != nil {
			pattern, excluded = cfg.Matches(f.Name)
		}
		switch {
		case excluded:
			t.skip("excluded by config",
				fmt.Sprintf("excluded by config pattern %q", pattern),
				"put it back with: forkman configure --exclude-remove "+f.Name)
		case f.Archived:
			t.skip("archived", "archived repositories cannot be pushed to")
		case !f.HasParent:
			t.skip("no parent", "no upstream parent: this repository is not a fork")
		case kind == KindSync && !writable[f.ViewerPermission]:
			perm := f.ViewerPermission
			if perm == "" {
				perm = "UNKNOWN"
			}
			t.skip(fmt.Sprintf("read-only (%s) · cannot push", perm),
				"viewerPermission: "+perm+" — need WRITE to push",
				"ask an org owner for write access, or exclude it: forkman configure --exclude-add "+f.Name)
		case kind == KindSync && f.HeadOID != "" && f.HeadOID == f.ParentHeadOID:
			t.UpToDate = true
			t.Log = []string{fmt.Sprintf("fork %s == upstream %s", short7(f.HeadOID), short7(f.ParentHeadOID))}
		}
		tasks = append(tasks, t)
	}
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].Fork.Name < tasks[j].Fork.Name })
	return tasks
}

// skip marks the task skipped and records the explanation the expanded row
// shows.
func (t *Task) skip(reason string, log ...string) {
	t.Skip, t.SkipReason, t.Log = true, reason, log
}

// short7 abbreviates an object id the way git does.
func short7(oid string) string {
	if len(oid) > 7 {
		return oid[:7]
	}
	if oid == "" {
		return "(unknown)"
	}
	return oid
}
