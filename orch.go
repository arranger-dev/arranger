package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	maxAttempts     = 3 // worker: first try + 2 retries fed with the failing checks' output
	maxPlanTries    = 2 // manager: re-ask when the plan/review isn't valid JSON or misses checks
	maxReviewRounds = 3 // manager: review, re-run rejected children, review again...
	maxDiffInReview = 15000
)

var (
	worktreeRoot string                            // set in main
	slots        = make(chan struct{}, 4)          // caps concurrent agent processes; resized in main
	runMu        sync.Mutex                        // guards running
	running      = map[string]context.CancelFunc{} // agent id -> cancel of its run
)

// Hub fans out live updates to SSE subscribers of a project and remembers
// each agent's latest activity line.
type Hub struct {
	mu   sync.Mutex
	subs map[chan []byte]string // chan -> project id
	last map[string]string      // agent id -> latest activity
}

var hub = &Hub{subs: map[chan []byte]string{}, last: map[string]string{}}

func (h *Hub) subscribe(project string) chan []byte {
	c := make(chan []byte, 256)
	h.mu.Lock()
	h.subs[c] = project
	h.mu.Unlock()
	return c
}

func (h *Hub) unsubscribe(c chan []byte) {
	h.mu.Lock()
	delete(h.subs, c)
	h.mu.Unlock()
}

func (h *Hub) publish(project string, v any) {
	b, _ := json.Marshal(v)
	h.mu.Lock()
	defer h.mu.Unlock()
	for c, p := range h.subs {
		if p != project {
			continue
		}
		select {
		case c <- b:
		default: // ponytail: slow client drops live events; the Logs tab refetches on open
		}
	}
}

func (h *Hub) setNow(agent, text string) {
	h.mu.Lock()
	h.last[agent] = text
	h.mu.Unlock()
}

func (h *Hub) now(agent string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last[agent]
}

// job is one agent's run: where it works and how it reports.
type job struct {
	ctx context.Context
	p   Project
	a   Agent
	run int64 // current runs.id, tags events
}

func (j *job) emit(kind, text, raw string) {
	e := LogEvent{Agent: j.a.ID, Run: j.run, Kind: kind, Text: text, Raw: raw}
	addEvent(&e)
	if kind != "stderr" {
		hub.setNow(j.a.ID, kind+": "+clip(text, 80))
	}
	hub.publish(j.p.ID, map[string]any{"type": "event", "event": e})
}

func (j *job) status(s string, kv map[string]any) string {
	if kv == nil {
		kv = map[string]any{}
	}
	kv["status"] = s
	setGoal(j.a.ID, kv)
	hub.publish(j.p.ID, map[string]any{"type": "status", "agent": j.a.ID, "status": s})
	return s
}

func (j *job) fail(s, msg string) string {
	j.emit("error", msg, "")
	return j.status(s, map[string]any{"feedback": msg})
}

func (j *job) newRun(attempt int) {
	res, err := db.Exec(`INSERT INTO runs(agent_id, attempt, started) VALUES(?,?,?)`, j.a.ID, attempt, time.Now().UnixMilli())
	if err == nil {
		j.run, _ = res.LastInsertId()
	}
}

// baseFor is the branch an agent works from: its manager's branch when that exists, else the project base.
func baseFor(p Project, a Agent) string {
	if a.Parent != "" && branchExists(p.Repo, branchOf(a.Parent)) {
		return branchOf(a.Parent)
	}
	return p.Base
}

// workspace returns the agent's worktree and the commit its work is measured from: the base
// branch's tip when the worktree was made. It's kept across runs, so an agent's diff still
// shows its work after a manager merged it.
func workspace(p Project, a Agent) (dir, base string, err error) {
	branch := baseFor(p, a)
	_, statErr := os.Stat(filepath.Join(worktreeRoot, a.ID))
	if dir, err = ensureWorktree(worktreeRoot, p.Repo, branch, a.ID); err != nil {
		return "", "", err
	}
	g, _ := getGoal(a.ID)
	if statErr == nil && shaRe.MatchString(g.Base) {
		if _, err := git(dir, "merge-base", "--is-ancestor", g.Base, "HEAD"); err == nil {
			return dir, g.Base, nil
		}
	}
	out, err := git(p.Repo, "rev-parse", branch)
	if err != nil {
		return "", "", err
	}
	base = strings.TrimSpace(out)
	setGoal(a.ID, map[string]any{"base": base})
	return dir, base, nil
}

func stopGoal(agentID string) bool {
	runMu.Lock()
	defer runMu.Unlock()
	cancel, ok := running[agentID]
	if ok {
		cancel()
	}
	return ok
}

func isRunning(agentID string) bool {
	runMu.Lock()
	defer runMu.Unlock()
	_, ok := running[agentID]
	return ok
}

// register marks an agent as running with cancel; false if it already is.
func register(agentID string, cancel context.CancelFunc) bool {
	runMu.Lock()
	defer runMu.Unlock()
	if _, busy := running[agentID]; busy {
		return false
	}
	running[agentID] = cancel
	return true
}

func unregister(agentID string) {
	runMu.Lock()
	delete(running, agentID)
	runMu.Unlock()
}

// checkGoal reports why a goal can't run, or nil.
func checkGoal(a Agent, g Goal) error {
	if strings.TrimSpace(g.Title) == "" {
		return fmt.Errorf("%s has no goal", a.Name)
	}
	if len(lines(g.Checks)) == 0 {
		return fmt.Errorf("%s's goal needs at least one check: nothing is done until a check passes", a.Name)
	}
	if _, ok := runtimes[a.Runtime]; !ok {
		return fmt.Errorf("unknown runtime %q", a.Runtime)
	}
	return nil
}

// startGoal validates a run and starts it in the background. A manager runs its whole subtree.
func startGoal(agentID string) error {
	a, pid, err := getAgent(agentID)
	if err != nil {
		return fmt.Errorf("agent %s: %w", agentID, err)
	}
	p, err := getProject(pid)
	if err != nil {
		return err
	}
	if p.Repo == "" {
		return errors.New("set a git repository for this project first")
	}
	g, err := getGoal(agentID)
	if err != nil {
		return err
	}
	if err := checkGoal(a, g); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	if !register(agentID, cancel) {
		cancel()
		return fmt.Errorf("%s is already running", a.Name)
	}
	go func() {
		defer unregister(agentID)
		defer cancel()
		execute(&job{ctx: ctx, p: p, a: a}, "")
	}()
	return nil
}

// execute runs an agent's goal and returns its final status. feedback comes from a reviewer.
func execute(j *job, feedback string) string {
	g, err := getGoal(j.a.ID)
	if err == nil {
		err = checkGoal(j.a, g)
	}
	if err != nil {
		return j.fail("failed", err.Error())
	}
	kids, err := listChildren(j.a.ID)
	if err != nil {
		return j.fail("failed", err.Error())
	}
	dir, base, err := workspace(j.p, j.a)
	if err != nil {
		return j.fail("failed", err.Error())
	}
	g.Base = base
	setGoal(j.a.ID, map[string]any{"attempts": 0, "feedback": "", "passed": 0, "total": 0})
	if len(kids) > 0 {
		return runManager(j, g, dir, kids, feedback)
	}
	return runWorker(j, g, dir, feedback)
}

func runWorker(j *job, g Goal, dir, feedback string) string {
	checks := lines(g.Checks)
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		j.status("running", map[string]any{"attempts": attempt})
		j.newRun(attempt)
		j.emit("msg", fmt.Sprintf("attempt %d/%d with %s", attempt, maxAttempts, j.a.Runtime), "")

		_, agentErr := runAgent(j, workerPrompt(j.a, g, dir, checks, feedback), dir)
		if sha, err := checkpoint(dir, fmt.Sprintf("arranger: %s attempt %d", j.a.Name, attempt)); err != nil {
			j.emit("error", err.Error(), "")
		} else if sha != "" {
			j.emit("msg", "checkpoint "+sha, "")
		}
		refreshDiff(j.a.ID, dir, g.Base)
		if j.ctx.Err() != nil {
			return j.fail("stopped", "stopped by user")
		}
		if agentErr != nil {
			j.emit("error", agentErr.Error(), "")
		}

		j.status("verifying", nil)
		passed, fb := runChecks(j, dir, checks)
		setGoal(j.a.ID, map[string]any{"passed": passed, "total": len(checks)})
		if j.ctx.Err() != nil {
			return j.fail("stopped", "stopped by user")
		}
		if passed == len(checks) {
			j.emit("done", fmt.Sprintf("all %d checks passed", passed), "")
			return j.status("done", map[string]any{"feedback": ""})
		}
		feedback = fb
		if agentErr != nil {
			feedback = "The agent process failed: " + agentErr.Error() + "\n" + fb
		}
		setGoal(j.a.ID, map[string]any{"feedback": feedback})
	}
	return j.fail("failed", fmt.Sprintf("checks still failing after %d attempts:\n%s", maxAttempts, feedback))
}

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
func runManager(j *job, g Goal, dir string, kids []Agent, feedback string) string {
	j.status("planning", nil)
	byID := map[string]Agent{}
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
	if err != nil {
		return j.fail("failed", "no valid plan: "+err.Error())
	}
	addDecision(j.a.ID, "plan", plan)
	var team []Agent
	for _, s := range plan.Subgoals {
		saveGoal(s.Agent, Goal{Title: s.Title, Body: s.Body, Criteria: s.Criteria, Checks: strings.Join(s.Checks, "\n")})
		setGoal(s.Agent, map[string]any{"status": "idle", "attempts": 0, "feedback": "", "passed": 0, "total": 0})
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
		var done []string
		var failed []string
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
		if err != nil {
			return j.fail("blocked", "needs you: review failed: "+err.Error())
		}
		addDecision(j.a.ID, "review", verdicts)
		var again []Agent
		for _, id := range done {
			k, v := byID[id], verdicts[id]
			if !v.Accept {
				j.emit("verdict", "✗ rejected "+k.Name+": "+v.Feedback, "")
				fb[id] = v.Feedback
				again = append(again, k)
				continue
			}
			if err := mergeBranch(dir, branchOf(id), "arranger: merge "+k.Name); err != nil {
				return j.fail("blocked", "needs you: merging "+k.Name+" failed: "+err.Error())
			}
			j.emit("verdict", "✓ accepted and merged "+k.Name, "")
		}
		refreshDiff(j.a.ID, dir, g.Base)
		if len(again) > 0 && round == maxReviewRounds {
			return j.fail("blocked", fmt.Sprintf("needs you: work still rejected after %d reviews", round))
		}
		team = again
	}

	// 3. verify the merged result against the manager's own goal
	checks := lines(g.Checks)
	j.status("verifying", nil)
	passed, fbk := runChecks(j, dir, checks)
	setGoal(j.a.ID, map[string]any{"passed": passed, "total": len(checks)})
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
func runChildren(j *job, kids []Agent, feedback map[string]string) map[string]string {
	var mu sync.Mutex
	var wg sync.WaitGroup
	res := map[string]string{}
	for _, k := range kids {
		ctx, cancel := context.WithCancel(j.ctx)
		if !register(k.ID, cancel) {
			cancel()
			res[k.ID] = "already running"
			continue
		}
		wg.Add(1)
		go func(k Agent) {
			defer wg.Done()
			defer unregister(k.ID)
			defer cancel()
			s := execute(&job{ctx: ctx, p: j.p, a: k}, feedback[k.ID])
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
		git(dir, "reset", "-q", "--hard", "HEAD")
		git(dir, "clean", "-qfd")
		if err == nil {
			err = extractJSON(out, v)
		}
		if err == nil {
			err = valid()
		}
		if err == nil || j.ctx.Err() != nil || try == maxPlanTries {
			return err
		}
		j.emit("error", "reply rejected: "+err.Error(), "")
		prompt += "\n\nYour previous reply was rejected: " + err.Error() + "\nReply again with only the JSON object."
	}
}

func (j *job) review(g Goal, dir string, ids []string) (map[string]verdict, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, a manager reviewing your team's work toward: %s\n", j.a.Name, g.Title)
	b.WriteString("Every item below already passed its checks. Judge the substance: accept only if the change does what its subgoal asks, meets its criteria, and changes nothing unrelated. When you reject, say exactly what to fix.\n")
	for _, id := range ids {
		k, _, _ := getAgent(id)
		kg, _ := getGoal(id)
		d, err := rawDiff(filepath.Join(worktreeRoot, id), kg.Base)
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

func validatePlan(sgs []subgoal, team map[string]Agent) error {
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

func refreshDiff(agentID, dir, base string) {
	if fs, err := worktreeDiff(dir, base); err == nil {
		adds, dels := diffStat(fs)
		setGoal(agentID, map[string]any{"adds": adds, "dels": dels})
	}
}

// runAgent runs the agent CLI once, streaming its events. It returns the agent's final answer.
func runAgent(j *job, prompt, dir string) (string, error) {
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-j.ctx.Done():
		return "", j.ctx.Err()
	}
	rt := runtimes[j.a.Runtime]
	// ponytail: cancel kills the agent process only, not its children; use a process group if strays show up.
	cmd, err := rt.command(j.ctx, j.a.Model, j.a.Args, prompt)
	if err != nil {
		return "", err
	}
	cmd.Dir = dir
	cmd.WaitDelay = 5 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	cmd.Stderr = lineWriter(func(l string) { j.emit("stderr", clip(l, 500), "") })
	if err := cmd.Start(); err != nil {
		return "", err
	}
	var cost float64
	var in, out int
	var final, lastMsg, delta string
	emit := j.emit
	flush := func() { // streamed text becomes one message
		if t := strings.TrimSpace(delta); t != "" {
			lastMsg = t
			emit("msg", t, "")
		}
		delta = ""
	}
	r := bufio.NewReader(stdout)
	for {
		line, err := r.ReadBytes('\n')
		if l := strings.TrimSpace(string(line)); l != "" {
			// worktree-relative paths read better; raw keeps the original line
			for i, e := range rt.Parse([]byte(strings.ReplaceAll(l, dir+"/", ""))) {
				cost += e.Cost
				in += e.In
				out += e.Out
				if e.Kind == "delta" {
					delta += e.Text
					continue
				}
				flush()
				switch e.Kind {
				case "usage":
					continue
				case "msg":
					lastMsg = e.Text
				case "done":
					final = e.Text
					if e.Text == lastMsg {
						e.Text = "finished" // the answer was just logged as a msg
					}
				}
				raw := ""
				if i == 0 {
					raw = clip(l, 64<<10)
				}
				emit(e.Kind, e.Text, raw)
			}
		}
		if err == io.EOF {
			break
		} else if err != nil {
			emit("error", err.Error(), "")
			break
		}
	}
	flush()
	werr := cmd.Wait()
	db.Exec(`UPDATE runs SET ended=?, exit=?, cost=?, tok_in=?, tok_out=? WHERE id=?`,
		time.Now().UnixMilli(), cmd.ProcessState.ExitCode(), cost, in, out, j.run)
	if final == "" {
		final = lastMsg
	}
	return final, werr
}

// runChecks runs each check with sh in dir. Returns how many passed and feedback for the failures.
func runChecks(j *job, dir string, checks []string) (int, string) {
	passed := 0
	var fb strings.Builder
	for _, c := range checks {
		cctx, cancel := context.WithTimeout(j.ctx, 10*time.Minute)
		cmd := exec.CommandContext(cctx, "sh", "-c", c) // ponytail: needs sh; Windows users need Git Bash on PATH
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		cancel()
		tail := string(out)
		if len(tail) > 2000 {
			tail = "…" + tail[len(tail)-2000:]
		}
		if err == nil {
			passed++
			j.emit("check", "✓ "+c, tail)
			continue
		}
		j.emit("check", fmt.Sprintf("✗ %s (%v)", c, err), tail)
		fmt.Fprintf(&fb, "Check `%s` failed (%v). Output:\n%s\n\n", c, err, tail)
	}
	return passed, fb.String()
}

func goalText(b *strings.Builder, g Goal, checks []string) {
	fmt.Fprintf(b, "\nGOAL: %s\n", g.Title)
	if g.Body != "" {
		b.WriteString(g.Body + "\n")
	}
	if g.Criteria != "" {
		b.WriteString("\nACCEPTANCE CRITERIA:\n" + g.Criteria + "\n")
	}
	b.WriteString("\nThese checks run in the worktree afterwards and must all pass:\n")
	for _, c := range checks {
		b.WriteString("- " + c + "\n")
	}
}

func workerPrompt(a Agent, g Goal, dir string, checks []string, feedback string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, a %s agent. You work in the git worktree %s; only change files there.\n", a.Name, a.Role, dir)
	if a.Prompt != "" {
		b.WriteString("\n" + a.Prompt + "\n")
	}
	goalText(&b, g, checks)
	b.WriteString("\nStay within the goal. Don't claim something works unless you verified it. Don't commit; the orchestrator does.\n")
	if g.Notes != "" {
		b.WriteString("\nTHE USER REMOVED THESE CHANGES OF YOURS. Do not re-add them:\n" + g.Notes + "\n")
	}
	if feedback != "" {
		b.WriteString("\nYOUR PREVIOUS ATTEMPT WAS NOT ACCEPTED. Fix this:\n" + feedback)
	}
	return b.String()
}

func planPrompt(a Agent, g Goal, kids []Agent, feedback string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, a manager agent. You delegate and review; you do not edit files.\n", a.Name)
	if a.Prompt != "" {
		b.WriteString("\n" + a.Prompt + "\n")
	}
	goalText(&b, g, lines(g.Checks))
	b.WriteString("\nYOUR TEAM:\n")
	for _, k := range kids {
		fmt.Fprintf(&b, "- id %q: %s, role %s", k.ID, k.Name, k.Role)
		if k.Prompt != "" {
			b.WriteString(" (" + clip(k.Prompt, 200) + ")")
		}
		b.WriteString("\n")
	}
	if feedback != "" {
		b.WriteString("\nYOUR PREVIOUS RESULT WAS NOT ACCEPTED. Plan to fix this:\n" + feedback + "\n")
	}
	b.WriteString(`
Read the repository if you need to, then split the goal into at most one subgoal per team member (skip members you don't need).
Each subgoal must be small, clearly scoped, and have at least one shell check that proves it's done; checks run from the repository root.
Members work in parallel on separate branches, so avoid giving two members the same files.
Reply with ONLY a JSON object, no prose:
{"subgoals":[{"agent":"<id>","title":"...","body":"...","criteria":"...","checks":["..."]}]}`)
	return b.String()
}

// lineWriter calls f for each non-empty line written.
// ponytail: a line split across two writes shows up as two lines.
type lineWriter func(string)

func (f lineWriter) Write(p []byte) (int, error) {
	for _, l := range lines(string(p)) {
		f(l)
	}
	return len(p), nil
}

// lines returns the non-empty trimmed lines of s.
func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
