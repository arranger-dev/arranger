package orch

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"go.uber.org/zap"

	"arranger/internal/agents"
)

func (j *job) limitMsg() string {
	return fmt.Sprintf("stopped: used %d tokens, over this agent's hard limit of %d. Raise it in Settings or narrow the goal.", j.used, j.a.Hard)
}

// account adds token usage to the run, updates the UI live, and enforces the agent's limits.
// It returns false once the hard limit is hit.
func (j *job) account(in, out int) bool {
	j.used += in + out
	j.o.Hub.Publish(j.p.ID, map[string]any{"type": "usage", "agent": j.a.ID})
	if j.a.Hard > 0 && j.used >= j.a.Hard {
		if !j.overLimit {
			j.overLimit = true
			j.emit("error", fmt.Sprintf("hard token limit: %d of %d tokens used, stopping", j.used, j.a.Hard), "")
		}
		return false
	}
	if j.a.Soft > 0 && j.used >= j.a.Soft && !j.warned {
		j.warned = true
		j.emit("warn", fmt.Sprintf("soft token limit: %d of %d tokens used", j.used, j.a.Soft), "")
	}
	return true
}

// runAgent runs the agent CLI once, streaming its events. It returns the agent's final answer.
func runAgent(j *job, prompt, dir string) (string, error) {
	select {
	case j.o.slots <- struct{}{}:
		defer func() { <-j.o.slots }()
	case <-j.ctx.Done():
		return "", j.ctx.Err()
	}
	if j.overLimit {
		return "", errTokenLimit
	}
	rt := agents.Runtimes[j.a.Runtime]
	ctx, cancel := context.WithCancel(j.ctx) // cancelled early when the hard token limit is hit
	defer cancel()
	// ponytail: cancel kills the agent process only, not its children; use a process group if strays show up.
	cmd, err := rt.Command(ctx, j.a.Model, j.a.Args, prompt)
	if err != nil {
		return "", err
	}
	cmd.Dir = dir
	cmd.WaitDelay = 5 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	cmd.Stderr = lineWriter(func(l string) { j.emit("stderr", agents.Clip(l, 500), "") })
	if err := cmd.Start(); err != nil {
		return "", err
	}
	started := time.Now()
	zap.L().Debug("agent process started", zap.String("agent", j.a.Name), zap.String("cmd", cmd.Path), zap.Int("pid", cmd.Process.Pid), zap.String("dir", dir))
	var cost float64
	var in, out int
	var final, lastMsg, delta string
	snaps := map[string][2]int{} // message id -> latest usage snapshot
	usage := func(e agents.Event) {
		din, dout := e.In, e.Out
		switch {
		case e.ID == "total" && len(snaps) > 0:
			din, dout = 0, 0 // already counted message by message
		case e.ID != "" && e.ID != "total":
			prev := snaps[e.ID]
			snaps[e.ID] = [2]int{e.In, e.Out}
			din, dout = e.In-prev[0], e.Out-prev[1]
		}
		if din == 0 && dout == 0 && e.Cost == 0 {
			return
		}
		in, out, cost = in+din, out+dout, cost+e.Cost
		j.o.Store.UpdateRun(j.run, cost, in, out, -1)
		if !j.account(din, dout) {
			cancel()
		}
	}
	flush := func() { // streamed text becomes one message
		if t := strings.TrimSpace(delta); t != "" {
			lastMsg = t
			j.emit("msg", t, "")
		}
		delta = ""
	}
	r := bufio.NewReader(stdout)
	for {
		line, err := r.ReadBytes('\n')
		if l := strings.TrimSpace(string(line)); l != "" {
			// worktree-relative paths read better; raw keeps the original line
			for i, e := range rt.Parse([]byte(strings.ReplaceAll(l, dir+"/", ""))) {
				usage(e)
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
					raw = agents.Clip(l, 64<<10)
				}
				j.emit(e.Kind, e.Text, raw)
			}
		}
		if err == io.EOF {
			break
		} else if err != nil {
			j.emit("error", err.Error(), "")
			break
		}
	}
	flush()
	werr := cmd.Wait()
	j.o.Store.UpdateRun(j.run, cost, in, out, cmd.ProcessState.ExitCode())
	zap.L().Info("agent process exited", zap.String("agent", j.a.Name), zap.Int("exit", cmd.ProcessState.ExitCode()),
		zap.Duration("took", time.Since(started)), zap.Int("tokensIn", in), zap.Int("tokensOut", out), zap.Float64("cost", cost))
	if final == "" {
		final = lastMsg
	}
	if j.overLimit {
		return final, errTokenLimit
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
		zap.L().Debug("check", zap.String("agent", j.a.Name), zap.String("cmd", c), zap.Bool("ok", err == nil))
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

// lineWriter calls f for each non-empty line written.
// ponytail: a line split across two writes shows up as two lines.
type lineWriter func(string)

func (f lineWriter) Write(p []byte) (int, error) {
	for _, l := range lines(string(p)) {
		f(l)
	}
	return len(p), nil
}
