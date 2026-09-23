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

type verdict struct {
	Agent    string `json:"agent"`
	Accept   bool   `json:"accept"`
	Feedback string `json:"feedback"`
}

// runManager plans subgoals for its children, runs them in parallel, reviews and merges
// their verified work into its own branch, then runs its own checks on the merged result.
func runManager(j *job, g store.Goal, dir string, kids []store.Agent, feedback string) string {
	st := j.o.Store
	j.status("planning", nil)
	byID := map[string]store.Agent{}
	for _, k := range kids {
		byID[k.ID] = k
	}

	// 1. plan
	var plan struct{ Subgoals []subgoal }
	j.newRun(1)
	err := j.decide(planPrompt(j.a, g, kids, feedback), dir, &plan, func() error { return validatePlan(plan.Subgoals, byID) })
	if j.ctx.Err() != nil {
		return j.fail("stopped", "stopped by user")
	}
	if errors.Is(err, errTokenLimit) {
		return j.fail("failed", j.limitMsg())
	}
	if err != nil {
		return j.fail("failed", "no valid plan: "+err.Error())
	}
	st.AddDecision(j.a.ID, "plan", plan)
	var team []store.Agent
	for _, s := range plan.Subgoals {
		st.SaveGoal(s.Agent, store.Goal{Title: s.Title, Body: s.Body, Criteria: s.Criteria, Checks: strings.Join(s.Checks, "\n")})
		st.SetGoal(s.Agent, map[string]any{"status": "idle", "attempts": 0, "feedback": "", "passed": 0, "total": 0})
		team = append(team, byID[s.Agent])
		j.emit("plan", fmt.Sprintf("%s → %s (checks: %s)", byID[s.Agent].Name, s.Title, strings.Join(s.Checks, "; ")), "")
	}

	// 2. run children, review, merge; rejected children re-run with the review feedback
	fb := map[string]string{}
	for round := 1; len(team) > 0; round++ {
		j.status("waiting", nil)
		results := runChildren(j, team, fb)
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
		verdicts, err := j.review(g, dir, done)
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

// runChildren executes the given children in parallel and returns each one's final status.
func runChildren(j *job, kids []store.Agent, feedback map[string]string) map[string]string {
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
		wg.Add(1)
		go func(k store.Agent) {
			defer wg.Done()
			defer j.o.unregister(k.ID)
			defer cancel()
			s := j.o.execute(&job{o: j.o, ctx: ctx, p: j.p, a: k}, feedback[k.ID])
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

func (j *job) review(g store.Goal, dir string, ids []string) (map[string]verdict, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, a manager reviewing your team's work toward: %s\n", j.a.Name, g.Title)
	b.WriteString("Every item below already passed its checks. Judge the substance: accept only if the change does what its subgoal asks, meets its criteria, and changes nothing unrelated. When you reject, say exactly what to fix.\n")
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
		fmt.Fprintf(&b, "\n=== agent %q (%s)\nsubgoal: %s\ncriteria: %s\ndiff:\n%s\n", id, k.Name, kg.Title, kg.Criteria, d)
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
