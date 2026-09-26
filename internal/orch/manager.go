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
	resumeFix := false             // continuing a fix round
	if j.cont {
		// continuing a blocked run: the same team, work and changes as its last plan; no new plan
		var err error
		if team, resumeFix, err = j.lastTeam(byID, changes); err != nil {
			return j.fail("failed", err.Error())
		}
		j.emit("plan", "continuing with the same plan: "+names(team), "")
	} else {
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
	}

	// 2. run children, review, merge; rejected children re-run with the review feedback. A continued
	// run keeps the work of the reports that already finished and runs only the others.
	if s := j.runTeam(g, dir, team, byID, changes, resumeFix, j.cont); s != "" {
		return s
	}

	// 3. verify the merged result against the manager's own goal. When it fails, the manager
	// passes the failure to the reports it concerns, they fix it, and the result is checked again.
	checks := lines(g.Checks)
	for round := 0; ; round++ {
		j.status("verifying", nil)
		passed, fbk := runChecks(j, dir, checks)
		st.SetGoal(j.a.ID, map[string]any{"passed": passed, "total": len(checks)})
		if j.ctx.Err() != nil {
			return j.fail("stopped", "stopped by user")
		}
		if passed == len(checks) {
			j.emit("done", fmt.Sprintf("team work merged, all %d checks passed", passed), "")
			return j.status("done", map[string]any{"feedback": ""})
		}
		if round == maxFixRounds {
			return j.fail("failed", fmt.Sprintf("merged work still fails the manager's checks after %d fix rounds:\n%s", round, fbk))
		}
		j.emit("warn", fmt.Sprintf("merged work fails my checks; passing the failure to the team (fix %d/%d)", round+1, maxFixRounds), "")
		j.status("fixing", nil)
		j.newRun(1)
		for _, k := range kids {
			goals[k.ID], _ = st.Goal(k.ID)
		}
		cs, err := j.proposeFix(g, dir, kids, byID, goals, fbk)
		if j.ctx.Err() != nil {
			return j.fail("stopped", "stopped by user")
		}
		if errors.Is(err, errTokenLimit) {
			return j.fail("failed", j.limitMsg())
		}
		if err != nil {
			return j.fail("failed", "merged work fails the manager's checks, and it couldn't pass the fix to its team ("+err.Error()+"):\n"+fbk)
		}
		fixes := map[string]string{}
		team := j.applyRevision("fix", cs, byID, goals, fixes)
		if s := j.runTeam(g, dir, team, byID, fixes, true, false); s != "" {
			return s
		}
	}
}

// runTeam runs the given reports in parallel, reviews their work and merges what it accepts;
// rejected reports re-run with the review's feedback. changes holds each report's part of a
// requested change (fix: of a fix for the manager's failing checks). It returns "" once all the
// work is merged, else the manager's final status.
//
// With keepDone, reports whose goal is already done aren't run again in the first round; they go
// straight to review with the others.
func (j *job) runTeam(g store.Goal, dir string, team []store.Agent, byID map[string]store.Agent, changes map[string]string, fix, keepDone bool) string {
	st := j.o.Store
	fb := map[string]string{}
	for round := 1; len(team) > 0; round++ {
		j.status("waiting", nil)
		run, results := team, map[string]string{}
		if keepDone && round == 1 {
			run = nil
			for _, k := range team {
				if kg, _ := st.Goal(k.ID); kg.Status == "done" {
					results[k.ID] = "done"
					j.emit("msg", k.Name+" already finished; keeping its work", "")
				} else {
					run = append(run, k)
				}
			}
		}
		for id, s := range runChildren(j, run, fb, changes, fix) {
			results[id] = s
		}
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
		verdicts, err := j.review(g, dir, done, changes, fix)
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
			kg, _ := st.Goal(id)
			msg := mergeMessage(dir, git.BranchOf(id), kg.Title)
			if err := git.MergeBranch(dir, git.BranchOf(id), msg); err != nil {
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

	return ""
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
		return j.applyRevision("revision", cs, byID, goals, changes)
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

// lastTeam rebuilds the team of the manager's last plan, routed change or fix round, with each
// report's part of the change, for continuing a blocked run.
func (j *job) lastTeam(byID map[string]store.Agent, changes map[string]string) (team []store.Agent, fix bool, err error) {
	kind, raw := j.o.Store.LastDecision(j.a.ID, "plan", "revision", "fix")
	if kind == "" {
		return nil, false, errors.New("there's no plan to continue; press Run to plan")
	}
	var d struct {
		Subgoals []subgoal
		Changes  []revision
	}
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return nil, false, err
	}
	add := func(id string) {
		if k, ok := byID[id]; ok {
			team = append(team, k)
		}
	}
	for _, s := range d.Subgoals {
		add(s.Agent)
	}
	for _, c := range d.Changes {
		add(c.Agent)
		changes[c.Agent] = c.Change
	}
	if len(team) == 0 {
		return nil, false, errors.New("the last plan's reports aren't on the team anymore; press Run to plan again")
	}
	return team, kind == "fix", nil
}

func names(as []store.Agent) string {
	ns := make([]string, len(as))
	for i, a := range as {
		ns[i] = a.Name
	}
	return strings.Join(ns, ", ")
}

// proposeFix asks the manager which reports must fix what, now that their merged work fails its
// checks. The answer has the same shape as a routed change.
func (j *job) proposeFix(g store.Goal, dir string, kids []store.Agent, byID map[string]store.Agent, goals map[string]store.Goal,
	failure string) ([]revision, error) {
	var rev revisionDraft
	err := j.decide(fixPrompt(j.a, g, kids, goals, failure), dir, &rev, func() error { return validateRevision(rev.Changes, byID, goals) })
	return rev.Changes, err
}

// applyRevision gives each concerned report its part of the change. They keep their goals (plus
// any check that proves the change) and are the only ones re-run.
// kind is how it's recorded: "revision" for a change the user asked for, "fix" for failing checks.
func (j *job) applyRevision(kind string, cs []revision, byID map[string]store.Agent, goals map[string]store.Goal, changes map[string]string) []store.Agent {
	st := j.o.Store
	st.AddDecision(j.a.ID, kind, revisionDraft{Changes: cs})
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
func runChildren(j *job, kids []store.Agent, feedback, changes map[string]string, fix bool) map[string]string {
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
		kj := &job{o: j.o, ctx: ctx, p: j.p, a: k, change: changes[k.ID], fix: fix && changes[k.ID] != ""}
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

func (j *job) review(g store.Goal, dir string, ids []string, changes map[string]string, fix bool) (map[string]verdict, error) {
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
		d = reviewDiff(d, id, kg.Base)
		fmt.Fprintf(&b, "\n=== agent %q (%s)\nsubgoal: %s\ncriteria: %s\n", id, k.Name, kg.Title, kg.Criteria)
		if c := changes[id]; c != "" && fix {
			fmt.Fprintf(&b, "fix you asked for, because the merged work failed your checks: %s\n", c)
		} else if c != "" {
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

// reviewDiff fits a report's diff into the review: a list of every file it changed, then whole
// files' diffs up to maxDiffInReview bytes. Files that don't fit are named, with the command
// that shows them, so the manager never judges work it can't see.
func reviewDiff(d, id, base string) string {
	files := splitDiff(d)
	var b strings.Builder
	b.WriteString("files changed:\n")
	for _, f := range git.ParseDiff(d) {
		fmt.Fprintf(&b, "  %s (+%d -%d)\n", f.Path, f.Adds, f.Dels)
	}
	var left []string
	size := 0
	for _, f := range files {
		if size+len(f) > maxDiffInReview {
			left = append(left, diffPath(f))
			continue
		}
		size += len(f)
		b.WriteString(f)
	}
	if len(left) > 0 {
		fmt.Fprintf(&b, "\n(%d file(s) too big to show here: %s. Read them with: git diff %s arranger/%s -- <file>)\n",
			len(left), strings.Join(left, ", "), base, id)
	}
	return b.String()
}

// splitDiff splits a unified diff into one chunk per file.
func splitDiff(d string) []string {
	var files []string
	for len(d) > 0 {
		next := strings.Index(d[1:], "\ndiff --git ")
		if next < 0 {
			files = append(files, d)
			break
		}
		files = append(files, d[:next+2])
		d = d[next+2:]
	}
	return files
}

// diffPath is the file a one-file diff chunk is about.
func diffPath(chunk string) string {
	if fs := git.ParseDiff(chunk); len(fs) > 0 {
		return fs[0].Path
	}
	return "?"
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

// extractJSON decodes the manager's answer from its reply: the last complete, non-empty JSON object
// in it. Replies often wrap the answer in prose, code fences or code with braces of their own.
func extractJSON(s string, v any) error {
	var objs []json.RawMessage
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		var raw json.RawMessage
		if dec.Decode(&raw) != nil {
			continue // prose like "{roughly}", or code
		}
		objs = append(objs, raw)
		i += int(dec.InputOffset()) - 1 // skip what's inside it
	}
	for k := len(objs) - 1; k >= 0; k-- {
		var keys map[string]json.RawMessage
		if json.Unmarshal(objs[k], &keys) == nil && len(keys) > 0 { // skips {} from code like map[string]int{}
			return json.Unmarshal(objs[k], v)
		}
	}
	return errors.New("no JSON object in the reply")
}
