package docaudit

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// blameDates returns one commit timestamp per line of path. A zero timestamp
// marks a line with no commit yet (uncommitted working-tree content).
//
// The second return explains unavailability instead of hiding it: git
// missing, directory outside a repository, or an untracked file each produce
// Available=false with a stated reason, which the report prints verbatim.
// Staleness that cannot be measured honestly is reported as unmeasured.
func blameDates(path string) (dates []time.Time, reason string, err error) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, "git not found on PATH", nil
	}
	dir := filepath.Dir(path)

	if out, err := run(dir, "rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(out) != "true" {
		return nil, "not a git repository", nil
	}
	if out, err := run(dir, "ls-files", "--error-unmatch", filepath.Base(path)); err != nil || !strings.Contains(out, filepath.Base(path)) {
		return nil, "file is not tracked by git", nil
	}

	out, err := run(dir, "blame", "--line-porcelain", "--", filepath.Base(path))
	if err != nil {
		return nil, "git blame failed", err
	}

	var (
		cur     time.Time
		curZero bool
		haveCur bool
	)
	for _, ln := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(ln, "\t"):
			// Content line: the pending header belongs to it. Uncommitted
			// lines carry the all-zero SHA and NO committer-time, so they must
			// fall back to the zero timestamp here — inheriting the previous
			// line's commit would misfile brand-new text as ancient.
			switch {
			case !haveCur:
			case curZero:
				dates = append(dates, time.Time{})
			default:
				dates = append(dates, cur)
			}
			cur, curZero, haveCur = time.Time{}, false, false
		case isShaHeader(ln):
			curZero = strings.HasPrefix(ln, "0000000000000000000000000000000000000000")
			haveCur = true
		case strings.HasPrefix(ln, "committer-time ") && !curZero:
			if sec, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(ln, "committer-time ")), 10, 64); err == nil {
				cur = time.Unix(sec, 0).UTC()
			}
		}
	}
	return dates, "", nil
}

func isShaHeader(line string) bool {
	f := strings.Fields(line)
	if len(f) < 3 {
		return false
	}
	sha := f[0]
	if len(sha) != 40 {
		return false
	}
	for _, r := range sha {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	for _, num := range f[1:] {
		if _, err := strconv.Atoi(num); err != nil {
			return false
		}
	}
	return true
}

func run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	return string(out), err
}
