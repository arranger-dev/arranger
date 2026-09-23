package main

import (
	"crypto/sha1"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func git(dir string, args ...string) (string, error) {
	return gitIn(dir, "", args...)
}

// gitIn runs git in dir with stdin as its input.
func gitIn(dir, stdin string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// checkRepo verifies repo is a git work tree and returns base, defaulting to the current branch.
func checkRepo(repo, base string) (string, error) {
	if _, err := git(repo, "rev-parse", "--show-toplevel"); err != nil {
		return "", fmt.Errorf("%s is not a git repository", repo)
	}
	if base == "" {
		out, err := git(repo, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return "", err
		}
		base = strings.TrimSpace(out)
	}
	if _, err := git(repo, "rev-parse", "--verify", "--quiet", base+"^{commit}"); err != nil {
		return "", fmt.Errorf("base %q not found in %s", base, repo)
	}
	return base, nil
}

// identity lets arranger commit without depending on the user's git config.
var identity = []string{"-c", "user.name=arranger", "-c", "user.email=arranger@localhost"}

func branchOf(agentID string) string { return "arranger/" + agentID }

func branchExists(repo, branch string) bool {
	_, err := git(repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// ensureWorktree returns the agent's worktree, creating it on branch arranger/<agent> from base.
func ensureWorktree(root, repo, base, agentID string) (string, error) {
	dir := filepath.Join(root, agentID)
	if _, err := os.Stat(dir); err == nil {
		return dir, nil
	}
	branch := branchOf(agentID)
	args := []string{"worktree", "add", "-b", branch, dir, base}
	if _, err := git(repo, "rev-parse", "--verify", "--quiet", branch); err == nil {
		args = []string{"worktree", "add", dir, branch} // branch survived a deleted worktree
	}
	git(repo, "worktree", "prune")
	_, err := git(repo, args...)
	return dir, err
}

// checkpoint commits everything in the worktree. Returns "" when there was nothing to commit.
func checkpoint(dir, msg string) (string, error) {
	if _, err := git(dir, "add", "-A"); err != nil {
		return "", err
	}
	if _, err := git(dir, "diff", "--cached", "--quiet"); err == nil {
		return "", nil
	}
	if _, err := git(dir, append(identity, "commit", "--no-verify", "-q", "-m", msg)...); err != nil {
		return "", err
	}
	out, err := git(dir, "rev-parse", "--short", "HEAD")
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

// rawDiff is everything the agent changed since it branched off base, committed or not.
// ponytail: new files show once they're checkpointed (untracked files aren't in git diff).
func rawDiff(dir, base string) (string, error) {
	mb, err := git(dir, "merge-base", base, "HEAD")
	if err != nil {
		return "", err
	}
	return git(dir, "diff", "--no-color", "--no-ext-diff", strings.TrimSpace(mb))
}

func worktreeDiff(dir, base string) ([]FileDiff, error) {
	out, err := rawDiff(dir, base)
	if err != nil {
		return nil, err
	}
	return parseDiff(out), nil
}

func parseDiff(s string) []FileDiff {
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

func diffStat(fs []FileDiff) (adds, dels int) {
	for _, f := range fs {
		adds += f.Adds
		dels += f.Dels
	}
	return
}

// hunkPatch builds a patch holding only the hunks whose ids are in ids.
func hunkPatch(fs []FileDiff, ids map[string]bool) (patch string, n int) {
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

type Checkpoint struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
	Time    int64  `json:"time"`
}

// checkpoints lists the commits on the agent's branch since base, newest first.
func checkpoints(dir, base string) ([]Checkpoint, error) {
	mb, err := git(dir, "merge-base", base, "HEAD")
	if err != nil {
		return nil, err
	}
	out, err := git(dir, "log", "--format=%h%x1f%s%x1f%ct", strings.TrimSpace(mb)+"..HEAD")
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

// revertCommit undoes one checkpoint with a new commit; a conflicting revert is abandoned.
func revertCommit(dir, sha string) error {
	if _, err := checkpoint(dir, "arranger: save work before revert"); err != nil {
		return err
	}
	if _, err := git(dir, append(identity, "revert", "--no-edit", sha)...); err != nil {
		git(dir, "revert", "--abort")
		return err
	}
	return nil
}

// mergeBranch merges branch into the worktree at dir; a conflicting merge is abandoned.
func mergeBranch(dir, branch, msg string) error {
	if _, err := checkpoint(dir, "arranger: save work before merge"); err != nil {
		return err
	}
	if _, err := git(dir, append(identity, "merge", "--no-ff", "-m", msg, branch)...); err != nil {
		git(dir, "merge", "--abort")
		return err
	}
	return nil
}

// checkedOutAt returns the worktree that has branch checked out, or "".
func checkedOutAt(repo, branch string) string {
	out, _ := git(repo, "worktree", "list", "--porcelain")
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

// defaultTarget is main or master, whichever exists (main when neither does).
func defaultTarget(repo string) string {
	for _, b := range []string{"main", "master"} {
		if branchExists(repo, b) {
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

// previewMerge reports what merging source into target would do, without changing anything.
func previewMerge(repo, source, target, start string) (MergePreview, error) {
	m := MergePreview{Source: source, Target: target, Start: start, Exists: branchExists(repo, target), Conflicts: []string{}}
	into := target
	if !m.Exists {
		into = start
	}
	if m.CheckedOut = checkedOutAt(repo, target); m.CheckedOut != "" {
		out, _ := git(m.CheckedOut, "status", "--porcelain", "--untracked-files=no")
		m.Dirty = strings.TrimSpace(out) != ""
	}
	out, err := git(repo, "rev-list", "--count", into+".."+source)
	if err != nil {
		return m, err
	}
	fmt.Sscan(out, &m.Commits)
	_, err = git(repo, "merge-base", "--is-ancestor", into, source)
	m.CanFF = err == nil
	// What the merge would change is the difference between the target and the merged tree;
	// that also reads "nothing" after a squash, which leaves the commits unmerged but the content in.
	merged, err := git(repo, "merge-tree", "--write-tree", "--name-only", "--no-messages", into, source)
	ls := lines(merged)
	if err != nil {
		if len(ls) < 2 {
			return m, err
		}
		m.Conflicts = ls[1:] // first line is the tree id
		out, _ = git(repo, "diff", "--numstat", into+"..."+source)
	} else {
		out, _ = git(repo, "diff", "--numstat", into, ls[0])
	}
	for _, l := range lines(out) {
		var a, d int
		fmt.Sscan(l, &a, &d) // binary files show "-" and count as 0
		m.Files, m.Adds, m.Dels = m.Files+1, m.Adds+a, m.Dels+d
	}
	return m, nil
}

// mergeInto merges source into target with strategy merge|squash|ff, creating target from start
// if needed. It works in target's own checkout when that is clean, else in a temporary one.
func mergeInto(repo, source, target, start, strategy, msg string) (sha string, err error) {
	if !branchExists(repo, target) {
		if _, err := git(repo, "branch", target, start); err != nil {
			return "", err
		}
		defer func() {
			if err != nil {
				git(repo, "branch", "-D", target) // don't leave behind a branch for a merge that didn't happen
			}
		}()
	}
	dir := checkedOutAt(repo, target)
	if dir != "" {
		if out, _ := git(dir, "status", "--porcelain", "--untracked-files=no"); strings.TrimSpace(out) != "" {
			return "", fmt.Errorf("%s has uncommitted changes in %s; commit or stash them first", target, dir)
		}
	} else {
		tmp, err := os.MkdirTemp("", "arranger-merge-")
		if err != nil {
			return "", err
		}
		os.Remove(tmp) // git worktree add wants to create it
		if _, err := git(repo, "worktree", "add", "-q", tmp, target); err != nil {
			return "", err
		}
		defer git(repo, "worktree", "remove", "--force", tmp)
		dir = tmp
	}
	var who []string // the user's own identity when git has one, else arranger's
	if out, _ := git(dir, "config", "user.email"); strings.TrimSpace(out) == "" {
		who = identity
	}
	switch strategy {
	case "merge":
		_, err = git(dir, append(who, "merge", "--no-ff", "-m", msg, source)...)
	case "squash":
		if _, err = git(dir, "merge", "--squash", source); err == nil {
			_, err = git(dir, append(who, "commit", "-q", "-m", msg)...)
		}
	case "ff":
		_, err = git(dir, "merge", "--ff-only", source)
	default:
		return "", fmt.Errorf("unknown strategy %q", strategy)
	}
	if err != nil {
		git(dir, "reset", "--merge") // back to where target was
		return "", err
	}
	out, err := git(dir, "rev-parse", "--short", "HEAD")
	return strings.TrimSpace(out), err
}
