package main

import (
	"fmt"
	"os"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/contextblock"
)

// checkContextSync reports the state of the managed context block, if any.
//
// The contract it enforces is the one the block's begin marker publishes:
// edits inside the markers are overwritten by design. A hand edit is therefore
// a warning — the user should know the next sync will replace their words and
// that a .bak keeps the previous file — never a failure, and never silence.
// No block at all is an info: the whole feature is opt-in.
func checkContextSync(projectDir string) checkResult {
	c := checkResult{Name: "context block"}

	var path string
	var content string
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		p := projectDir + string(os.PathSeparator) + name
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if _, _, _, found := contextblock.Parse(string(data)); found {
			path, content = p, string(data)
			break
		}
	}
	if path == "" {
		c.Status = "info"
		c.Detail = "no managed context block in AGENTS.md / CLAUDE.md"
		c.Hint = "optional: `graymatter context-sync` projects the highest-weight facts into a budgeted block your agent reads every session"
		return c
	}

	body, meta, verified, _ := contextblock.Parse(content)
	_ = body

	switch {
	case !verified:
		c.Status = "warn"
		c.Detail = fmt.Sprintf("%s was edited by hand since the last sync", path)
		c.Hint = "the next `graymatter context-sync` will overwrite the managed block (previous file is kept as .bak); re-run to accept, or move your edits outside the markers"
	case time.Since(meta.SyncedAt) > 30*24*time.Hour:
		c.Status = "info"
		c.Detail = fmt.Sprintf("%s: block verified but not synced in over 30 days (%d fact(s))", path, meta.Facts)
		c.Hint = "re-run `graymatter context-sync` to refresh the projection"
	default:
		c.Status = "ok"
		c.Detail = fmt.Sprintf("%s: %d fact(s), ~%s", path, meta.Facts, syncedWhen(meta.SyncedAt))
	}
	return c
}

func syncedWhen(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return "synced just now"
	case d < 24*time.Hour:
		return fmt.Sprintf("synced %dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("synced %dd ago", int(d.Hours()/24))
	}
}
