package main

import (
	"path/filepath"
	"testing"
	"time"
)

// A fake agent that fails the check on its first attempt and fixes it on the second,
// proving the verify -> feedback -> retry -> done loop end to end.
func TestRunGoalRetriesUntilChecksPass(t *testing.T) {
	data := t.TempDir()
	worktreeRoot = filepath.Join(data, "worktrees")
	if err := openDB(filepath.Join(data, "a.db")); err != nil {
		t.Fatal(err)
	}
	runtimes["fake"] = Runtime{
		Bin: "sh",
		Args: func(string) []string {
			return []string{"-c", `grep -q "PREVIOUS ATTEMPT FAILED" && echo ok > out.txt; ` +
				`echo '{"type":"result","subtype":"success","result":"did it","total_cost_usd":0.01}'`}
		},
		Parse: parseClaude,
	}
	defer delete(runtimes, "fake")

	repo := newRepo(t)
	p, err := createProject(Project{Name: "t", Repo: repo, Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if err := saveArrangement(p.ID, []Agent{{ID: "w", Name: "Worker", Role: "coder", Runtime: "fake"}}); err != nil {
		t.Fatal(err)
	}
	if err := startGoal("w"); err == nil {
		t.Fatal("run without goal should fail")
	}
	saveGoal("w", Goal{Title: "make out.txt", Checks: "test -f out.txt"})
	if err := startGoal("w"); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(20 * time.Second)
	var g Goal
	for time.Now().Before(deadline) {
		if g, _ = getGoal("w"); g.Status == "done" || g.Status == "failed" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if g.Status != "done" || g.Attempts != 2 || g.Passed != 1 || g.Adds != 1 {
		t.Fatalf("goal: %+v", g)
	}
	s, _ := projectSummary(p.ID)
	if s.Cost < 0.019 {
		t.Fatalf("cost not summed: %+v", s)
	}
	es, _ := listEvents("w", 100)
	if len(es) == 0 {
		t.Fatal("no events logged")
	}
}
