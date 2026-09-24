// Package orch runs agent goals: a worker works, gets checkpointed and checked, and retries
// with the failure output; a manager plans subgoals for its children, runs them in parallel,
// reviews and merges their verified work, then checks the merged result.
package orch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"arranger/internal/agents"
	"arranger/internal/git"
	"arranger/internal/store"
)

const (
	maxAttempts     = 3 // worker: first try + 2 retries fed with the failing checks' output
	maxPlanTries    = 2 // manager: re-ask when the plan/review isn't valid JSON or misses checks
	maxReviewRounds = 3 // manager: review, re-run rejected children, review again...
	maxDiffInReview = 15000
)

var errTokenLimit = errors.New("hard token limit reached")

// Orchestrator starts, tracks and stops runs.
type Orchestrator struct {
	Store *store.Store
	Hub   *Hub
	Root  string // one worktree per agent lives here

	slots   chan struct{} // caps concurrent agent processes
	mu      sync.Mutex
	running map[string]context.CancelFunc // agent id -> cancel of its run
}

// New returns an orchestrator that runs at most parallel agent processes at once.
func New(st *store.Store, root string, parallel int) *Orchestrator {
	return &Orchestrator{Store: st, Hub: NewHub(), Root: root, slots: make(chan struct{}, max(1, parallel)),
		running: map[string]context.CancelFunc{}}
}

// Dir is the agent's worktree path (it may not exist yet).
func (o *Orchestrator) Dir(agentID string) string { return filepath.Join(o.Root, agentID) }

func (o *Orchestrator) Stop(agentID string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	cancel, ok := o.running[agentID]
	if ok {
		cancel()
	}
	return ok
}

// StopAll stops every run and waits up to timeout for them to wind down (agents are marked
// stopped, checkpoints committed). It reports whether they all finished in time.
func (o *Orchestrator) StopAll(timeout time.Duration) bool {
	o.mu.Lock()
	for _, cancel := range o.running {
		cancel()
	}
	o.mu.Unlock()
	for deadline := time.Now().Add(timeout); ; time.Sleep(50 * time.Millisecond) {
		o.mu.Lock()
		n := len(o.running)
		o.mu.Unlock()
		if n == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

func (o *Orchestrator) IsRunning(agentID string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.running[agentID]
	return ok
}

// register marks an agent as running with cancel; false if it already is.
func (o *Orchestrator) register(agentID string, cancel context.CancelFunc) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, busy := o.running[agentID]; busy {
		return false
	}
	o.running[agentID] = cancel
	return true
}

func (o *Orchestrator) unregister(agentID string) {
	o.mu.Lock()
	delete(o.running, agentID)
	o.mu.Unlock()
}

// BaseFor is the branch an agent works from: its manager's branch when that exists, else the project base.
func BaseFor(p store.Project, a store.Agent) string {
	if a.Parent != "" && git.BranchExists(p.Repo, git.BranchOf(a.Parent)) {
		return git.BranchOf(a.Parent)
	}
	return p.Base
}

// Workspace returns the agent's worktree and the commit its work is measured from: the base
// branch's tip when the worktree was made. It's kept across runs, so an agent's diff still
// shows its work after a manager merged it.
func (o *Orchestrator) Workspace(p store.Project, a store.Agent) (dir, base string, err error) {
	branch := BaseFor(p, a)
	_, statErr := os.Stat(o.Dir(a.ID))
	if dir, err = git.EnsureWorktree(o.Root, p.Repo, branch, a.ID); err != nil {
		return "", "", err
	}
	g, _ := o.Store.Goal(a.ID)
	if statErr == nil && git.IsSHA(g.Base) {
		if _, err := git.Run(dir, "merge-base", "--is-ancestor", g.Base, "HEAD"); err == nil {
			return dir, g.Base, nil
		}
	}
	out, err := git.Run(p.Repo, "rev-parse", branch)
	if err != nil {
		return "", "", err
	}
	base = strings.TrimSpace(out)
	o.Store.SetGoal(a.ID, map[string]any{"base": base})
	return dir, base, nil
}

// Sync brings the agent's worktree up to date with the branch it works from (see BaseFor) and
// returns its worktree, its new diff base, and how many commits came in. The agent's diff is
// measured from the synced tip from then on, so upstream changes don't show as its work.
func (o *Orchestrator) Sync(p store.Project, a store.Agent) (dir, base string, n int, err error) {
	if dir, base, err = o.Workspace(p, a); err != nil {
		return "", "", 0, err
	}
	from := BaseFor(p, a)
	if n, err = git.Sync(dir, from); err != nil || n == 0 {
		return dir, base, 0, err
	}
	out, err := git.Run(dir, "rev-parse", from)
	if err != nil {
		return dir, base, n, err
	}
	base = strings.TrimSpace(out)
	o.Store.SetGoal(a.ID, map[string]any{"base": base})
	o.RefreshDiff(a.ID, dir, base)
	return dir, base, n, nil
}

// RefreshDiff recomputes the lines added and removed shown on the agent's box.
func (o *Orchestrator) RefreshDiff(agentID, dir, base string) {
	if fs, err := git.Diff(dir, base); err == nil {
		adds, dels := git.Stat(fs)
		o.Store.SetGoal(agentID, map[string]any{"adds": adds, "dels": dels})
	}
}

// checkGoal reports why a goal can't run, or nil.
func checkGoal(a store.Agent, g store.Goal) error {
	if strings.TrimSpace(g.Title) == "" {
		return fmt.Errorf("%s has no goal", a.Name)
	}
	if len(lines(g.Checks)) == 0 {
		return fmt.Errorf("%s's goal needs at least one check: nothing is done until a check passes", a.Name)
	}
	if _, ok := agents.Runtimes[a.Runtime]; !ok {
		return fmt.Errorf("unknown runtime %q", a.Runtime)
	}
	return nil
}

// Start validates a run and starts it in the background. A manager runs its whole subtree.
func (o *Orchestrator) Start(agentID string) error { return o.start(agentID, "") }

// Revise re-runs an agent that already worked, to make the change the user describes. A worker
// keeps its work and makes the change; a manager passes each part of it to the reports it
// concerns and re-runs only those, then reviews, merges and checks as usual.
func (o *Orchestrator) Revise(agentID, change string) error {
	if change = strings.TrimSpace(change); change == "" {
		return errors.New("describe what should change")
	}
	return o.start(agentID, change)
}

func (o *Orchestrator) start(agentID, change string) error {
	a, pid, err := o.Store.Agent(agentID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("agent %s not found; save the arrangement first", agentID)
	} else if err != nil {
		return fmt.Errorf("agent %s: %w", agentID, err)
	}
	p, err := o.Store.Project(pid)
	if err != nil {
		return err
	}
	if p.Repo == "" {
		return errors.New("set a git repository for this project first")
	}
	g, err := o.Store.Goal(agentID)
	if err != nil {
		return err
	}
	if err := checkGoal(a, g); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	if !o.register(agentID, cancel) {
		cancel()
		return fmt.Errorf("%s is already running", a.Name)
	}
	j := &job{o: o, ctx: ctx, p: p, a: a, change: change}
	if change != "" {
		j.emit("request", "you asked for changes: "+change, "")
	}
	j.status("starting", nil) // before returning, so the page shows it working right away
	go func() {
		defer o.unregister(agentID)
		defer cancel()
		o.execute(j, "")
	}()
	return nil
}

// job is one agent's run: where it works and how it reports.
type job struct {
	o   *Orchestrator
	ctx context.Context
	p   store.Project
	a   store.Agent
	run int64 // current runs.id, tags events

	// change is what the user asked to change since the last run (or, for a report, the part of
	// it its manager passed down). "" for a plain run from the goal.
	change string

	used      int  // tokens this run, across attempts
	warned    bool // soft limit already reported
	overLimit bool // hard limit hit; the run must end
}

func (j *job) emit(kind, text, raw string) {
	e := store.LogEvent{Agent: j.a.ID, Run: j.run, Kind: kind, Text: text, Raw: raw}
	j.o.Store.AddEvent(&e)
	if kind != "stderr" {
		j.o.Hub.SetNow(j.a.ID, kind+": "+agents.Clip(text, 80))
	}
	j.o.Hub.Publish(j.p.ID, map[string]any{"type": "event", "event": e})
}

func (j *job) status(s string, kv map[string]any) string {
	if kv == nil {
		kv = map[string]any{}
	}
	kv["status"] = s
	j.o.Store.SetGoal(j.a.ID, kv)
	j.o.Hub.Publish(j.p.ID, map[string]any{"type": "status", "agent": j.a.ID, "status": s})
	return s
}

func (j *job) fail(s, msg string) string {
	zap.L().Warn("agent "+s, zap.String("agent", j.a.Name), zap.String("id", j.a.ID), zap.String("reason", agents.Clip(msg, 300)))
	j.emit("error", msg, "")
	return j.status(s, map[string]any{"feedback": msg})
}

func (j *job) newRun(attempt int) { j.run = j.o.Store.NewRun(j.a.ID, attempt) }

// execute runs an agent's goal and returns its final status. feedback comes from a reviewer.
func (o *Orchestrator) execute(j *job, feedback string) (status string) {
	lg := zap.L().With(zap.String("agent", j.a.Name), zap.String("id", j.a.ID), zap.String("runtime", j.a.Runtime))
	lg.Info("run started", zap.String("project", j.p.Name), zap.Bool("rerun", feedback != ""))
	start := time.Now()
	defer func() { lg.Info("run finished", zap.String("status", status), zap.Duration("took", time.Since(start))) }()
	g, err := o.Store.Goal(j.a.ID)
	if err == nil {
		err = checkGoal(j.a, g)
	}
	if err != nil {
		return j.fail("failed", err.Error())
	}
	kids, err := o.Store.Children(j.a.ID)
	if err != nil {
		return j.fail("failed", err.Error())
	}
	// every run starts from the latest code on the branch the agent works from
	dir, base, n, err := o.Sync(j.p, j.a)
	switch {
	case dir == "":
		return j.fail("failed", err.Error())
	case err != nil:
		j.emit("warn", "couldn't sync with "+BaseFor(j.p, j.a)+", working from where this agent left off: "+err.Error(), "")
	case n > 0:
		j.emit("msg", fmt.Sprintf("synced with %s: %d new commit(s)", BaseFor(j.p, j.a), n), "")
	}
	g.Base = base
	o.Store.SetGoal(j.a.ID, map[string]any{"attempts": 0, "feedback": "", "passed": 0, "total": 0, "since": time.Now().UnixMilli()})
	if len(kids) > 0 {
		return runManager(j, g, dir, kids, feedback)
	}
	return runWorker(j, g, dir, feedback)
}

func runWorker(j *job, g store.Goal, dir, feedback string) string {
	checks := lines(g.Checks)
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		j.status("running", map[string]any{"attempts": attempt})
		j.newRun(attempt)
		j.emit("msg", fmt.Sprintf("attempt %d/%d with %s", attempt, maxAttempts, j.a.Runtime), "")

		_, agentErr := runAgent(j, workerPrompt(j.a, g, dir, checks, feedback, j.change), dir)
		if sha, err := git.Commit(dir, fmt.Sprintf("arranger: %s attempt %d", j.a.Name, attempt)); err != nil {
			j.emit("error", err.Error(), "")
		} else if sha != "" {
			j.emit("msg", "checkpoint "+sha, "")
		}
		j.o.RefreshDiff(j.a.ID, dir, g.Base)
		if j.ctx.Err() != nil {
			return j.fail("stopped", "stopped by user")
		}
		if errors.Is(agentErr, errTokenLimit) {
			return j.fail("failed", j.limitMsg())
		}
		if agentErr != nil {
			j.emit("error", agentErr.Error(), "")
		}

		j.status("verifying", nil)
		passed, fb := runChecks(j, dir, checks)
		j.o.Store.SetGoal(j.a.ID, map[string]any{"passed": passed, "total": len(checks)})
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
		j.o.Store.SetGoal(j.a.ID, map[string]any{"feedback": feedback})
	}
	return j.fail("failed", fmt.Sprintf("checks still failing after %d attempts:\n%s", maxAttempts, feedback))
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
