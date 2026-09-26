package orch

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"arranger/internal/agents"
	"arranger/internal/store"
)

// approvalTeam sets up a manager "m" that asks for approval, with reports "a" and "b". The fake
// manager plans both reports (a new plan when it gets feedback), routes changes to "a", and
// accepts everything in review. It records each prompt it gets in prompts.
func approvalTeam(t *testing.T) (o *Orchestrator, prompts string) {
	t.Helper()
	o = setup(t)
	prompts = filepath.Join(t.TempDir(), "prompts")
	plain := func(l []byte) []agents.Event { return []agents.Event{{Kind: "msg", Text: string(l)}} }
	agents.Runtimes["apmgr"] = agents.Runtime{Bin: "sh", Parse: plain, Args: func(string) []string {
		return []string{"-c", `p=$(cat); printf '%s\n=====\n' "$p" >> ` + prompts + `
case "$p" in
*"WHAT EACH ONE ALREADY DID"*) echo '{"changes":[{"agent":"a","change":"write A","checks":["grep -q A a.txt"]}]}' ;;
*"sent back your plan"*) echo '{"subgoals":[{"agent":"b","title":"only b","checks":["test -f b.txt"]}]}' ;;
*"YOUR TEAM:"*) echo '{"subgoals":[{"agent":"a","title":"write a","checks":["test -f a.txt"]},{"agent":"b","title":"write b","checks":["test -f b.txt"]}]}' ;;
*) echo '{"verdicts":[]}' ;;
esac`}
	}}
	agents.Runtimes["apworker"] = agents.Runtime{Bin: "sh", Parse: plain, Args: func(string) []string {
		return []string{"-c", `p=$(cat); case "$p" in *"write A"*) echo A > a.txt ;; *"You are Alpha"*) echo a > a.txt ;; *) echo b > b.txt ;; esac`}
	}}
	t.Cleanup(func() { delete(agents.Runtimes, "apmgr"); delete(agents.Runtimes, "apworker") })

	p, _ := o.Store.CreateProject(store.Project{Name: "t", Repo: newRepo(t), Base: "main"})
	if err := o.Store.SaveArrangement(p.ID, []store.Agent{
		{ID: "m", Name: "Lead", Role: "manager", Runtime: "apmgr", ApprovePlan: true},
		{ID: "a", Name: "Alpha", Role: "coder", Parent: "m", Runtime: "apworker"},
		{ID: "b", Name: "Beta", Role: "coder", Parent: "m", Runtime: "apworker"},
	}); err != nil {
		t.Fatal(err)
	}
	o.Store.SaveGoal("b", store.Goal{Title: "b's old goal", Checks: "true"})
	o.Store.SaveGoal("m", store.Goal{Title: "files", Checks: "true"})
	return o, prompts
}

// waitPlan waits until m's plan waits for a decision and returns the draft.
func waitPlan(t *testing.T, o *Orchestrator) store.Plan {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		// the draft is saved just before the status changes, so wait for both
		if p, err := o.Store.PendingPlan("m"); err == nil && o.Awaiting("m") {
			if g, _ := o.Store.Goal("m"); g.Status == "awaiting" {
				return p
			}
		}
	}
	t.Fatal("no plan to approve")
	return store.Plan{}
}

func runs(o *Orchestrator, id string) (n int) {
	o.Store.SQL().QueryRow(`SELECT count(*) FROM runs WHERE agent_id=?`, id).Scan(&n)
	return
}

// Nothing is saved or run until the user approves; the edited plan is what runs.
func TestApproveEditedPlan(t *testing.T) {
	o, _ := approvalTeam(t)
	if err := o.Start("m"); err != nil {
		t.Fatal(err)
	}
	p := waitPlan(t, o)
	var draft planDraft
	json.Unmarshal(p.JSON, &draft)
	if p.Kind != "plan" || len(draft.Subgoals) != 2 {
		t.Fatalf("draft: %+v", p)
	}
	if g, _ := o.Store.Goal("a"); g.Title != "" {
		t.Fatal("a report's goal changed before approval")
	}
	if g, _ := o.Store.Goal("b"); g.Title != "b's old goal" || runs(o, "a")+runs(o, "b") != 0 {
		t.Fatal("reports touched before approval")
	}

	// the user edits a's goal and skips b
	edited := []subgoal{{Agent: "a", Title: "write a, edited", Checks: []string{"test -f a.txt"}}}
	if err := o.Decide("m", Decision{Draft: p.ID + 1, Action: "approve", Subgoals: edited}); !errors.Is(err, ErrStale) {
		t.Fatalf("a wrong draft id should be stale, got %v", err)
	}
	if err := o.Decide("m", Decision{Draft: p.ID, Action: "approve", Subgoals: []subgoal{{Agent: "a", Title: "no checks"}}}); err == nil {
		t.Fatal("a plan without checks should be refused")
	}
	if err := o.Decide("m", Decision{Draft: p.ID, Action: "approve", Subgoals: edited}); err != nil {
		t.Fatal(err)
	}
	if err := o.Decide("m", Decision{Draft: p.ID, Action: "cancel"}); !errors.Is(err, ErrStale) {
		t.Fatal("a second decision should be stale")
	}
	if g := waitDone(t, o, "m"); g.Status != "done" {
		es, _ := o.Store.Events("m", 50)
		t.Fatalf("manager: %+v\n%+v", g, es)
	}
	if g, _ := o.Store.Goal("a"); g.Title != "write a, edited" || g.Status != "done" {
		t.Fatalf("a: %+v", g)
	}
	if g, _ := o.Store.Goal("b"); g.Title != "b's old goal" || runs(o, "b") != 0 {
		t.Fatalf("skipped b should keep its goal and not run: %+v", g)
	}
	var status, saved string
	o.Store.SQL().QueryRow(`SELECT status, json FROM plans WHERE id=?`, p.ID).Scan(&status, &saved)
	if status != "approved" || !strings.Contains(saved, "edited") {
		t.Fatalf("draft after approval: %s %s", status, saved)
	}
}

// Sending a plan back with feedback plans again with that feedback; nothing ran in between.
func TestReplanWithFeedback(t *testing.T) {
	o, prompts := approvalTeam(t)
	o.Start("m")
	first := waitPlan(t, o)
	if err := o.Decide("m", Decision{Draft: first.ID, Action: "replan", Feedback: "Beta can do it alone"}); err != nil {
		t.Fatal(err)
	}
	var second store.Plan
	for deadline := time.Now().Add(20 * time.Second); second.ID <= first.ID; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no second plan")
		}
		second, _ = o.Store.PendingPlan("m")
	}
	b, _ := os.ReadFile(prompts)
	if !strings.Contains(string(b), "Beta can do it alone") || !strings.Contains(string(b), `"title":"write a"`) {
		t.Fatalf("the new plan should get the feedback and the plan sent back:\n%s", b)
	}
	if runs(o, "a")+runs(o, "b") != 0 {
		t.Fatal("reports ran before approval")
	}
	if !strings.Contains(string(second.JSON), "only b") {
		t.Fatalf("second plan: %s", second.JSON)
	}
	o.Stop("m")
	waitDone(t, o, "m")
}

// Cancel, Stop and running out of time all end the run as stopped without touching the reports.
func TestPlanCancelStopExpire(t *testing.T) {
	defer func(d time.Duration) { planTimeout = d }(planTimeout)
	for _, how := range []string{"cancel", "stop", "expire"} {
		t.Run(how, func(t *testing.T) {
			o, _ := approvalTeam(t)
			if how == "expire" {
				planTimeout = 300 * time.Millisecond
			} else {
				planTimeout = time.Hour
			}
			o.Start("m")
			p := waitPlan(t, o)
			switch how {
			case "cancel":
				if err := o.Decide("m", Decision{Draft: p.ID, Action: "cancel"}); err != nil {
					t.Fatal(err)
				}
			case "stop":
				o.Stop("m")
			}
			if g := waitDone(t, o, "m"); g.Status != "stopped" {
				t.Fatalf("manager: %+v", g)
			}
			want := map[string]string{"cancel": "cancelled", "stop": "cancelled", "expire": "expired"}[how]
			var status string
			o.Store.SQL().QueryRow(`SELECT status FROM plans WHERE id=?`, p.ID).Scan(&status)
			if status != want {
				t.Fatalf("draft status %q, want %q", status, want)
			}
			if g, _ := o.Store.Goal("b"); g.Title != "b's old goal" || runs(o, "a")+runs(o, "b") != 0 {
				t.Fatal("reports touched")
			}
			if o.Awaiting("m") {
				t.Fatal("still marked as waiting")
			}
		})
	}
}

// A requested change is routed as a draft too; approving it runs only the reports it names.
func TestApproveRevision(t *testing.T) {
	o, _ := approvalTeam(t)
	o.Start("m")
	p := waitPlan(t, o)
	var draft planDraft
	json.Unmarshal(p.JSON, &draft)
	o.Decide("m", Decision{Draft: p.ID, Action: "approve", Subgoals: draft.Subgoals})
	if g := waitDone(t, o, "m"); g.Status != "done" {
		t.Fatalf("first run: %+v", g)
	}

	if err := o.Revise("m", "capital A please"); err != nil {
		t.Fatal(err)
	}
	p = waitPlan(t, o)
	if p.Kind != "revision" || !strings.Contains(string(p.JSON), "write A") {
		t.Fatalf("revision draft: %+v %s", p, p.JSON)
	}
	var rev revisionDraft
	json.Unmarshal(p.JSON, &rev)
	if err := o.Decide("m", Decision{Draft: p.ID, Action: "approve", Changes: rev.Changes}); err != nil {
		t.Fatal(err)
	}
	if g := waitDone(t, o, "m"); g.Status != "done" {
		t.Fatalf("revision: %+v", g)
	}
	if runs(o, "a") != 2 || runs(o, "b") != 1 {
		t.Fatalf("only a should re-run: a=%d b=%d", runs(o, "a"), runs(o, "b"))
	}
}
