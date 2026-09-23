package main

import (
	"path/filepath"
	"strings"
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
			return []string{"-c", `grep -q "PREVIOUS ATTEMPT WAS NOT ACCEPTED" && echo ok > out.txt; ` +
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

// waitDone polls until the agent's goal leaves the busy states.
func waitDone(t *testing.T, id string) Goal {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		g, _ := getGoal(id)
		switch g.Status {
		case "done", "failed", "blocked", "stopped":
			return g
		}
	}
	t.Fatal("timed out")
	return Goal{}
}

// A manager plans for two fake workers, rejects one in its first review, and merges both.
func TestManagerPlansReviewsMerges(t *testing.T) {
	data := t.TempDir()
	worktreeRoot = filepath.Join(data, "worktrees")
	if err := openDB(filepath.Join(data, "a.db")); err != nil {
		t.Fatal(err)
	}
	flag := filepath.Join(data, "reviewed-once")
	plain := func(l []byte) []Event { return []Event{{Kind: "msg", Text: string(l)}} }
	runtimes["fakemgr"] = Runtime{Bin: "sh", Parse: plain, Args: func(string) []string {
		return []string{"-c", `p=$(cat); echo scribble > junk.txt
case "$p" in
*"YOUR TEAM"*) echo '{"subgoals":[{"agent":"a","title":"write a","checks":["test -f a.txt"]},{"agent":"b","title":"write b","checks":["test -f b.txt"]}]}' ;;
*) if [ -f ` + flag + ` ]; then echo '{"verdicts":[{"agent":"b","accept":true}]}'
   else touch ` + flag + `; echo 'Sure: {"verdicts":[{"agent":"a","accept":true},{"agent":"b","accept":false,"feedback":"add a newline"}]}'; fi ;;
esac`}
	}}
	runtimes["fakeworker"] = Runtime{Bin: "sh", Parse: plain, Args: func(string) []string {
		return []string{"-c", `p=$(cat); case "$p" in *"You are Alpha"*) echo a > a.txt ;; *) echo b > b.txt ;; esac; echo worked`}
	}}
	defer delete(runtimes, "fakemgr")
	defer delete(runtimes, "fakeworker")

	repo := newRepo(t)
	p, _ := createProject(Project{Name: "t", Repo: repo, Base: "main"})
	err := saveArrangement(p.ID, []Agent{
		{ID: "m", Name: "Lead", Role: "manager", Runtime: "fakemgr"},
		{ID: "a", Name: "Alpha", Role: "coder", Parent: "m", Runtime: "fakeworker"},
		{ID: "b", Name: "Beta", Role: "coder", Parent: "m", Runtime: "fakeworker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	saveGoal("m", Goal{Title: "both files", Checks: "test -f a.txt && test -f b.txt"})
	if err := startGoal("m"); err != nil {
		t.Fatal(err)
	}
	if g := waitDone(t, "m"); g.Status != "done" {
		es, _ := listEvents("m", 50)
		t.Fatalf("manager: %+v\nevents: %+v", g, es)
	}
	// children got their subgoals from the plan and finished
	for _, id := range []string{"a", "b"} {
		if g, _ := getGoal(id); g.Status != "done" || len(g.Base) != 40 {
			t.Fatalf("%s: %+v", id, g)
		}
	}
	// a child's diff still shows its work after the manager merged it
	ga, _ := getGoal("a")
	if fs, err := worktreeDiff(filepath.Join(worktreeRoot, "a"), ga.Base); err != nil || len(fs) != 1 || fs[0].Path != "a.txt" {
		t.Fatalf("a's diff after merge: %+v %v", fs, err)
	}
	// b was rejected once, so it ran twice; a ran once
	var runsA, runsB int
	db.QueryRow(`SELECT count(*) FROM runs WHERE agent_id='a'`).Scan(&runsA)
	db.QueryRow(`SELECT count(*) FROM runs WHERE agent_id='b'`).Scan(&runsB)
	if runsA != 1 || runsB != 2 {
		t.Fatalf("runs a=%d b=%d", runsA, runsB)
	}
	// the manager's branch holds both files, and its decision calls left no edits behind
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, err := git(repo, "cat-file", "-e", "arranger/m:"+f); err != nil {
			t.Fatalf("%s not merged into the manager: %v", f, err)
		}
	}
	if _, err := git(repo, "cat-file", "-e", "arranger/m:junk.txt"); err == nil {
		t.Fatal("manager's own file edits should be discarded")
	}
	var decisions int
	db.QueryRow(`SELECT count(*) FROM decisions WHERE agent_id='m'`).Scan(&decisions)
	if decisions != 3 { // plan + two reviews
		t.Fatalf("decisions: %d", decisions)
	}
}

func TestValidatePlan(t *testing.T) {
	team := map[string]Agent{"a": {}, "b": {}}
	bad := [][]subgoal{
		nil,
		{{Agent: "zz", Title: "x", Checks: []string{"true"}}},
		{{Agent: "a", Title: "x", Checks: []string{" "}}},
		{{Agent: "a", Title: "", Checks: []string{"true"}}},
		{{Agent: "a", Title: "x", Checks: []string{"true"}}, {Agent: "a", Title: "y", Checks: []string{"true"}}},
	}
	for i, p := range bad {
		if validatePlan(p, team) == nil {
			t.Errorf("plan %d should be rejected", i)
		}
	}
	var v struct{ Subgoals []subgoal }
	if err := extractJSON("Here you go:\n```json\n{\"subgoals\":[]}\n```", &v); err != nil {
		t.Fatal(err)
	}
}

// Stopping a manager stops the children it's waiting on.
func TestStopManagerStopsTeam(t *testing.T) {
	data := t.TempDir()
	worktreeRoot = filepath.Join(data, "worktrees")
	if err := openDB(filepath.Join(data, "a.db")); err != nil {
		t.Fatal(err)
	}
	plain := func(l []byte) []Event { return []Event{{Kind: "msg", Text: string(l)}} }
	runtimes["fakemgr2"] = Runtime{Bin: "sh", Parse: plain, Args: func(string) []string {
		return []string{"-c", `cat >/dev/null; echo '{"subgoals":[{"agent":"s","title":"sleep","checks":["true"]}]}'`}
	}}
	runtimes["sleeper"] = Runtime{Bin: "sh", Parse: plain, Args: func(string) []string { return []string{"-c", "cat >/dev/null; sleep 30"} }}
	defer delete(runtimes, "fakemgr2")
	defer delete(runtimes, "sleeper")

	p, _ := createProject(Project{Name: "t", Repo: newRepo(t), Base: "main"})
	saveArrangement(p.ID, []Agent{
		{ID: "m", Name: "M", Role: "manager", Runtime: "fakemgr2"},
		{ID: "s", Name: "S", Role: "coder", Parent: "m", Runtime: "sleeper"},
	})
	saveGoal("m", Goal{Title: "x", Checks: "true"})
	if err := startGoal("m"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); !isRunning("s"); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("child never started")
		}
	}
	start := time.Now()
	stopGoal("m")
	if g := waitDone(t, "m"); g.Status != "stopped" {
		t.Fatalf("manager: %+v", g)
	}
	if g, _ := getGoal("s"); g.Status != "stopped" {
		t.Fatalf("child: %+v", g)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("stop took too long")
	}
}

// Soft limit warns once; hard limit kills the agent mid-run and fails the goal with a clear error.
func TestTokenLimits(t *testing.T) {
	data := t.TempDir()
	worktreeRoot = filepath.Join(data, "worktrees")
	if err := openDB(filepath.Join(data, "a.db")); err != nil {
		t.Fatal(err)
	}
	// each assistant message reports 100 tokens; the same message repeated must not double count
	runtimes["chatty"] = Runtime{Bin: "sh", Parse: parseClaude, Args: func(string) []string {
		return []string{"-c", `cat >/dev/null
for i in 1 2 3 4 5; do
  echo '{"type":"assistant","message":{"id":"m'$i'","content":[{"type":"text","text":"step '$i'"}],"usage":{"input_tokens":90,"output_tokens":10}}}'
  echo '{"type":"assistant","message":{"id":"m'$i'","content":[],"usage":{"input_tokens":90,"output_tokens":10}}}'
  sleep 0.2
done
sleep 30`}
	}}
	defer delete(runtimes, "chatty")

	p, _ := createProject(Project{Name: "t", Repo: newRepo(t), Base: "main"})
	saveArrangement(p.ID, []Agent{{ID: "w", Name: "W", Role: "coder", Runtime: "chatty"}})
	a, _, _ := getAgent("w")
	a.Soft, a.Hard = 150, 300
	updateAgent(a)
	saveGoal("w", Goal{Title: "talk", Checks: "true"})
	start := time.Now()
	if err := startGoal("w"); err != nil {
		t.Fatal(err)
	}
	g := waitDone(t, "w")
	if g.Status != "failed" || !strings.Contains(g.Feedback, "hard limit of 300") {
		t.Fatalf("goal: %+v", g)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("hard limit should stop the agent right away, not after it finishes")
	}
	s, _ := projectSummary(p.ID)
	if st := s.Agents["w"]; st.Tokens != 300 || st.AllTokens != 300 {
		t.Fatalf("tokens counted: %+v", st)
	}
	var warns int
	db.QueryRow(`SELECT count(*) FROM events WHERE agent_id='w' AND kind='warn'`).Scan(&warns)
	if warns != 1 {
		t.Fatalf("soft limit warnings: %d", warns)
	}
}
