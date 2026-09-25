package orch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"arranger/internal/agents"
	"arranger/internal/git"
	"arranger/internal/store"
)

// A fake agent that fails the check on its first attempt and fixes it on the second,
// proving the verify -> feedback -> retry -> done loop end to end.
func TestRunGoalRetriesUntilChecksPass(t *testing.T) {
	o := setup(t)
	agents.Runtimes["fake"] = agents.Runtime{
		Bin: "sh",
		Args: func(string) []string {
			return []string{"-c", `grep -q "PREVIOUS ATTEMPT WAS NOT ACCEPTED" && echo ok > out.txt; ` +
				`echo '{"type":"result","subtype":"success","result":"did it","total_cost_usd":0.01}'`}
		},
		Parse: agents.Runtimes["claude"].Parse,
	}
	defer delete(agents.Runtimes, "fake")

	repo := newRepo(t)
	p, err := o.Store.CreateProject(store.Project{Name: "t", Repo: repo, Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Store.SaveArrangement(p.ID, []store.Agent{{ID: "w", Name: "Worker", Role: "coder", Runtime: "fake"}}); err != nil {
		t.Fatal(err)
	}
	if err := o.Start("w"); err == nil {
		t.Fatal("run without goal should fail")
	}
	o.Store.SaveGoal("w", store.Goal{Title: "make out.txt", Checks: "test -f out.txt"})
	if err := o.Start("w"); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(20 * time.Second)
	var g store.Goal
	for time.Now().Before(deadline) {
		if g, _ = o.Store.Goal("w"); g.Status == "done" || g.Status == "failed" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if g.Status != "done" || g.Attempts != 2 || g.Passed != 1 || g.Adds != 1 {
		t.Fatalf("goal: %+v", g)
	}
	s, _ := o.Store.Summary(p.ID)
	if s.Cost < 0.019 {
		t.Fatalf("cost not summed: %+v", s)
	}
	es, _ := o.Store.Events("w", 100)
	if len(es) == 0 {
		t.Fatal("no events logged")
	}
}

// waitDone polls until the agent's goal leaves the busy states.
func waitDone(t *testing.T, o *Orchestrator, id string) store.Goal {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		g, _ := o.Store.Goal(id)
		switch g.Status {
		case "done", "failed", "blocked", "stopped":
			return g
		}
	}
	t.Fatal("timed out")
	return store.Goal{}
}

// A manager plans for two fake workers, rejects one in its first review, and merges both.
func TestManagerPlansReviewsMerges(t *testing.T) {
	o := setup(t)
	flag := filepath.Join(t.TempDir(), "reviewed-once")
	plain := func(l []byte) []agents.Event { return []agents.Event{{Kind: "msg", Text: string(l)}} }
	agents.Runtimes["fakemgr"] = agents.Runtime{Bin: "sh", Parse: plain, Args: func(string) []string {
		return []string{"-c", `p=$(cat); echo scribble > junk.txt
case "$p" in
*"YOUR TEAM"*) echo '{"subgoals":[{"agent":"a","title":"write a","checks":["test -f a.txt"]},{"agent":"b","title":"write b","checks":["test -f b.txt"]}]}' ;;
*) if [ -f ` + flag + ` ]; then echo '{"verdicts":[{"agent":"b","accept":true}]}'
   else touch ` + flag + `; echo 'Sure: {"verdicts":[{"agent":"a","accept":true},{"agent":"b","accept":false,"feedback":"add a newline"}]}'; fi ;;
esac`}
	}}
	agents.Runtimes["fakeworker"] = agents.Runtime{Bin: "sh", Parse: plain, Args: func(string) []string {
		return []string{"-c", `p=$(cat); case "$p" in *"You are Alpha"*) echo a > a.txt ;; *) echo b > b.txt ;; esac; echo worked`}
	}}
	defer delete(agents.Runtimes, "fakemgr")
	defer delete(agents.Runtimes, "fakeworker")

	repo := newRepo(t)
	p, _ := o.Store.CreateProject(store.Project{Name: "t", Repo: repo, Base: "main"})
	err := o.Store.SaveArrangement(p.ID, []store.Agent{
		{ID: "m", Name: "Lead", Role: "manager", Runtime: "fakemgr"},
		{ID: "a", Name: "Alpha", Role: "coder", Parent: "m", Runtime: "fakeworker"},
		{ID: "b", Name: "Beta", Role: "coder", Parent: "m", Runtime: "fakeworker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	o.Store.SaveGoal("m", store.Goal{Title: "both files", Checks: "test -f a.txt && test -f b.txt"})
	if err := o.Start("m"); err != nil {
		t.Fatal(err)
	}
	if g := waitDone(t, o, "m"); g.Status != "done" {
		es, _ := o.Store.Events("m", 50)
		t.Fatalf("manager: %+v\nevents: %+v", g, es)
	}
	// children got their subgoals from the plan and finished
	for _, id := range []string{"a", "b"} {
		if g, _ := o.Store.Goal(id); g.Status != "done" || len(g.Base) != 40 {
			t.Fatalf("%s: %+v", id, g)
		}
	}
	// a child's diff still shows its work after the manager merged it
	ga, _ := o.Store.Goal("a")
	if fs, err := git.Diff(o.Dir("a"), ga.Base); err != nil || len(fs) != 1 || fs[0].Path != "a.txt" {
		t.Fatalf("a's diff after merge: %+v %v", fs, err)
	}
	// b was rejected once, so it ran twice; a ran once
	var runsA, runsB int
	o.Store.SQL().QueryRow(`SELECT count(*) FROM runs WHERE agent_id='a'`).Scan(&runsA)
	o.Store.SQL().QueryRow(`SELECT count(*) FROM runs WHERE agent_id='b'`).Scan(&runsB)
	if runsA != 1 || runsB != 2 {
		t.Fatalf("runs a=%d b=%d", runsA, runsB)
	}
	// the manager's branch holds both files, and its decision calls left no edits behind
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, err := git.Run(repo, "cat-file", "-e", "arranger/m:"+f); err != nil {
			t.Fatalf("%s not merged into the manager: %v", f, err)
		}
	}
	if _, err := git.Run(repo, "cat-file", "-e", "arranger/m:junk.txt"); err == nil {
		t.Fatal("manager's own file edits should be discarded")
	}
	var decisions int
	o.Store.SQL().QueryRow(`SELECT count(*) FROM decisions WHERE agent_id='m'`).Scan(&decisions)
	if decisions != 3 { // plan + two reviews
		t.Fatalf("decisions: %d", decisions)
	}
}

func TestValidatePlan(t *testing.T) {
	team := map[string]store.Agent{"a": {}, "b": {}}
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
	o := setup(t)
	plain := func(l []byte) []agents.Event { return []agents.Event{{Kind: "msg", Text: string(l)}} }
	agents.Runtimes["fakemgr2"] = agents.Runtime{Bin: "sh", Parse: plain, Args: func(string) []string {
		return []string{"-c", `cat >/dev/null; echo '{"subgoals":[{"agent":"s","title":"sleep","checks":["true"]}]}'`}
	}}
	agents.Runtimes["sleeper"] = agents.Runtime{Bin: "sh", Parse: plain, Args: func(string) []string { return []string{"-c", "cat >/dev/null; sleep 30"} }}
	defer delete(agents.Runtimes, "fakemgr2")
	defer delete(agents.Runtimes, "sleeper")

	p, _ := o.Store.CreateProject(store.Project{Name: "t", Repo: newRepo(t), Base: "main"})
	o.Store.SaveArrangement(p.ID, []store.Agent{
		{ID: "m", Name: "M", Role: "manager", Runtime: "fakemgr2"},
		{ID: "s", Name: "S", Role: "coder", Parent: "m", Runtime: "sleeper"},
	})
	o.Store.SaveGoal("m", store.Goal{Title: "x", Checks: "true"})
	if err := o.Start("m"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); !o.IsRunning("s"); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("child never started")
		}
	}
	start := time.Now()
	o.Stop("m")
	if g := waitDone(t, o, "m"); g.Status != "stopped" {
		t.Fatalf("manager: %+v", g)
	}
	if g, _ := o.Store.Goal("s"); g.Status != "stopped" {
		t.Fatalf("child: %+v", g)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("stop took too long")
	}
}

// Soft limit warns once; hard limit kills the agent mid-run and fails the goal with a clear error.
func TestTokenLimits(t *testing.T) {
	o := setup(t)
	// each assistant message reports 100 tokens; the same message repeated must not double count
	agents.Runtimes["chatty"] = agents.Runtime{Bin: "sh", Parse: agents.Runtimes["claude"].Parse, Args: func(string) []string {
		return []string{"-c", `cat >/dev/null
for i in 1 2 3 4 5; do
  echo '{"type":"assistant","message":{"id":"m'$i'","content":[{"type":"text","text":"step '$i'"}],"usage":{"input_tokens":90,"output_tokens":10}}}'
  echo '{"type":"assistant","message":{"id":"m'$i'","content":[],"usage":{"input_tokens":90,"output_tokens":10}}}'
  sleep 0.2
done
sleep 30`}
	}}
	defer delete(agents.Runtimes, "chatty")

	p, _ := o.Store.CreateProject(store.Project{Name: "t", Repo: newRepo(t), Base: "main"})
	o.Store.SaveArrangement(p.ID, []store.Agent{{ID: "w", Name: "W", Role: "coder", Runtime: "chatty"}})
	a, _, _ := o.Store.Agent("w")
	a.Soft, a.Hard = 150, 300
	o.Store.UpdateAgent(a)
	o.Store.SaveGoal("w", store.Goal{Title: "talk", Checks: "true"})
	start := time.Now()
	if err := o.Start("w"); err != nil {
		t.Fatal(err)
	}
	g := waitDone(t, o, "w")
	if g.Status != "failed" || !strings.Contains(g.Feedback, "hard limit of 300") {
		t.Fatalf("goal: %+v", g)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("hard limit should stop the agent right away, not after it finishes")
	}
	s, _ := o.Store.Summary(p.ID)
	if st := s.Agents["w"]; st.Tokens != 300 || st.AllTokens != 300 {
		t.Fatalf("tokens counted: %+v", st)
	}
	var warns int
	o.Store.SQL().QueryRow(`SELECT count(*) FROM events WHERE agent_id='w' AND kind='warn'`).Scan(&warns)
	if warns != 1 {
		t.Fatalf("soft limit warnings: %d", warns)
	}
}

// setup returns an orchestrator on a fresh database and worktree root.
func setup(t *testing.T) *Orchestrator {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, filepath.Join(dir, "worktrees"), 4)
}

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
	if _, err := git.Commit(repo, "a"); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestStopAll(t *testing.T) {
	o := setup(t)
	repo := newRepo(t)
	p, _ := o.Store.CreateProject(store.Project{Name: "x", Repo: repo, Base: "main"})
	o.Store.SaveArrangement(p.ID, []store.Agent{{ID: "s", Name: "S", Runtime: "generic", Args: "sleep 30"}})
	o.Store.SaveGoal("s", store.Goal{Title: "wait", Checks: "true"})
	if err := o.Start("s"); err != nil {
		t.Fatal(err)
	}
	if !o.StopAll(10 * time.Second) {
		t.Fatal("run still going after StopAll")
	}
	if g, _ := o.Store.Goal("s"); g.Status != "stopped" {
		t.Fatalf("status %q, want stopped", g.Status)
	}
}

// Asking a manager for a change after its team worked re-runs only the reports the change
// concerns: they keep their goal and work, get their part of the change (and any check that
// proves it), and the result is reviewed, merged and checked like any run.
func TestRequestChanges(t *testing.T) {
	o := setup(t)
	plain := func(l []byte) []agents.Event { return []agents.Event{{Kind: "msg", Text: string(l)}} }
	agents.Runtimes["fakemgr"] = agents.Runtime{Bin: "sh", Parse: plain, Args: func(string) []string {
		return []string{"-c", `p=$(cat)
case "$p" in
*"WHAT EACH ONE ALREADY DID"*) echo '{"changes":[{"agent":"a","change":"write A in capitals","checks":["grep -q A a.txt"]}]}' ;;
*"YOUR TEAM:"*) echo '{"subgoals":[{"agent":"a","title":"write a","checks":["test -f a.txt"]},{"agent":"b","title":"write b","checks":["test -f b.txt"]}]}' ;;
*"change the user asked for: write A in capitals"*) echo '{"verdicts":[{"agent":"a","accept":true}]}' ;;
*) echo '{"verdicts":[{"agent":"a","accept":true},{"agent":"b","accept":true}]}' ;;
esac`}
	}}
	agents.Runtimes["fakeworker"] = agents.Runtime{Bin: "sh", Parse: plain, Args: func(string) []string {
		return []string{"-c", `p=$(cat)
case "$p" in
*"USER ASKS FOR THESE CHANGES"*"write A in capitals"*) echo A > a.txt ;;
*"You are Alpha"*) echo a > a.txt ;;
*) echo b > b.txt ;;
esac`}
	}}
	defer delete(agents.Runtimes, "fakemgr")
	defer delete(agents.Runtimes, "fakeworker")

	repo := newRepo(t)
	p, _ := o.Store.CreateProject(store.Project{Name: "t", Repo: repo, Base: "main"})
	o.Store.SaveArrangement(p.ID, []store.Agent{
		{ID: "m", Name: "Lead", Role: "manager", Runtime: "fakemgr"},
		{ID: "a", Name: "Alpha", Role: "coder", Parent: "m", Runtime: "fakeworker"},
		{ID: "b", Name: "Beta", Role: "coder", Parent: "m", Runtime: "fakeworker"},
	})
	o.Store.SaveGoal("m", store.Goal{Title: "both files", Checks: "test -f a.txt && test -f b.txt"})
	if err := o.Revise("m", "  "); err == nil {
		t.Fatal("an empty change request should be refused")
	}
	if err := o.Start("m"); err != nil {
		t.Fatal(err)
	}
	if g := waitDone(t, o, "m"); g.Status != "done" {
		t.Fatalf("first run: %+v", g)
	}

	if err := o.Revise("m", "Alpha's file should say A, in capitals"); err != nil {
		t.Fatal(err)
	}
	if g := waitDone(t, o, "m"); g.Status != "done" {
		es, _ := o.Store.Events("m", 50)
		t.Fatalf("revision: %+v\nevents: %+v", g, es)
	}
	var runsA, runsB int
	o.Store.SQL().QueryRow(`SELECT count(*) FROM runs WHERE agent_id='a'`).Scan(&runsA)
	o.Store.SQL().QueryRow(`SELECT count(*) FROM runs WHERE agent_id='b'`).Scan(&runsB)
	if runsA != 2 || runsB != 1 {
		t.Fatalf("only Alpha should re-run: runs a=%d b=%d", runsA, runsB)
	}
	if ga, _ := o.Store.Goal("a"); ga.Title != "write a" || !strings.Contains(ga.Checks, "grep -q A a.txt") || !strings.Contains(ga.Checks, "test -f a.txt") {
		t.Fatalf("Alpha keeps its goal and gains the change's check: %+v", ga)
	}
	if out, _ := git.Run(repo, "show", "arranger/m:a.txt"); strings.TrimSpace(out) != "A" {
		t.Fatalf("the change should be merged into the manager: a.txt = %q", out)
	}
	if _, err := git.Run(repo, "cat-file", "-e", "arranger/m:b.txt"); err != nil {
		t.Fatal("Beta's earlier work should still be there")
	}
	var revisions int
	o.Store.SQL().QueryRow(`SELECT count(*) FROM decisions WHERE agent_id='m' AND kind='revision'`).Scan(&revisions)
	es, _ := o.Store.Events("m", 100)
	asked := false
	for _, e := range es {
		asked = asked || e.Kind == "request" && strings.Contains(e.Text, "in capitals")
	}
	if revisions != 1 || !asked {
		t.Fatalf("the request and the manager's routing should be on record: revisions=%d asked=%v", revisions, asked)
	}
}

func TestValidateRevision(t *testing.T) {
	team := map[string]store.Agent{"a": {ID: "a"}, "b": {ID: "b"}}
	goals := map[string]store.Goal{"a": {Title: "x"}}
	ok := []revision{{Agent: "a", Change: "do y"}}
	if err := validateRevision(ok, team, goals); err != nil {
		t.Fatal(err)
	}
	for name, cs := range map[string][]revision{
		"none":      nil,
		"stranger":  {{Agent: "z", Change: "y"}},
		"twice":     {{Agent: "a", Change: "y"}, {Agent: "a", Change: "z"}},
		"empty":     {{Agent: "a", Change: " "}},
		"never ran": {{Agent: "b", Change: "y"}},
	} {
		if validateRevision(cs, team, goals) == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// When the team's merged work fails the manager's own checks, the manager passes the failure to the
// report it concerns, which fixes it; the merged result then passes. Reports it doesn't name don't re-run.
func TestManagerFixesFailingChecks(t *testing.T) {
	for _, tc := range []struct {
		name, check, status string
		runsA               int
	}{
		{"fixed", "grep -q fixed a.txt", "done", 2},
		{"gives up", "false", "failed", 1 + maxFixRounds},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := setup(t)
			plain := func(l []byte) []agents.Event { return []agents.Event{{Kind: "msg", Text: string(l)}} }
			agents.Runtimes["fixmgr"] = agents.Runtime{Bin: "sh", Parse: plain, Args: func(string) []string {
				return []string{"-c", `p=$(cat)
case "$p" in
*"your own checks fail on the merged result"*) echo '{"changes":[{"agent":"a","change":"write fixed into a.txt"}]}' ;;
*"YOUR TEAM:"*) echo '{"subgoals":[{"agent":"a","title":"write a","checks":["test -f a.txt"]},{"agent":"b","title":"write b","checks":["test -f b.txt"]}]}' ;;
*) echo '{"verdicts":[]}' ;;
esac`}
			}}
			agents.Runtimes["fixworker"] = agents.Runtime{Bin: "sh", Parse: plain, Args: func(string) []string {
				return []string{"-c", `p=$(cat)
case "$p" in
*"ITS CHECKS FAIL"*"write fixed into a.txt"*) echo fixed > a.txt ;;
*"You are Alpha"*) echo a > a.txt ;;
*) echo b > b.txt ;;
esac`}
			}}
			defer delete(agents.Runtimes, "fixmgr")
			defer delete(agents.Runtimes, "fixworker")

			repo := newRepo(t)
			p, _ := o.Store.CreateProject(store.Project{Name: "t", Repo: repo, Base: "main"})
			o.Store.SaveArrangement(p.ID, []store.Agent{
				{ID: "m", Name: "Lead", Role: "manager", Runtime: "fixmgr"},
				{ID: "a", Name: "Alpha", Role: "coder", Parent: "m", Runtime: "fixworker"},
				{ID: "b", Name: "Beta", Role: "coder", Parent: "m", Runtime: "fixworker"},
			})
			o.Store.SaveGoal("m", store.Goal{Title: "both files", Checks: tc.check})
			if err := o.Start("m"); err != nil {
				t.Fatal(err)
			}
			g := waitDone(t, o, "m")
			if g.Status != tc.status {
				es, _ := o.Store.Events("m", 60)
				eb, _ := o.Store.Events("b", 60)
				t.Fatalf("manager: %+v\nevents: %+v\nbeta: %+v", g, es, eb)
			}
			var runsA, runsB, fixes int
			o.Store.SQL().QueryRow(`SELECT count(*) FROM runs WHERE agent_id='a'`).Scan(&runsA)
			o.Store.SQL().QueryRow(`SELECT count(*) FROM runs WHERE agent_id='b'`).Scan(&runsB)
			o.Store.SQL().QueryRow(`SELECT count(*) FROM decisions WHERE agent_id='m' AND kind='fix'`).Scan(&fixes)
			if runsA != tc.runsA || runsB != 1 || fixes != tc.runsA-1 {
				t.Fatalf("runs a=%d b=%d, fix rounds %d", runsA, runsB, fixes)
			}
			if tc.status == "done" {
				if out, _ := git.Run(repo, "show", "arranger/m:a.txt"); strings.TrimSpace(out) != "fixed" {
					t.Fatalf("the fix should be merged into the manager: a.txt = %q", out)
				}
			} else if !strings.Contains(g.Feedback, fmt.Sprintf("after %d fix rounds", maxFixRounds)) {
				t.Fatalf("feedback: %q", g.Feedback)
			}
		})
	}
}

// alive reports whether the process whose pid is in file is still running.
func alive(t *testing.T, file string) bool {
	t.Helper()
	var b []byte
	for deadline := time.Now().Add(5 * time.Second); len(strings.TrimSpace(string(b))) == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no pid in %s", file)
		}
		b, _ = os.ReadFile(file)
	}
	return exec.Command("kill", "-0", strings.TrimSpace(string(b))).Run() == nil
}

// A check that leaves a process running in the background still finishes, and the process is killed.
func TestCheckWithBackgroundProcess(t *testing.T) {
	o := setup(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	j := &job{o: o, ctx: context.Background(), a: store.Agent{ID: "w", Name: "W"}}
	start := time.Now()
	passed, fb := runChecks(j, t.TempDir(), []string{"sleep 30 & echo $! > " + pidFile + "; echo started"})
	if passed != 1 {
		t.Fatalf("the check itself passed: %s", fb)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("the check waited for its background process")
	}
	for deadline := time.Now().Add(3 * time.Second); alive(t, pidFile); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the background process outlived its check")
		}
	}
}

// Whatever an agent starts goes away with it: when it's stopped, and when it exits on its own.
func TestAgentProcessGroup(t *testing.T) {
	for _, stop := range []bool{true, false} {
		o := setup(t)
		pidFile := filepath.Join(t.TempDir(), "pid")
		script := "cat >/dev/null; sleep 300 & echo $! > " + pidFile + "; echo started; "
		if stop {
			script += "sleep 300"
		}
		agents.Runtimes["spawner"] = agents.Runtime{Bin: "sh", Parse: func(l []byte) []agents.Event { return []agents.Event{{Kind: "msg", Text: string(l)}} },
			Args: func(string) []string { return []string{"-c", script} }}
		p, _ := o.Store.CreateProject(store.Project{Name: "t", Repo: newRepo(t), Base: "main"})
		o.Store.SaveArrangement(p.ID, []store.Agent{{ID: "w", Name: "W", Role: "coder", Runtime: "spawner"}})
		o.Store.SaveGoal("w", store.Goal{Title: "spawn", Checks: "true"})
		start := time.Now()
		if err := o.Start("w"); err != nil {
			t.Fatal(err)
		}
		if stop {
			alive(t, pidFile) // wait until it's up
			o.Stop("w")
		}
		g := waitDone(t, o, "w")
		if want := map[bool]string{true: "stopped", false: "done"}[stop]; g.Status != want {
			t.Fatalf("stop=%v: status %q", stop, g.Status)
		}
		if time.Since(start) > 15*time.Second {
			t.Fatalf("stop=%v: the run waited on the background process", stop)
		}
		for deadline := time.Now().Add(3 * time.Second); alive(t, pidFile); time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("stop=%v: the agent's background process is still running", stop)
			}
		}
		delete(agents.Runtimes, "spawner")
	}
}

// Stderr lines that arrive in pieces come out whole.
func TestLineWriter(t *testing.T) {
	var got []string
	w := &lineWriter{emit: func(l string) { got = append(got, l) }}
	w.Write([]byte("hel"))
	w.Write([]byte("lo wor"))
	w.Write([]byte("ld\nsecond\nthi"))
	w.Write([]byte("rd"))
	w.Flush()
	if strings.Join(got, "|") != "hello world|second|third" {
		t.Fatalf("lines: %q", got)
	}
}

// A manager's reply often has prose or code with braces around the JSON; the plan is still found.
func TestExtractJSONAmongBraces(t *testing.T) {
	for name, reply := range map[string]string{
		"brace in prose":   "I'll split it {roughly} in two:\n{\"subgoals\":[{\"agent\":\"a\",\"title\":\"x\",\"checks\":[\"true\"]}]}",
		"code before":      "The handler is `func h() { return }`. Plan:\n```json\n{\"subgoals\":[{\"agent\":\"a\",\"title\":\"x\",\"checks\":[\"true\"]}]}\n```\nDone {ok}.",
		"brace after":      "{\"subgoals\":[{\"agent\":\"a\",\"title\":\"x\",\"checks\":[\"true\"]}]}\nNote: map[string]int{} is fine.",
		"draft then final": "First try: {\"subgoals\":[]}\nBetter:\n{\"subgoals\":[{\"agent\":\"a\",\"title\":\"x\",\"checks\":[\"true\"]}]}",
	} {
		var v struct{ Subgoals []subgoal }
		if err := extractJSON(reply, &v); err != nil || len(v.Subgoals) != 1 || v.Subgoals[0].Title != "x" {
			t.Errorf("%s: %v %+v", name, err, v)
		}
	}
	var v struct{ Subgoals []subgoal }
	if extractJSON("no json here {at all}", &v) == nil {
		t.Error("a reply without a JSON object should be an error")
	}
}

// A big change is never cut mid-file for review: every file is listed, whole files are shown up to
// the limit, and the rest are named with the command that shows them.
func TestReviewDiff(t *testing.T) {
	small := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-x\n+y\n"
	big := "diff --git a/big.go b/big.go\n--- a/big.go\n+++ b/big.go\n@@ -0,0 +1,1 @@\n+" + strings.Repeat("z", maxDiffInReview) + "\n"
	got := reviewDiff(small+big, "w", "abc123")
	for _, want := range []string{"a.go (+1 -1)", "big.go (+1 -0)", "+y", "1 file(s) too big to show here: big.go", "git diff abc123 arranger/w -- <file>"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%.400s", want, got)
		}
	}
	if strings.Contains(got, "zzzz") {
		t.Error("the file that doesn't fit shouldn't be half shown")
	}
	if got := reviewDiff(small, "w", "b"); strings.Contains(got, "too big") || !strings.Contains(got, "+y") {
		t.Errorf("a small diff is shown whole: %s", got)
	}
}

// An agent moved to another manager keeps its own work and drops its old manager's, instead of
// showing the old manager's work as its own.
func TestMoveToAnotherManager(t *testing.T) {
	o := setup(t)
	repo := newRepo(t)
	p, _ := o.Store.CreateProject(store.Project{Name: "t", Repo: repo, Base: "main"})
	arrange := func(parent string) {
		o.Store.SaveArrangement(p.ID, []store.Agent{
			{ID: "m1", Name: "M1", Role: "manager"}, {ID: "m2", Name: "M2", Role: "manager"},
			{ID: "c", Name: "C", Role: "coder", Parent: parent},
		})
	}
	arrange("m1")
	agent := func(id string) store.Agent { a, _, _ := o.Store.Agent(id); return a }
	work := func(id, file string) {
		dir, _, _, err := o.Sync(p, agent(id))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, file), []byte(id+"\n"), 0o644)
		if _, err := git.Commit(dir, id+" work"); err != nil {
			t.Fatal(err)
		}
	}
	work("m1", "m1.txt")
	work("m2", "m2.txt")
	work("c", "c.txt") // under M1: has m1.txt too

	arrange("m2")
	dir, base, _, err := o.Sync(p, agent("c"))
	if err != nil {
		t.Fatal(err)
	}
	fs, _ := git.Diff(dir, base)
	if len(fs) != 1 || fs[0].Path != "c.txt" {
		t.Fatalf("after the move its diff should be only its own work: %+v", fs)
	}
	for file, want := range map[string]bool{"c.txt": true, "m2.txt": true, "m1.txt": false} {
		if _, err := os.Stat(filepath.Join(dir, file)); (err == nil) != want {
			t.Errorf("%s present=%v, want %v", file, err == nil, want)
		}
	}
	if g, _ := o.Store.Goal("c"); g.From != "arranger/m2" {
		t.Fatalf("from: %q", g.From)
	}
}

// A page too slow for the live updates is told it missed some, once.
func TestHubReportsDrops(t *testing.T) {
	h := NewHub()
	c := h.Subscribe("p")
	if h.Dropped(c) {
		t.Fatal("nothing dropped yet")
	}
	for range cap(c) + 5 {
		h.Publish("p", map[string]any{"type": "status"})
	}
	if !h.Dropped(c) || h.Dropped(c) {
		t.Fatal("drops should be reported exactly once")
	}
}

// Everything one run logs shares a session, attempts included, so the Logs tab can show it as
// one run; the next run gets a new one.
func TestEventsGroupedByRun(t *testing.T) {
	o := setup(t)
	p, _ := o.Store.CreateProject(store.Project{Name: "t", Repo: newRepo(t), Base: "main"})
	o.Store.SaveArrangement(p.ID, []store.Agent{{ID: "w", Name: "W", Role: "programmer", Runtime: "generic", Args: "true"}})
	o.Store.SaveGoal("w", store.Goal{Title: "t", Checks: "test -f nope"}) // fails, so it retries
	for range 2 {
		if err := o.Start("w"); err != nil {
			t.Fatal(err)
		}
		waitDone(t, o, "w")
	}
	es, _ := o.Store.Events("w", 500)
	sessions, attempts := map[int64]bool{}, map[int]bool{}
	for _, e := range es {
		if e.Session == 0 {
			t.Fatalf("event without a session: %+v", e)
		}
		sessions[e.Session] = true
		attempts[e.Attempt] = true
	}
	if len(sessions) != 2 || !attempts[1] || !attempts[3] {
		t.Fatalf("sessions %v, attempts %v", sessions, attempts)
	}
}
