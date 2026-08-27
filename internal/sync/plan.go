package sync

import (
	"sort"

	"forkman/internal/config"
	"forkman/internal/github"
)

// writable permissions that allow merge-upstream.
var writable = map[string]bool{"WRITE": true, "MAINTAIN": true, "ADMIN": true}

// Plan classifies every discovered fork exactly once. The returned order is
// name-ascending and is THE display order for both the TUI and plain output.
func Plan(forks []github.Fork, cfg *config.Config) []Task {
	tasks := make([]Task, 0, len(forks))
	for _, f := range forks {
		t := Task{Fork: f}
		switch {
		case cfg != nil && cfg.IsExcluded(f.Name):
			t.Skip, t.SkipReason = true, "excluded by config"
		case f.Archived:
			t.Skip, t.SkipReason = true, "archived"
		case !f.HasParent:
			t.Skip, t.SkipReason = true, "no parent"
		case !writable[f.ViewerPermission]:
			t.Skip, t.SkipReason = true, "insufficient permission"
		case f.HeadOID != "" && f.HeadOID == f.ParentHeadOID:
			t.UpToDate = true
		}
		tasks = append(tasks, t)
	}
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].Fork.Name < tasks[j].Fork.Name })
	return tasks
}
