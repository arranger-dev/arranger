package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const maxAttempts = 3 // first try + 2 retries fed with the failing checks' output

var worktreeRoot string // set in main

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

var (
	runMu   sync.Mutex
	running = map[string]context.CancelFunc{}
)

func stopGoal(agentID string) bool {
	runMu.Lock()
	defer runMu.Unlock()
	cancel, ok := running[agentID]
	if ok {
		cancel()
	}
	return ok
}

// startGoal validates everything a run needs, then runs the goal in the background.
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
	if strings.TrimSpace(g.Title) == "" {
		return fmt.Errorf("%s has no goal", a.Name)
	}
	if len(lines(g.Checks)) == 0 {
		return fmt.Errorf("%s's goal needs at least one check: nothing is done until a check passes", a.Name)
	}
	rt, ok := runtimes[a.Runtime]
	if !ok {
		return fmt.Errorf("unknown runtime %q", a.Runtime)
	}
	if _, err := exec.LookPath(rt.Bin); err != nil {
		return fmt.Errorf("%s is not installed", rt.Bin)
	}
	runMu.Lock()
	defer runMu.Unlock()
	if _, busy := running[agentID]; busy {
		return fmt.Errorf("%s is already running", a.Name)
	}
	ctx, cancel := context.WithCancel(context.Background())
	running[agentID] = cancel
	go func() {
		defer func() {
			runMu.Lock()
			delete(running, agentID)
			runMu.Unlock()
			cancel()
		}()
		runGoal(ctx, p, a, g, rt)
	}()
	return nil
}

func runGoal(ctx context.Context, p Project, a Agent, g Goal, rt Runtime) {
	var runID int64
	emit := func(kind, text, raw string) {
		e := LogEvent{Agent: a.ID, Run: runID, Kind: kind, Text: text, Raw: raw}
		addEvent(&e)
		if kind != "stderr" {
			hub.setNow(a.ID, kind+": "+clip(text, 80))
		}
		hub.publish(p.ID, map[string]any{"type": "event", "event": e})
	}
	status := func(s string, kv map[string]any) {
		if kv == nil {
			kv = map[string]any{}
		}
		kv["status"] = s
		setGoal(a.ID, kv)
		hub.publish(p.ID, map[string]any{"type": "status", "agent": a.ID, "status": s})
	}
	refreshDiff := func(dir string) {
		if fs, err := worktreeDiff(dir, p.Base); err == nil {
			adds, dels := diffStat(fs)
			setGoal(a.ID, map[string]any{"adds": adds, "dels": dels})
		}
	}

	status("running", map[string]any{"attempts": 0, "feedback": "", "passed": 0, "total": 0})
	dir, err := ensureWorktree(worktreeRoot, p.Repo, p.Base, a.ID)
	if err != nil {
		emit("error", err.Error(), "")
		status("failed", nil)
		return
	}
	checks := lines(g.Checks)
	feedback := ""
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		status("running", map[string]any{"attempts": attempt})
		res, _ := db.Exec(`INSERT INTO runs(agent_id, attempt, started) VALUES(?,?,?)`, a.ID, attempt, time.Now().UnixMilli())
		runID, _ = res.LastInsertId()
		emit("msg", fmt.Sprintf("attempt %d/%d with %s in %s", attempt, maxAttempts, a.Runtime, dir), "")

		agentErr := runAgent(ctx, rt, a, buildPrompt(a, g, dir, checks, feedback), dir, runID, emit)
		sha, err := checkpoint(dir, fmt.Sprintf("arranger: %s attempt %d", a.Name, attempt))
		if err != nil {
			emit("error", err.Error(), "")
		} else if sha != "" {
			emit("msg", "checkpoint "+sha, "")
		}
		refreshDiff(dir)
		if ctx.Err() != nil {
			emit("error", "stopped by user", "")
			status("stopped", nil)
			return
		}
		if agentErr != nil {
			emit("error", agentErr.Error(), "")
		}

		status("verifying", nil)
		passed, fb := runChecks(ctx, dir, checks, emit)
		setGoal(a.ID, map[string]any{"passed": passed, "total": len(checks)})
		if ctx.Err() != nil {
			status("stopped", nil)
			return
		}
		if passed == len(checks) {
			emit("done", fmt.Sprintf("all %d checks passed", passed), "")
			status("done", map[string]any{"feedback": ""})
			return
		}
		feedback = fb
		if agentErr != nil {
			feedback = "The agent process failed: " + agentErr.Error() + "\n" + fb
		}
		setGoal(a.ID, map[string]any{"feedback": feedback})
	}
	emit("error", fmt.Sprintf("checks still failing after %d attempts", maxAttempts), "")
	status("failed", nil)
}

func runAgent(ctx context.Context, rt Runtime, a Agent, prompt, dir string, runID int64, emit func(kind, text, raw string)) error {
	// ponytail: cancel kills the agent process only, not its children; use a process group if strays show up.
	cmd := rt.command(ctx, a.Model, a.Args)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(prompt)
	cmd.WaitDelay = 5 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = lineWriter(func(l string) { emit("stderr", clip(l, 500), "") })
	if err := cmd.Start(); err != nil {
		return err
	}
	var cost float64
	var in, out int
	r := bufio.NewReader(stdout)
	for {
		line, err := r.ReadBytes('\n')
		if l := strings.TrimSpace(string(line)); l != "" {
			for i, e := range rt.Parse([]byte(l)) {
				raw := ""
				if i == 0 {
					raw = clip(l, 64<<10)
				}
				cost += e.Cost
				in += e.In
				out += e.Out
				emit(e.Kind, strings.ReplaceAll(e.Text, dir+"/", ""), raw) // worktree-relative paths read better
			}
		}
		if err == io.EOF {
			break
		} else if err != nil {
			emit("error", err.Error(), "")
			break
		}
	}
	werr := cmd.Wait()
	db.Exec(`UPDATE runs SET ended=?, exit=?, cost=?, tok_in=?, tok_out=? WHERE id=?`,
		time.Now().UnixMilli(), cmd.ProcessState.ExitCode(), cost, in, out, runID)
	return werr
}

// runChecks runs each check with sh in dir. Returns how many passed and feedback for the failures.
func runChecks(ctx context.Context, dir string, checks []string, emit func(kind, text, raw string)) (int, string) {
	passed := 0
	var fb strings.Builder
	for _, c := range checks {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
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
			emit("check", "✓ "+c, tail)
			continue
		}
		emit("check", fmt.Sprintf("✗ %s (%v)", c, err), tail)
		fmt.Fprintf(&fb, "Check `%s` failed (%v). Output:\n%s\n\n", c, err, tail)
	}
	return passed, fb.String()
}

func buildPrompt(a Agent, g Goal, dir string, checks []string, feedback string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, a %s agent. You work in the git worktree %s; only change files there.\n", a.Name, a.Role, dir)
	if a.Prompt != "" {
		b.WriteString("\n" + a.Prompt + "\n")
	}
	fmt.Fprintf(&b, "\nGOAL: %s\n", g.Title)
	if g.Body != "" {
		b.WriteString(g.Body + "\n")
	}
	if g.Criteria != "" {
		b.WriteString("\nACCEPTANCE CRITERIA:\n" + g.Criteria + "\n")
	}
	b.WriteString("\nWhen you finish, these checks run in the worktree and must all pass:\n")
	for _, c := range checks {
		b.WriteString("- " + c + "\n")
	}
	b.WriteString("\nStay within the goal. Don't claim something works unless you verified it. Don't commit; the orchestrator does.\n")
	if feedback != "" {
		b.WriteString("\nYOUR PREVIOUS ATTEMPT FAILED THESE CHECKS. Fix the cause:\n" + feedback)
	}
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
