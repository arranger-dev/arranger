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
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
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

// ensureWorktree returns the agent's worktree, creating it on branch arranger/<agent> from base.
func ensureWorktree(root, repo, base, agentID string) (string, error) {
	dir := filepath.Join(root, agentID)
	if _, err := os.Stat(dir); err == nil {
		return dir, nil
	}
	branch := "arranger/" + agentID
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
	if _, err := git(dir, "-c", "user.name=arranger", "-c", "user.email=arranger@localhost", "commit", "--no-verify", "-q", "-m", msg); err != nil {
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
	Path  string `json:"path"`
	Adds  int    `json:"adds"`
	Dels  int    `json:"dels"`
	Hunks []Hunk `json:"hunks"`
}

// worktreeDiff is everything the agent changed since it branched off base, committed or not.
// ponytail: new files show once they're checkpointed (untracked files aren't in git diff).
func worktreeDiff(dir, base string) ([]FileDiff, error) {
	mb, err := git(dir, "merge-base", base, "HEAD")
	if err != nil {
		return nil, err
	}
	out, err := git(dir, "diff", "--no-color", "--no-ext-diff", strings.TrimSpace(mb))
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
			fs = append(fs, FileDiff{Path: strings.TrimPrefix(l[strings.LastIndex(l, " b/")+1:], "b/")})
			f, h = &fs[len(fs)-1], nil
		case f == nil, l == "":
		case strings.HasPrefix(l, "@@"):
			sum := sha1.Sum([]byte(f.Path + l))
			f.Hunks = append(f.Hunks, Hunk{ID: fmt.Sprintf("%x", sum[:4]), Header: l})
			h = &f.Hunks[len(f.Hunks)-1]
		case h == nil:
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
