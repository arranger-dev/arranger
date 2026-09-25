package orch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"arranger/internal/git"
	"arranger/internal/store"
)

type subgoal struct {
	Agent    string   `json:"agent"`
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	Criteria string   `json:"criteria"`
	Checks   []string `json:"checks"`
}

// revision is one report's part of a change the user asked for.
type revision struct {
	Agent  string   `json:"agent"`
	Change string   `json:"change"`
	Checks []string `json:"checks"` // optional extra checks that prove the change
}

type verdict struct {
	Agent    string `json:"agent"`
	Accept   bool   `json:"accept"`
	Feedback string `json:"feedback"`
}

// runManager plans subgoals for its children, runs them in parallel, reviews and merges
// their verified work into its own branch, then runs its own checks on the merged result.
// Asked for a change after its team already worked, it re-runs only the reports the change
// concerns, each with its part of it, instead of planning again.
func runManager(j *job, g store.Goal, dir string, kids []store.Agent, feedback string) string {
	st := j.o.Store
	j.status("planning", nil)
	byID := map[string]store.Agent{}
	goals := map[string]store.Goal{}
	planned := false
	for _, k := range kids {
		byID[k.ID] = k
		goals[k.ID], _ = st.Goal(k.ID)
		planned = planned || goals[k.ID].Title != ""
	}

	// 1. plan, or route the requested change to the reports it concerns; with approval on, the
	// user approves, edits, sends back or cancels it before anything is saved or run
	var team []store.Agent
	changes := map[string]string{} // report id -> its part of the requested change
	revising := j.change != "" && planned
	planFeedback := feedback
	for {
		j.newRun(1)
		var sgs []subgoal
		var cs []revision
		var err error
		if revising {
			cs, err = j.proposeRevision(g, dir, kids, byID, goals, planFeedback)
		} else {
			sgs, err = j.proposePlan(g, dir, kids, byID, planFeedback)
		}
		if j.ctx.Err() != nil {
			return j.fail("stopped", "stopped by user")
		}
		if errors.Is(err, errTokenLimit) {
			return j.fail("failed", j.limitMsg())
		}
		if err != nil {
			return j.fail("failed", "no valid plan: "+err.Error())
		}
		if !j.a.ApprovePlan {
			team = j.apply(sgs, cs, byID, goals, changes)
			break
		}
		kind, proposed := "plan", any(planDraft{Subgoals: sgs})
		if revising {
			kind, proposed = "revision", revisionDraft{Changes: cs}
		}
		d, ended := j.await(kind, proposed)
		if ended != "" {
			return ended
		}
		if d.Action == "cancel" {
			return j.fail("stopped", "you cancelled the plan")
		}
		if d.Action == "replan" {
			b, _ := json.Marshal(proposed)
			note := "The user sent back your plan without running it"
			if d.Feedback != "" {
				note += " and said: " + d.Feedback
			}
			j.emit("request", "you asked for a new plan"+strings.TrimPrefix(note, "The user sent back your plan without running it"), "")
			planFeedback = strings.TrimSpace(feedback + "\n\n" + note + ".\nThe plan they sent back: " + string(b))
			j.status("planning", nil)
			continue
		}
		edited := false
		if revising {
			edited = !sameJSON(cs, d.Changes)
			sgs, cs = nil, d.Changes
		} else {
			edited = !sameJSON(sgs, d.Subgoals)
			sgs, cs = d.Subgoals, nil
		}
		if edited {
			j.emit("request", "you approved the plan with edits", "")
		} else {
			j.emit("request", "you approved the plan", "")
		}
		team = j.apply(sgs, cs, byID, goals, changes)
		break
	}

	// 2. run children, review, merge; rejected children re-run with the review feedback
	fb := map[string]string{}
	for round := 1; len(team) > 0; round++ {
		j.status("waiting", nil)
		results := runChildren(j, team, fb, changes)
		if j.ctx.Err() != nil {
			return j.fail("stopped", "stopped by user")
		}
		var done, failed []string
		for _, k := range team {
			if results[k.ID] == "done" {
				done = append(done, k.ID)
			} else {
				failed = append(failed, fmt.Sprintf("%s (%s)", k.Name, results[k.ID]))
			}
		}
		// ponytail: any failed child blocks the manager for a human; re-planning around it would be smarter
		if len(failed) > 0 {
			return j.fail("blocked", "needs you: "+strings.Join(failed, ", ")+" did not finish")
		}

		j.status("reviewing", nil)
		j.newRun(round)
		verdicts, err := j.review(g, dir, done, changes)
		if j.ctx.Err() != nil {
			return j.fail("stopped", "stopped by user")
		}
		if errors.Is(err, errTokenLimit) {
			return j.fail("failed", j.limitMsg())
		}
		if err != nil {
			return j.fail("blocked", "needs you: review failed: "+err.Error())
		}
		st.AddDecision(j.a.ID, "review", verdicts)
		var again []store.Agent
		for _, id := range done {
			k, v := byID[id], verdicts[id]
			if !v.Accept {
				j.emit("verdict", "✗ rejected "+k.Name+": "+v.Feedback, "")
				fb[id] = v.Feedback
				again = append(again, k)
				continue
			}
			if err := git.MergeBranch(dir, git.BranchOf(id), "arranger: merge "+k.Name); err != nil {
				return j.fail("blocked", "needs you: merging "+k.Name+" failed: "+err.Error())
			}
			j.emit("verdict", "✓ accepted and merged "+k.Name, "")
		}
		j.o.RefreshDiff(j.a.ID, dir, g.Base)
		if len(again) > 0 && round == maxReviewRounds {
			return j.fail("blocked", fmt.Sprintf("needs you: work still rejected after %d reviews", round))
		}
		team = again
	}

	// 3. verify the merged result against the manager's own goal
	checks := lines(g.Checks)
	j.status("verifying", nil)
	passed, fbk := runChecks(j, dir, checks)
	st.SetGoal(j.a.ID, map[string]any{"passed": passed, "total": len(checks)})
	if j.ctx.Err() != nil {
		return j.fail("stopped", "stopped by user")
	}
	if passed < len(checks) {
		return j.fail("failed", "merged work fails the manager's checks:\n"+fbk)
	}
	j.emit("done", fmt.Sprintf("team work merged, all %d checks passed", passed), "")
	return j.status("done", map[string]any{"feedback": ""})
}

// planDraft and revisionDraft are what a manager proposes, as stored and shown for approval.
type planDraft struct {
	Subgoals []subgoal `json:"subgoals"`
}

type revisionDraft struct {
	Changes []revision `json:"changes"`
}

// proposePlan asks the manager to split its goal into a subgoal per report.
func (j *job) proposePlan(g store.Goal, dir string, kids []store.Agent, byID map[string]store.Agent, feedback string) ([]subgoal, error) {
	var plan planDraft
	err := j.decide(planPrompt(j.a, g, kids, feedback, j.change), dir, &plan, func() error { return validatePlan(plan.Subgoals, byID) })
	return plan.Subgoals, err
}

// apply saves a plan (sgs) or a routed change (cs) to the reports and returns who runs.
func (j *job) apply(sgs []subgoal, cs []revision, byID map[string]store.Agent, goals map[string]store.Goal, changes map[string]string) []store.Agent {
	if cs != nil {
		return j.applyRevision(cs, byID, goals, changes)
	}
	return j.applyPlan(sgs, byID)
}

// applyPlan saves each subgoal as its report's goal.
func (j *job) applyPlan(sgs []subgoal, byID map[string]store.Agent) []store.Agent {
	st := j.o.Store
	st.AddDecision(j.a.ID, "plan", planDraft{Subgoals: sgs})
	var team []store.Agent
	for _, s := range sgs {
		st.SaveGoal(s.Agent, store.Goal{Title: s.Title, Body: s.Body, Criteria: s.Criteria, Checks: strings.Join(s.Checks, "\n")})
		st.SetGoal(s.Agent, map[string]any{"status": "idle", "attempts": 0, "feedback": "", "passed": 0, "total": 0})
		team = append(team, byID[s.Agent])
		j.emit("plan", fmt.Sprintf("%s → %s (checks: %s)", byID[s.Agent].Name, s.Title, strings.Join(s.Checks, "; ")), "")
	}
	return team
}

// proposeRevision asks the manager which reports the user's change concerns and what each must do.
func (j *job) proposeRevision(g store.Goal, dir string, kids []store.Agent, byID map[string]store.Agent, goals map[string]store.Goal,
	feedback string) ([]revision, error) {
	var rev revisionDraft
	err := j.decide(revisePrompt(j.a, g, kids, goals, j.change, feedback), dir, &rev, func() error { return validateRevision(rev.Changes, byID, goals) })
	return rev.Changes, err
}

// applyRevision gives each concerned report its part of the change. They keep their goals (plus
// any check that proves the change) and are the only ones re-run.
func (j *job) applyRevision(cs []revision, byID map[string]store.Agent, goals map[string]store.Goal, changes map[string]string) []store.Agent {
	st := j.o.Store
	st.AddDecision(j.a.ID, "revision", revisionDraft{Changes: cs})
	var team []store.Agent
	for _, c := range cs {
		kg := goals[c.Agent]
		if added := addChecks(&kg, c.Checks); len(added) > 0 {
			st.SaveGoal(c.Agent, kg)
		}
		st.SetGoal(c.Agent, map[string]any{"status": "idle", "attempts": 0, "feedback": "", "passed": 0, "total": 0})
		changes[c.Agent] = c.Change
		team = append(team, byID[c.Agent])
		msg := fmt.Sprintf("%s → change: %s", byID[c.Agent].Name, c.Change)
		if len(c.Checks) > 0 {
			msg += " (new checks: " + strings.Join(c.Checks, "; ") + ")"
		}
		j.emit("plan", msg, "")
	}
	return team
}

// addChecks appends the checks g doesn't have yet and returns them.
func addChecks(g *store.Goal, checks []string) []string {
	have := map[string]bool{}
	for _, c := range lines(g.Checks) {
		have[c] = true
	}
	var added []string
	for _, c := range lines(strings.Join(checks, "\n")) {
		if !have[c] {
			have[c] = true
			added = append(added, c)
		}
	}
	if len(added) > 0 {
		g.Checks = strings.TrimSpace(g.Checks + "\n" + strings.Join(added, "\n"))
	}
	return added
}

func validateRevision(cs []revision, team map[string]store.Agent, goals map[string]store.Goal) error {
	if len(cs) == 0 {
		return errors.New("no member got a change; give the change to the members whose work it concerns")
	}
	seen := map[string]bool{}
	for _, c := range cs {
		if _, ok := team[c.Agent]; !ok {
			return fmt.Errorf("%q is not on your team", c.Agent)
		}
		if seen[c.Agent] {
			return fmt.Errorf("%s got two changes; merge them into one", c.Agent)
		}
		seen[c.Agent] = true
		if strings.TrimSpace(c.Change) == "" {
			return fmt.Errorf("the change for %s is empty", c.Agent)
		}
		if goals[c.Agent].Title == "" {
			return fmt.Errorf("%s has no subgoal yet, so it has no work to change", c.Agent)
		}
	}
	return nil
}

// runChildren executes the given children in parallel and returns each one's final status.
// changes holds each child's part of a change the user asked for, if any.
func runChildren(j *job, kids []store.Agent, feedback, changes map[string]string) map[string]string {
	var mu sync.Mutex
	var wg sync.WaitGroup
	res := map[string]string{}
	for _, k := range kids {
		ctx, cancel := context.WithCancel(j.ctx)
		if !j.o.register(k.ID, cancel) {
			cancel()
			res[k.ID] = "already running"
			continue
		}
		kj := &job{o: j.o, ctx: ctx, p: j.p, a: k, change: changes[k.ID]}
		kj.status("starting", nil) // the hand-off shows on the canvas at once, not after the worktree is ready
		wg.Add(1)
		go func(k store.Agent) {
			defer wg.Done()
			defer j.o.unregister(k.ID)
			defer cancel()
			s := j.o.execute(kj, feedback[k.ID])
			mu.Lock()
			res[k.ID] = s
			mu.Unlock()
		}(k)
	}
	wg.Wait()
	return res
}

// decide asks the manager's CLI for a JSON decision, decodes it into v and checks it with
// valid, re-asking with the error when it's unusable. Managers decide, they don't edit:
// any file changes a decision call makes are thrown away.
func (j *job) decide(prompt, dir string, v any, valid func() error) error {
	for try := 1; ; try++ {
		out, err := runAgent(j, prompt, dir)
		git.Run(dir, "reset", "-q", "--hard", "HEAD")
		git.Run(dir, "clean", "-qfd")
		if err == nil {
			err = extractJSON(out, v)
		}
		if err == nil {
			err = valid()
		}
		if err == nil || j.ctx.Err() != nil || errors.Is(err, errTokenLimit) || try == maxPlanTries {
			return err
		}
		j.emit("error", "reply rejected: "+err.Error(), "")
		prompt += "\n\nYour previous reply was rejected: " + err.Error() + "\nReply again with only the JSON object."
	}
}

func (j *job) review(g store.Goal, dir string, ids []string, changes map[string]string) (map[string]verdict, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, a manager reviewing your team's work toward: %s\n", j.a.Name, g.Title)
	b.WriteString("Every item below already passed its checks. Judge the substance: accept only if the change does what its subgoal asks, meets its criteria, and changes nothing unrelated. Where the user asked for a change, accept only if it was made. When you reject, say exactly what to fix.\n")
	for _, id := range ids {
		k, _, _ := j.o.Store.Agent(id)
		kg, _ := j.o.Store.Goal(id)
		d, err := git.RawDiff(j.o.Dir(id), kg.Base)
		if err != nil {
			return nil, err
		}
		if len(d) > maxDiffInReview {
			d = d[:maxDiffInReview] + "\n… (diff truncated)"
		}
		fmt.Fprintf(&b, "\n=== agent %q (%s)\nsubgoal: %s\ncriteria: %s\n", id, k.Name, kg.Title, kg.Criteria)
		if c := changes[id]; c != "" {
			fmt.Fprintf(&b, "change the user asked for: %s\n", c)
		}
		fmt.Fprintf(&b, "diff:\n%s\n", d)
	}
	b.WriteString("\nReply with ONLY a JSON object, no prose:\n" + `{"verdicts":[{"agent":"<id>","accept":true,"feedback":""}]}`)

	var out struct{ Verdicts []verdict }
	if err := j.decide(b.String(), dir, &out, func() error { return nil }); err != nil {
		return nil, err
	}
	vs := map[string]verdict{}
	for _, v := range out.Verdicts {
		vs[v.Agent] = v
	}
	for _, id := range ids {
		if _, ok := vs[id]; !ok {
			j.emit("verdict", "no verdict for "+id+"; accepted on its passing checks", "")
			vs[id] = verdict{Agent: id, Accept: true}
		}
	}
	return vs, nil
}

func validatePlan(sgs []subgoal, team map[string]store.Agent) error {
	if len(sgs) == 0 {
		return errors.New("the plan has no subgoals")
	}
	seen := map[string]bool{}
	for _, s := range sgs {
		if _, ok := team[s.Agent]; !ok {
			return fmt.Errorf("%q is not on your team", s.Agent)
		}
		if seen[s.Agent] {
			return fmt.Errorf("%s got two subgoals; give each agent one", s.Agent)
		}
		seen[s.Agent] = true
		if strings.TrimSpace(s.Title) == "" {
			return fmt.Errorf("subgoal for %s has no title", s.Agent)
		}
		if len(lines(strings.Join(s.Checks, "\n"))) == 0 {
			return fmt.Errorf("subgoal for %s has no checks; every subgoal needs a shell check", s.Agent)
		}
	}
	return nil
}

// extractJSON decodes the outermost {...} in s, tolerating prose or code fences around it.
func extractJSON(s string, v any) error {
	i, k := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i < 0 || k < i {
		return errors.New("no JSON object in the reply")
	}
	return json.Unmarshal([]byte(s[i:k+1]), v)
}
