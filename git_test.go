package main

import (
	"os"
	"os/exec"
	"path/filepath"
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
