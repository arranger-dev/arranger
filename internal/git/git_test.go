package git

import (
	"fmt"
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
	if _, err := Commit(repo, "a"); err != nil {
		t.Fatal(err)
	}
	return repo
}

// A freshly initialized repo has an unborn HEAD; saving it must explain what to do, not dump git's error.
func TestCheckRepoWithoutCommits(t *testing.T) {
	repo := t.TempDir()
	exec.Command("git", "-C", repo, "init", "-q", "-b", "main").Run()
	for _, base := range []string{"", "main"} {
		_, err := CheckRepo(repo, base)
		if err == nil || !strings.Contains(err.Error(), "no commits yet") || strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("base %q: want a 'no commits yet' error, got %v", base, err)
		}
	}

	// an orphan branch in a repo that has commits elsewhere: blank base can't default to it
	repo = newRepo(t)
	exec.Command("git", "-C", repo, "checkout", "-q", "--orphan", "fresh").Run()
	if _, err := CheckRepo(repo, ""); err == nil || !strings.Contains(err.Error(), "enter a base branch") {
		t.Fatalf("orphan branch: got %v", err)
	}
	if base, err := CheckRepo(repo, "main"); err != nil || base != "main" {
		t.Fatalf("orphan branch with base main: %q %v", base, err)
	}
}

func TestWorktreeDiff(t *testing.T) {
	repo := newRepo(t)
	base, err := CheckRepo(repo, "")
	if err != nil || base != "main" {
		t.Fatalf("checkRepo: %q %v", base, err)
	}
	dir, err := EnsureWorktree(t.TempDir(), repo, base, "agent1")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n2\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("new\n"), 0o644)
	sha, err := Commit(dir, "attempt 1")
	if err != nil || sha == "" {
		t.Fatalf("checkpoint: %q %v", sha, err)
	}
	if sha2, _ := Commit(dir, "nothing"); sha2 != "" {
		t.Fatal("empty checkpoint should be skipped")
	}
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n2\nthree\n"), 0o644) // uncommitted change counts too

	fs, err := Diff(dir, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 2 || fs[0].Path != "a.txt" || fs[1].Path != "b.txt" {
		t.Fatalf("files: %+v", fs)
	}
	if adds, dels := Stat(fs); adds != 3 || dels != 1 {
		t.Fatalf("stat +%d -%d", adds, dels)
	}
	if len(fs[0].Hunks) != 1 || fs[0].Hunks[0].ID == "" {
		t.Fatalf("hunks: %+v", fs[0].Hunks)
	}
	// base branch untouched
	if out, _ := Run(repo, "show", "main:a.txt"); out != "one\ntwo\n" {
		t.Fatalf("base changed: %q", out)
	}
}

func TestHunksRevertMerge(t *testing.T) {
	repo := newRepo(t)
	root := t.TempDir()
	mgr, err := EnsureWorktree(root, repo, "main", "mgr")
	if err != nil {
		t.Fatal(err)
	}
	kid, err := EnsureWorktree(root, repo, BranchOf("mgr"), "kid")
	if err != nil {
		t.Fatal(err)
	}
	// two separate hunks in a.txt plus a new file
	os.WriteFile(filepath.Join(repo, "a.txt"), nil, 0o644)
	body := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n"
	os.WriteFile(filepath.Join(kid, "a.txt"), []byte(body), 0o644)
	Commit(kid, "base content")
	Run(mgr, append(identity, "merge", "-q", BranchOf("kid"))...) // manager starts from the same content
	os.WriteFile(filepath.Join(kid, "a.txt"), []byte("ONE\n"+body[2:len(body)-3]+"TWELVE\n"), 0o644)
	os.WriteFile(filepath.Join(kid, "new.txt"), []byte("new\n"), 0o644)
	first, _ := Commit(kid, "attempt 1")

	fs, _ := Diff(kid, BranchOf("mgr"))
	if len(fs) != 2 || len(fs[0].Hunks) != 2 {
		t.Fatalf("want a.txt with 2 hunks + new.txt, got %+v", fs)
	}

	// promote only the first hunk of a.txt to the manager
	patch, n := HunkPatch(fs, map[string]bool{fs[0].Hunks[0].ID: true})
	if n != 1 {
		t.Fatal("patch should hold one hunk")
	}
	if _, err := RunIn(mgr, patch, "apply", "-"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(mgr, "a.txt")); !strings.HasPrefix(string(b), "ONE\n") || strings.Contains(string(b), "TWELVE") {
		t.Fatalf("promote applied wrong content: %q", b)
	}

	// remove the new file and the second hunk from the kid's work
	patch, _ = HunkPatch(fs, map[string]bool{fs[0].Hunks[1].ID: true, fs[1].Hunks[0].ID: true})
	if _, err := RunIn(kid, patch, "apply", "-R", "-"); err != nil {
		t.Fatal(err)
	}
	Commit(kid, "removed")
	fs, _ = Diff(kid, BranchOf("mgr"))
	if len(fs) != 1 || len(fs[0].Hunks) != 1 || fs[0].Adds != 1 {
		t.Fatalf("after remove: %+v", fs)
	}

	cs, err := Checkpoints(kid, BranchOf("mgr"))
	if err != nil || len(cs) != 2 || cs[0].Subject != "removed" {
		t.Fatalf("checkpoints: %+v %v", cs, err)
	}
	// reverting the first attempt conflicts with the later removal commit; it must abort cleanly
	if err := Revert(kid, first); err == nil {
		t.Log("revert applied cleanly")
	}
	if out, _ := Run(kid, "status", "--porcelain"); out != "" {
		t.Fatalf("worktree left dirty: %q", out)
	}
	if err := Revert(kid, cs[0].SHA); err != nil {
		t.Fatalf("revert of latest checkpoint: %v", err)
	}
	if _, err := os.Stat(filepath.Join(kid, "new.txt")); err != nil {
		t.Fatal("reverting the removal should bring new.txt back")
	}

	// merge the kid into the manager
	Commit(mgr, "promoted")
	if err := MergeBranch(mgr, BranchOf("kid"), "merge kid"); err != nil {
		t.Logf("merge conflicted as expected with the promoted hunk: %v", err)
		if out, _ := Run(mgr, "status", "--porcelain"); out != "" {
			t.Fatalf("aborted merge left the manager dirty: %q", out)
		}
	}
}

func TestMergeInto(t *testing.T) {
	repo := newRepo(t) // on main, with a.txt committed
	root := t.TempDir()
	dir, _ := EnsureWorktree(root, repo, "main", "agent")
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b\n"), 0o644)
	Commit(dir, "work 1")
	os.WriteFile(filepath.Join(dir, "c.txt"), []byte("c\n"), 0o644)
	Commit(dir, "work 2")
	src := BranchOf("agent")

	if got := DefaultTarget(repo); got != "main" {
		t.Fatalf("default target %q", got)
	}
	// a new branch is created from start; squash makes one commit; the user's checkout isn't touched
	m, err := PreviewMerge(repo, src, "release", "main")
	if err != nil || m.Exists || m.Commits != 2 || m.Files != 2 || !m.CanFF || len(m.Conflicts) != 0 {
		t.Fatalf("preview: %+v %v", m, err)
	}
	if _, err := MergeInto(repo, src, "release", "main", "squash", "add b and c"); err != nil {
		t.Fatal(err)
	}
	if out, _ := Run(repo, "log", "--format=%s", "main..release"); strings.TrimSpace(out) != "add b and c" {
		t.Fatalf("squash history: %q", out)
	}
	if m, _ := PreviewMerge(repo, src, "release", "main"); m.Files != 0 {
		t.Fatalf("after a squash there's nothing left to merge: %+v", m)
	}
	// merging into the checked-out branch updates the user's files, but only when clean
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("local edit\n"), 0o644)
	if _, err := MergeInto(repo, src, "main", "main", "merge", "m"); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("dirty checkout should be refused, got %v", err)
	}
	Run(repo, "checkout", "--", "a.txt")
	if _, err := MergeInto(repo, src, "main", "main", "ff", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "c.txt")); err != nil {
		t.Fatal("ff merge should update the checked-out files")
	}
	// conflicts are reported by the preview and leave the target untouched when attempted
	os.WriteFile(filepath.Join(repo, "b.txt"), []byte("theirs\n"), 0o644)
	Commit(repo, "conflicting b")
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("ours\n"), 0o644)
	Commit(dir, "change b")
	if m, _ := PreviewMerge(repo, src, "main", "main"); len(m.Conflicts) != 1 || m.Conflicts[0] != "b.txt" {
		t.Fatalf("conflicts: %+v", m.Conflicts)
	}
	if _, err := MergeInto(repo, src, "newbranch", "main", "merge", "m"); err == nil || BranchExists(repo, "newbranch") {
		t.Fatalf("a failed merge into a new branch must not leave the branch behind (err %v)", err)
	}
	before, _ := Run(repo, "rev-parse", "main")
	if _, err := MergeInto(repo, src, "main", "main", "merge", "m"); err == nil {
		t.Fatal("conflicting merge should fail")
	}
	after, _ := Run(repo, "rev-parse", "main")
	if out, _ := Run(repo, "status", "--porcelain"); before != after || out != "" {
		t.Fatalf("failed merge left changes: %q", out)
	}
}

func TestSync(t *testing.T) {
	repo := newRepo(t)
	dir, err := EnsureWorktree(t.TempDir(), repo, "main", "agent1")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := Run(repo, "rev-parse", "main")
	os.WriteFile(filepath.Join(dir, "mine.txt"), []byte("agent\n"), 0o644) // uncommitted work survives the sync

	// the user commits to main after the agent branched off
	os.WriteFile(filepath.Join(repo, "theirs.txt"), []byte("user\n"), 0o644)
	Commit(repo, "user work")
	if n := Behind(dir, "main"); n != 1 {
		t.Fatalf("behind = %d, want 1", n)
	}
	if n, err := Sync(dir, "main"); err != nil || n != 1 {
		t.Fatalf("sync: %d %v", n, err)
	}
	for _, f := range []string{"mine.txt", "theirs.txt"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("%s missing after sync", f)
		}
	}
	if n, err := Sync(dir, "main"); err != nil || n != 0 {
		t.Fatalf("second sync should be a no-op: %d %v", n, err)
	}
	// measured from main's new tip, the diff holds only the agent's own work
	tip, _ := Run(repo, "rev-parse", "main")
	if fs, _ := Diff(dir, strings.TrimSpace(tip)); len(fs) != 1 || fs[0].Path != "mine.txt" {
		t.Fatalf("diff from new tip: %+v", fs)
	}
	if fs, _ := Diff(dir, strings.TrimSpace(base)); len(fs) != 2 {
		t.Fatalf("diff from the old base should include theirs.txt too: %+v", fs)
	}

	// both sides change the same line: the merge is abandoned and the file named
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\nagent\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\nuser\n"), 0o644)
	Commit(repo, "user edits a.txt")
	if _, err := Sync(dir, "main"); err == nil || !strings.Contains(err.Error(), "a.txt") {
		t.Fatalf("conflicting sync: %v", err)
	}
	if st, _ := Run(dir, "status", "--porcelain"); strings.TrimSpace(st) != "" {
		t.Fatalf("worktree should be clean after an abandoned sync:\n%s", st)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "one\nagent\n" {
		t.Fatalf("agent's a.txt changed: %q", b)
	}
}

// New files show in the diff while the agent is still working, before any checkpoint commits them,
// and hunk remove works on them.
func TestDiffShowsUncommittedNewFiles(t *testing.T) {
	repo := newRepo(t)
	os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("build/\n"), 0o644)
	Commit(repo, "ignore build")
	dir, _ := EnsureWorktree(t.TempDir(), repo, "main", "a")
	os.MkdirAll(filepath.Join(dir, "pkg"), 0o755)
	os.WriteFile(filepath.Join(dir, "pkg", "deep.go"), []byte("package pkg\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n2\n"), 0o644) // an edit, uncommitted
	os.MkdirAll(filepath.Join(dir, "build"), 0o755)
	os.WriteFile(filepath.Join(dir, "build", "out.bin"), []byte("ignored"), 0o644)  // .gitignore'd
	os.WriteFile(filepath.Join(dir, "huge.log"), make([]byte, maxNewFile+1), 0o644) // too big to show

	fs, err := Diff(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]FileDiff{}
	for _, f := range fs {
		paths[f.Path] = f
	}
	if len(fs) != 2 || paths["pkg/deep.go"].Adds != 1 || len(paths["pkg/deep.go"].Hunks) != 1 || paths["a.txt"].Adds != 1 {
		t.Fatalf("want a.txt and pkg/deep.go, got %+v", fs)
	}
	if st, _ := Run(repo, "status", "--porcelain"); strings.TrimSpace(st) != "" {
		t.Fatalf("reading a worktree's diff must not change the repo: %q", st)
	}

	// removing the new file's hunk deletes it, like any other change
	patch, _ := HunkPatch(fs, map[string]bool{paths["pkg/deep.go"].Hunks[0].ID: true})
	if _, err := RunIn(dir, patch, "apply", "-R", "-"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pkg", "deep.go")); !os.IsNotExist(err) {
		t.Fatal("removing the hunk should delete the new file")
	}
}

// Reports that start together create their worktrees at the same moment; none may fail.
func TestEnsureWorktreeConcurrently(t *testing.T) {
	repo, root := newRepo(t), t.TempDir()
	errs := make(chan error, 8)
	for i := range 8 {
		go func() {
			_, err := EnsureWorktree(root, repo, "main", fmt.Sprintf("a%d", i))
			errs <- err
		}()
	}
	for range 8 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}
