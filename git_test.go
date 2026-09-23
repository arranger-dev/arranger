package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newRepo makes a temp git repo on branch main with one committed file.
func newRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\ntwo\n"), 0o644)
	if _, err := checkpoint(repo, "a"); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestWorktreeDiff(t *testing.T) {
	repo := newRepo(t)
	base, err := checkRepo(repo, "")
	if err != nil || base != "main" {
		t.Fatalf("checkRepo: %q %v", base, err)
	}
	dir, err := ensureWorktree(t.TempDir(), repo, base, "agent1")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n2\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("new\n"), 0o644)
	sha, err := checkpoint(dir, "attempt 1")
	if err != nil || sha == "" {
		t.Fatalf("checkpoint: %q %v", sha, err)
	}
	if sha2, _ := checkpoint(dir, "nothing"); sha2 != "" {
		t.Fatal("empty checkpoint should be skipped")
	}
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n2\nthree\n"), 0o644) // uncommitted change counts too

	fs, err := worktreeDiff(dir, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 2 || fs[0].Path != "a.txt" || fs[1].Path != "b.txt" {
		t.Fatalf("files: %+v", fs)
	}
	if adds, dels := diffStat(fs); adds != 3 || dels != 1 {
		t.Fatalf("stat +%d -%d", adds, dels)
	}
	if len(fs[0].Hunks) != 1 || fs[0].Hunks[0].ID == "" {
		t.Fatalf("hunks: %+v", fs[0].Hunks)
	}
	// base branch untouched
	if out, _ := git(repo, "show", "main:a.txt"); out != "one\ntwo\n" {
		t.Fatalf("base changed: %q", out)
	}
}

func TestHunksRevertMerge(t *testing.T) {
	repo := newRepo(t)
	root := t.TempDir()
	mgr, err := ensureWorktree(root, repo, "main", "mgr")
	if err != nil {
		t.Fatal(err)
	}
	kid, err := ensureWorktree(root, repo, branchOf("mgr"), "kid")
	if err != nil {
		t.Fatal(err)
	}
	// two separate hunks in a.txt plus a new file
	os.WriteFile(filepath.Join(repo, "a.txt"), nil, 0o644)
	body := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n"
	os.WriteFile(filepath.Join(kid, "a.txt"), []byte(body), 0o644)
	checkpoint(kid, "base content")
	git(mgr, append(identity, "merge", "-q", branchOf("kid"))...) // manager starts from the same content
	os.WriteFile(filepath.Join(kid, "a.txt"), []byte("ONE\n"+body[2:len(body)-3]+"TWELVE\n"), 0o644)
	os.WriteFile(filepath.Join(kid, "new.txt"), []byte("new\n"), 0o644)
	first, _ := checkpoint(kid, "attempt 1")

	fs, _ := worktreeDiff(kid, branchOf("mgr"))
	if len(fs) != 2 || len(fs[0].Hunks) != 2 {
		t.Fatalf("want a.txt with 2 hunks + new.txt, got %+v", fs)
	}

	// promote only the first hunk of a.txt to the manager
	patch, n := hunkPatch(fs, map[string]bool{fs[0].Hunks[0].ID: true})
	if n != 1 {
		t.Fatal("patch should hold one hunk")
	}
	if _, err := gitIn(mgr, patch, "apply", "-"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(mgr, "a.txt")); !strings.HasPrefix(string(b), "ONE\n") || strings.Contains(string(b), "TWELVE") {
		t.Fatalf("promote applied wrong content: %q", b)
	}

	// remove the new file and the second hunk from the kid's work
	patch, _ = hunkPatch(fs, map[string]bool{fs[0].Hunks[1].ID: true, fs[1].Hunks[0].ID: true})
	if _, err := gitIn(kid, patch, "apply", "-R", "-"); err != nil {
		t.Fatal(err)
	}
	checkpoint(kid, "removed")
	fs, _ = worktreeDiff(kid, branchOf("mgr"))
	if len(fs) != 1 || len(fs[0].Hunks) != 1 || fs[0].Adds != 1 {
		t.Fatalf("after remove: %+v", fs)
	}

	cs, err := checkpoints(kid, branchOf("mgr"))
	if err != nil || len(cs) != 2 || cs[0].Subject != "removed" {
		t.Fatalf("checkpoints: %+v %v", cs, err)
	}
	// reverting the first attempt conflicts with the later removal commit; it must abort cleanly
	if err := revertCommit(kid, first); err == nil {
		t.Log("revert applied cleanly")
	}
	if out, _ := git(kid, "status", "--porcelain"); out != "" {
		t.Fatalf("worktree left dirty: %q", out)
	}
	if err := revertCommit(kid, cs[0].SHA); err != nil {
		t.Fatalf("revert of latest checkpoint: %v", err)
	}
	if _, err := os.Stat(filepath.Join(kid, "new.txt")); err != nil {
		t.Fatal("reverting the removal should bring new.txt back")
	}

	// merge the kid into the manager
	checkpoint(mgr, "promoted")
	if err := mergeBranch(mgr, branchOf("kid"), "merge kid"); err != nil {
		t.Logf("merge conflicted as expected with the promoted hunk: %v", err)
		if out, _ := git(mgr, "status", "--porcelain"); out != "" {
			t.Fatalf("aborted merge left the manager dirty: %q", out)
		}
	}
}

func TestMergeInto(t *testing.T) {
	repo := newRepo(t) // on main, with a.txt committed
	root := t.TempDir()
	dir, _ := ensureWorktree(root, repo, "main", "agent")
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b\n"), 0o644)
	checkpoint(dir, "work 1")
	os.WriteFile(filepath.Join(dir, "c.txt"), []byte("c\n"), 0o644)
	checkpoint(dir, "work 2")
	src := branchOf("agent")

	if got := defaultTarget(repo); got != "main" {
		t.Fatalf("default target %q", got)
	}
	// a new branch is created from start; squash makes one commit; the user's checkout isn't touched
	m, err := previewMerge(repo, src, "release", "main")
	if err != nil || m.Exists || m.Commits != 2 || m.Files != 2 || !m.CanFF || len(m.Conflicts) != 0 {
		t.Fatalf("preview: %+v %v", m, err)
	}
	if _, err := mergeInto(repo, src, "release", "main", "squash", "add b and c"); err != nil {
		t.Fatal(err)
	}
	if out, _ := git(repo, "log", "--format=%s", "main..release"); strings.TrimSpace(out) != "add b and c" {
		t.Fatalf("squash history: %q", out)
	}
	if m, _ := previewMerge(repo, src, "release", "main"); m.Files != 0 {
		t.Fatalf("after a squash there's nothing left to merge: %+v", m)
	}
	// merging into the checked-out branch updates the user's files, but only when clean
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("local edit\n"), 0o644)
	if _, err := mergeInto(repo, src, "main", "main", "merge", "m"); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("dirty checkout should be refused, got %v", err)
	}
	git(repo, "checkout", "--", "a.txt")
	if _, err := mergeInto(repo, src, "main", "main", "ff", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "c.txt")); err != nil {
		t.Fatal("ff merge should update the checked-out files")
	}
	// conflicts are reported by the preview and leave the target untouched when attempted
	os.WriteFile(filepath.Join(repo, "b.txt"), []byte("theirs\n"), 0o644)
	checkpoint(repo, "conflicting b")
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("ours\n"), 0o644)
	checkpoint(dir, "change b")
	if m, _ := previewMerge(repo, src, "main", "main"); len(m.Conflicts) != 1 || m.Conflicts[0] != "b.txt" {
		t.Fatalf("conflicts: %+v", m.Conflicts)
	}
	if _, err := mergeInto(repo, src, "newbranch", "main", "merge", "m"); err == nil || branchExists(repo, "newbranch") {
		t.Fatalf("a failed merge into a new branch must not leave the branch behind (err %v)", err)
	}
	before, _ := git(repo, "rev-parse", "main")
	if _, err := mergeInto(repo, src, "main", "main", "merge", "m"); err == nil {
		t.Fatal("conflicting merge should fail")
	}
	after, _ := git(repo, "rev-parse", "main")
	if out, _ := git(repo, "status", "--porcelain"); before != after || out != "" {
		t.Fatalf("failed merge left changes: %q", out)
	}
}
