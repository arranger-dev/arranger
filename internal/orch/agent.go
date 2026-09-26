package orch

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
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
	cmd, err := rt.Command(ctx, j.a.Model, j.a.Args, prompt, j.allow)
	if err != nil {
		return "", err
	}
	cmd.Dir = dir
	cmd.WaitDelay = 5 * time.Second
	ownGroup(cmd) // stopping it stops everything it started
	// stdout is a plain pipe handed to the process, so reading ends when the process group is gone,
	// even if something the agent started in the background inherited it
	stdout, pw, err := os.Pipe()
	if err != nil {
		return "", err
	}
	defer stdout.Close()
	er, ew, err := os.Pipe()
	if err != nil {
		pw.Close()
		return "", err
	}
	cmd.Stdout, cmd.Stderr = pw, ew
	err = cmd.Start()
	pw.Close()
	ew.Close()
	if err != nil {
		er.Close()
		return "", err
	}
	stderr := &lineWriter{emit: func(l string) { j.emit("stderr", agents.Clip(l, 500), "") }}
	stderrDone := make(chan struct{})
	go func() { io.Copy(stderr, er); er.Close(); stderr.Flush(); close(stderrDone) }()
	exited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		killGroup(cmd) // whatever the agent left running (dev servers, watchers) goes with it
		exited <- err
	}()
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
		if j.overLimit {
			break // the run ends at the limit; output still buffered from before the kill isn't counted
		}
		if err == io.EOF {
			break
		} else if err != nil {
			j.emit("error", err.Error(), "")
			break
		}
	}
	flush()
	werr := <-exited
	<-stderrDone
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
		ownGroup(cmd) // a timeout or Stop kills everything the check started
		out, err := combinedOutput(cmd)
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

// combinedOutput runs cmd and returns its stdout and stderr together. It returns as soon as cmd
// exits, killing anything it left running in the background (which may hold its output open).
func combinedOutput(cmd *exec.Cmd) ([]byte, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = w, w
	err = cmd.Start()
	w.Close()
	if err != nil {
		r.Close()
		return nil, err
	}
	var out bytes.Buffer
	read := make(chan struct{})
	go func() { io.Copy(&out, r); r.Close(); close(read) }()
	err = cmd.Wait()
	killGroup(cmd)
	<-read
	return out.Bytes(), err
}

// lineWriter calls emit for each non-empty line written. A line split across writes is held
// until it's complete; Flush emits what's left when the writer is done.
type lineWriter struct {
	emit    func(string)
	mu      sync.Mutex
	partial []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.partial = append(w.partial, p...)
	if i := bytes.LastIndexByte(w.partial, '\n'); i >= 0 {
		for _, l := range lines(string(w.partial[:i])) {
			w.emit(l)
		}
		w.partial = append(w.partial[:0], w.partial[i+1:]...)
	}
	return len(p), nil
}

// Flush emits a last line that never got its newline.
func (w *lineWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if l := strings.TrimSpace(string(w.partial)); l != "" {
		w.emit(l)
	}
	w.partial = nil
}
