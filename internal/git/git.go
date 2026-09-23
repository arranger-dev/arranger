// Package git wraps the git CLI for arranger: one worktree and branch per agent, checkpoint
// commits, diffs split into hunks, and merging work between branches.
package git

import (
	"crypto/sha1"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Run runs git in dir and returns its combined output.
func Run(dir string, args ...string) (string, error) {
	return RunIn(dir, "", args...)
}

// RunIn runs git in dir with stdin as its input.
func RunIn(dir, stdin string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// CheckRepo verifies repo is a git work tree and returns base, defaulting to the current branch.
func CheckRepo(repo, base string) (string, error) {
	if _, err := Run(repo, "rev-parse", "--show-toplevel"); err != nil {
		return "", fmt.Errorf("%s is not a git repository", repo)
	}
	// after a fresh `git init` HEAD names a branch with no commit yet: there's nothing to branch agents from
	if _, err := Run(repo, "rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		if out, _ := Run(repo, "rev-list", "-n1", "--all"); strings.TrimSpace(out) == "" {
			return "", fmt.Errorf(`%s has no commits yet. Make a first commit (e.g. git commit --allow-empty -m "initial commit") and save again`, repo)
		}
		if base == "" {
			return "", fmt.Errorf("the current branch of %s has no commits yet; enter a base branch that does", repo)
		}
	}
	if base == "" {
		out, err := Run(repo, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return "", err
		}
		base = strings.TrimSpace(out)
	}
	if _, err := Run(repo, "rev-parse", "--verify", "--quiet", base+"^{commit}"); err != nil {
		return "", fmt.Errorf("base %q not found in %s", base, repo)
	}
	return base, nil
}

// identity lets arranger commit without depending on the user's git config.
var identity = []string{"-c", "user.name=arranger", "-c", "user.email=arranger@localhost"}

var shaRe = regexp.MustCompile(`^[0-9a-f]{4,40}$`)

// IsSHA reports whether s looks like a (possibly abbreviated) commit id.
func IsSHA(s string) bool { return shaRe.MatchString(s) }

// BranchOf is the branch an agent works on.
func BranchOf(agentID string) string { return "arranger/" + agentID }

// BranchExists reports whether a local branch exists.
func BranchExists(repo, branch string) bool {
	_, err := Run(repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// EnsureWorktree returns the agent's worktree, creating it on branch arranger/<agent> from base.
func EnsureWorktree(root, repo, base, agentID string) (string, error) {
	dir := filepath.Join(root, agentID)
	if _, err := os.Stat(dir); err == nil {
		return dir, nil
	}
	branch := BranchOf(agentID)
	args := []string{"worktree", "add", "-b", branch, dir, base}
	if _, err := Run(repo, "rev-parse", "--verify", "--quiet", branch); err == nil {
		args = []string{"worktree", "add", dir, branch} // branch survived a deleted worktree
	}
	Run(repo, "worktree", "prune")
	_, err := Run(repo, args...)
	return dir, err
}

// Commit commits everything in the worktree. Returns "" when there was nothing to commit.
func Commit(dir, msg string) (string, error) {
	if _, err := Run(dir, "add", "-A"); err != nil {
		return "", err
	}
	if _, err := Run(dir, "diff", "--cached", "--quiet"); err == nil {
		return "", nil
	}
	if _, err := Run(dir, append(identity, "commit", "--no-verify", "-q", "-m", msg)...); err != nil {
		return "", err
	}
	out, err := Run(dir, "rev-parse", "--short", "HEAD")
	return strings.TrimSpace(out), err
}

type Hunk struct {
	ID     string   `json:"id"`
	Header string   `json:"header"`
	Lines  []string `json:"lines"`
}

type FileDiff struct {
	Path   string   `json:"path"`
	Header []string `json:"-"` // "diff --git", "---", "+++" ... lines, needed to rebuild a patch
	Adds   int      `json:"adds"`
	Dels   int      `json:"dels"`
	Hunks  []Hunk   `json:"hunks"`
}

// RawDiff is everything the agent changed since it branched off base, committed or not.
// ponytail: new files show once they're checkpointed (untracked files aren't in git diff).
func RawDiff(dir, base string) (string, error) {
	mb, err := Run(dir, "merge-base", base, "HEAD")
	if err != nil {
		return "", err
	}
	return Run(dir, "diff", "--no-color", "--no-ext-diff", strings.TrimSpace(mb))
}

// Diff is RawDiff split into files and hunks.
func Diff(dir, base string) ([]FileDiff, error) {
	out, err := RawDiff(dir, base)
	if err != nil {
		return nil, err
	}
	return ParseDiff(out), nil
}

// ParseDiff splits unified diff output into files and hunks.
func ParseDiff(s string) []FileDiff {
	fs := []FileDiff{}
	var f *FileDiff
	var h *Hunk
	for _, l := range strings.Split(s, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			fs = append(fs, FileDiff{Path: strings.TrimPrefix(l[strings.LastIndex(l, " b/")+1:], "b/"), Header: []string{l}})
			f, h = &fs[len(fs)-1], nil
		case f == nil, l == "":
		case strings.HasPrefix(l, "@@"):
			sum := sha1.Sum([]byte(f.Path + l))
			f.Hunks = append(f.Hunks, Hunk{ID: fmt.Sprintf("%x", sum[:4]), Header: l})
			h = &f.Hunks[len(f.Hunks)-1]
		case h == nil:
			f.Header = append(f.Header, l)
			if strings.HasPrefix(l, "+++ b/") {
				f.Path = l[6:]
			}
		default:
			h.Lines = append(h.Lines, l)
			if strings.HasPrefix(l, "+") {
				f.Adds++
			} else if strings.HasPrefix(l, "-") {
				f.Dels++
			}
		}
	}
	return fs
}

// Stat totals lines added and removed.
func Stat(fs []FileDiff) (adds, dels int) {
	for _, f := range fs {
		adds += f.Adds
		dels += f.Dels
	}
	return
}

// HunkPatch builds a patch holding only the hunks whose ids are in ids.
func HunkPatch(fs []FileDiff, ids map[string]bool) (patch string, n int) {
	var b strings.Builder
	for _, f := range fs {
		head := false
		for _, h := range f.Hunks {
			if !ids[h.ID] {
				continue
			}
			if !head {
				b.WriteString(strings.Join(f.Header, "\n") + "\n")
				head = true
			}
			b.WriteString(h.Header + "\n" + strings.Join(h.Lines, "\n") + "\n")
			n++
		}
	}
	return b.String(), n
}

// ApplyHunks copies the selected hunks into the worktree at dir and commits nothing: the caller
// commits. Hunks dir already has are skipped (present); the rest apply as a patch, or as a
// three-way merge when dir's files have moved on. On a conflict dir is left as it was.
func ApplyHunks(dir string, fs []FileDiff, ids map[string]bool) (applied, present int, err error) {
	if _, err := Commit(dir, "arranger: save work before applying changes"); err != nil {
		return 0, 0, err
	}
	todo := map[string]bool{}
	for id := range ids {
		p, n := HunkPatch(fs, map[string]bool{id: true})
		if n == 0 {
			continue
		}
		if _, err := RunIn(dir, p, "apply", "-R", "--check", "-"); err == nil {
			present++ // undoing it would apply cleanly, so it's already there
			continue
		}
		todo[id] = true
	}
	if len(todo) == 0 {
		return 0, present, nil
	}
	patch, n := HunkPatch(fs, todo)
	if _, err := RunIn(dir, patch, "apply", "--check", "-"); err == nil {
		_, err = RunIn(dir, patch, "apply", "-")
		return n, present, err
	}
	if _, err := RunIn(dir, patch, "apply", "--3way", "-"); err != nil {
		out, _ := Run(dir, "diff", "--name-only", "--diff-filter=U")
		Run(dir, "reset", "-q", "--hard", "HEAD") // drop the half-applied patch; work was committed above
		if cs := lines(out); len(cs) > 0 {
			return 0, present, fmt.Errorf("conflicts in %s", strings.Join(cs, ", "))
		}
		return 0, present, err
	}
	return n, present, nil
}

type Checkpoint struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
	Time    int64  `json:"time"`
}

// Checkpoints lists the commits on the agent's branch since base, newest first.
func Checkpoints(dir, base string) ([]Checkpoint, error) {
	mb, err := Run(dir, "merge-base", base, "HEAD")
	if err != nil {
		return nil, err
	}
	out, err := Run(dir, "log", "--format=%h%x1f%s%x1f%ct", strings.TrimSpace(mb)+"..HEAD")
	if err != nil {
		return nil, err
	}
	cs := []Checkpoint{}
	for _, l := range lines(out) {
		if p := strings.Split(l, "\x1f"); len(p) == 3 {
			var t int64
			fmt.Sscan(p[2], &t)
			cs = append(cs, Checkpoint{SHA: p[0], Subject: p[1], Time: t})
		}
	}
	return cs, nil
}

// Revert undoes one checkpoint with a new commit; a conflicting revert is abandoned.
func Revert(dir, sha string) error {
	if _, err := Commit(dir, "arranger: save work before revert"); err != nil {
		return err
	}
	if _, err := Run(dir, append(identity, "revert", "--no-edit", sha)...); err != nil {
		Run(dir, "revert", "--abort")
		return err
	}
	return nil
}

// MergeBranch merges branch into the worktree at dir; a conflicting merge is abandoned.
func MergeBranch(dir, branch, msg string) error {
	if _, err := Commit(dir, "arranger: save work before merge"); err != nil {
		return err
	}
	if _, err := Run(dir, append(identity, "merge", "--no-ff", "-m", msg, branch)...); err != nil {
		Run(dir, "merge", "--abort")
		return err
	}
	return nil
}

// Behind counts the commits on branch that the worktree at dir doesn't have yet.
func Behind(dir, branch string) int {
	out, err := Run(dir, "rev-list", "--count", "HEAD.."+branch)
	if err != nil {
		return 0
	}
	var n int
	fmt.Sscan(out, &n)
	return n
}

// Sync merges branch into the worktree at dir so the agent works on the latest code, and returns
// how many new commits it brought in. A conflicting merge is abandoned and the conflicts named.
func Sync(dir, branch string) (int, error) {
	if _, err := Commit(dir, "arranger: save work before sync"); err != nil {
		return 0, err
	}
	n := Behind(dir, branch)
	if n == 0 {
		return 0, nil
	}
	if _, err := Run(dir, append(identity, "merge", "--no-edit", "-m", "arranger: sync with "+branch, branch)...); err != nil {
		out, _ := Run(dir, "diff", "--name-only", "--diff-filter=U")
		Run(dir, "merge", "--abort")
		if cs := lines(out); len(cs) > 0 {
			return 0, fmt.Errorf("%s conflicts with this agent's work in %s", branch, strings.Join(cs, ", "))
		}
		return 0, err
	}
	return n, nil
}

// CheckedOutAt returns the worktree that has branch checked out, or "".
func CheckedOutAt(repo, branch string) string {
	out, _ := Run(repo, "worktree", "list", "--porcelain")
	path := ""
	for _, l := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(l, "worktree "); ok {
			path = p
		}
		if l == "branch refs/heads/"+branch {
			return path
		}
	}
	return ""
}

// DefaultTarget is main or master, whichever exists (main when neither does).
func DefaultTarget(repo string) string {
	for _, b := range []string{"main", "master"} {
		if BranchExists(repo, b) {
			return b
		}
	}
	return "main"
}

type MergePreview struct {
	Source     string   `json:"source"`
	Target     string   `json:"target"`
	Exists     bool     `json:"exists"`     // false: it will be created from Start
	Start      string   `json:"start"`      // what a new target branch starts from
	CheckedOut string   `json:"checkedOut"` // worktree where target is checked out, if any
	Dirty      bool     `json:"dirty"`      // that worktree has uncommitted changes
	Commits    int      `json:"commits"`
	Files      int      `json:"files"`
	Adds       int      `json:"adds"`
	Dels       int      `json:"dels"`
	CanFF      bool     `json:"canFF"`
	Conflicts  []string `json:"conflicts"`
}

// PreviewMerge reports what merging source into target would do, without changing anything.
func PreviewMerge(repo, source, target, start string) (MergePreview, error) {
	m := MergePreview{Source: source, Target: target, Start: start, Exists: BranchExists(repo, target), Conflicts: []string{}}
	into := target
	if !m.Exists {
		into = start
	}
	if m.CheckedOut = CheckedOutAt(repo, target); m.CheckedOut != "" {
		out, _ := Run(m.CheckedOut, "status", "--porcelain", "--untracked-files=no")
		m.Dirty = strings.TrimSpace(out) != ""
	}
	out, err := Run(repo, "rev-list", "--count", into+".."+source)
	if err != nil {
		return m, err
	}
	fmt.Sscan(out, &m.Commits)
	_, err = Run(repo, "merge-base", "--is-ancestor", into, source)
	m.CanFF = err == nil
	// What the merge would change is the difference between the target and the merged tree;
	// that also reads "nothing" after a squash, which leaves the commits unmerged but the content in.
	merged, err := Run(repo, "merge-tree", "--write-tree", "--name-only", "--no-messages", into, source)
	ls := lines(merged)
	if err != nil {
		if len(ls) < 2 {
			return m, err
		}
		m.Conflicts = ls[1:] // first line is the tree id
		out, _ = Run(repo, "diff", "--numstat", into+"..."+source)
	} else {
		out, _ = Run(repo, "diff", "--numstat", into, ls[0])
	}
	for _, l := range lines(out) {
		var a, d int
		fmt.Sscan(l, &a, &d) // binary files show "-" and count as 0
		m.Files, m.Adds, m.Dels = m.Files+1, m.Adds+a, m.Dels+d
	}
	return m, nil
}

// MergeInto merges source into target with strategy merge|squash|ff, creating target from start
// if needed. It works in target's own checkout when that is clean, else in a temporary one.
func MergeInto(repo, source, target, start, strategy, msg string) (sha string, err error) {
	if !BranchExists(repo, target) {
		if _, err := Run(repo, "branch", target, start); err != nil {
			return "", err
		}
		defer func() {
			if err != nil {
				Run(repo, "branch", "-D", target) // don't leave behind a branch for a merge that didn't happen
			}
		}()
	}
	dir := CheckedOutAt(repo, target)
	if dir != "" {
		if out, _ := Run(dir, "status", "--porcelain", "--untracked-files=no"); strings.TrimSpace(out) != "" {
			return "", fmt.Errorf("%s has uncommitted changes in %s; commit or stash them first", target, dir)
		}
	} else {
		tmp, err := os.MkdirTemp("", "arranger-merge-")
		if err != nil {
			return "", err
		}
		os.Remove(tmp) // git worktree add wants to create it
		if _, err := Run(repo, "worktree", "add", "-q", tmp, target); err != nil {
			return "", err
		}
		defer Run(repo, "worktree", "remove", "--force", tmp)
		dir = tmp
	}
	var who []string // the user's own identity when git has one, else arranger's
	if out, _ := Run(dir, "config", "user.email"); strings.TrimSpace(out) == "" {
		who = identity
	}
	switch strategy {
	case "merge":
		_, err = Run(dir, append(who, "merge", "--no-ff", "-m", msg, source)...)
	case "squash":
		if _, err = Run(dir, "merge", "--squash", source); err == nil {
			_, err = Run(dir, append(who, "commit", "-q", "-m", msg)...)
		}
	case "ff":
		_, err = Run(dir, "merge", "--ff-only", source)
	default:
		return "", fmt.Errorf("unknown strategy %q", strategy)
	}
	if err != nil {
		Run(dir, "reset", "--merge") // back to where target was
		return "", err
	}
	out, err := Run(dir, "rev-parse", "--short", "HEAD")
	return strings.TrimSpace(out), err
}

// lines returns the non-empty trimmed lines of s.
func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
