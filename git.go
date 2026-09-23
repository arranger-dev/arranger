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
