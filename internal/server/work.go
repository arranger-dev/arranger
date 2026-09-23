package server

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"arranger/internal/git"
	"arranger/internal/orch"
	"arranger/internal/store"
)

// work is an agent with its project, goal and worktree, loaded for a request.
type work struct {
	a   store.Agent
	p   store.Project
	g   store.Goal
	dir string // "" when the agent has never run
}

// agentWork loads the agent named in the path, writing an error response when it can't.
func (s *Server) agentWork(w http.ResponseWriter, r *http.Request) (wk work, ok bool) {
	a, pid, err := s.st.Agent(r.PathValue("id"))
	if err != nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return wk, false
	}
	wk.a = a
	if wk.p, err = s.st.Project(pid); err == nil {
		wk.g, err = s.st.Goal(a.ID)
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return wk, false
	}
	if wk.g.Base == "" {
		wk.g.Base = wk.p.Base
	}
	if _, err := os.Stat(s.o.Dir(a.ID)); err == nil {
		wk.dir = s.o.Dir(a.ID)
	}
	return wk, true
}

// editable loads an agent whose worktree the user may change: it has run, and isn't running now.
func (s *Server) editable(w http.ResponseWriter, r *http.Request) (wk work, ok bool) {
	if wk, ok = s.agentWork(w, r); !ok {
		return
	}
	if wk.dir == "" {
		http.Error(w, wk.a.Name+" has no work yet", http.StatusBadRequest)
		return wk, false
	}
	if s.o.IsRunning(wk.a.ID) {
		http.Error(w, "stop "+wk.a.Name+" before changing its work", http.StatusConflict)
		return wk, false
	}
	return wk, true
}

// changed recomputes an agent's diff stat and tells the UI.
func (s *Server) changed(p store.Project, a store.Agent, dir, base string) {
	s.o.RefreshDiff(a.ID, dir, base)
	g, _ := s.st.Goal(a.ID)
	s.o.Hub.Publish(p.ID, map[string]any{"type": "status", "agent": a.ID, "status": g.Status})
}

// diff returns the agent's changes, and mergedInto: the manager whose work already contains
// them (so removing a change here won't remove it there).
func (s *Server) diff(w http.ResponseWriter, r *http.Request) {
	wk, ok := s.agentWork(w, r)
	if !ok {
		return
	}
	from := orch.BaseFor(wk.p, wk.a)
	res := map[string]any{"files": []git.FileDiff{}, "mergedInto": "", "from": from, "behind": 0}
	if wk.dir == "" {
		writeJSON(w, res) // never ran, so no changes yet
		return
	}
	fs, err := git.Diff(wk.dir, wk.g.Base)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	res["files"] = fs
	res["behind"] = git.Behind(wk.dir, from)
	if mgr, _, err := s.st.Agent(wk.a.Parent); err == nil && len(fs) > 0 && git.BranchExists(wk.p.Repo, git.BranchOf(mgr.ID)) {
		if _, err := git.Run(wk.p.Repo, "merge-base", "--is-ancestor", git.BranchOf(wk.a.ID), git.BranchOf(mgr.ID)); err == nil {
			res["mergedInto"] = mgr.Name
		}
	}
	writeJSON(w, res)
}

// sync merges the latest commits of the branch the agent works from into its work. Runs do this
// on their own; this lets the user do it (and see conflicts) before running.
func (s *Server) sync(w http.ResponseWriter, r *http.Request) {
	wk, ok := s.editable(w, r)
	if !ok {
		return
	}
	_, _, n, err := s.o.Sync(wk.p, wk.a)
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	if n > 0 {
		e := store.LogEvent{Agent: wk.a.ID, Kind: "msg", Text: fmt.Sprintf("synced with %s: %d new commit(s)", orch.BaseFor(wk.p, wk.a), n)}
		s.st.AddEvent(&e)
		s.o.Hub.Publish(wk.p.ID, map[string]any{"type": "event", "event": e})
		g, _ := s.st.Goal(wk.a.ID)
		s.o.Hub.Publish(wk.p.ID, map[string]any{"type": "status", "agent": wk.a.ID, "status": g.Status})
	}
	writeJSON(w, map[string]any{"commits": n, "from": orch.BaseFor(wk.p, wk.a)})
}

func (s *Server) checkpoints(w http.ResponseWriter, r *http.Request) {
	wk, ok := s.agentWork(w, r)
	if !ok {
		return
	}
	if wk.dir == "" {
		writeJSON(w, []git.Checkpoint{})
		return
	}
	cs, err := git.Checkpoints(wk.dir, wk.g.Base)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, cs)
}

func (s *Server) revert(w http.ResponseWriter, r *http.Request) {
	wk, ok := s.editable(w, r)
	if !ok {
		return
	}
	var req struct{ SHA string }
	if !readJSON(w, r, &req) {
		return
	}
	if !git.IsSHA(req.SHA) {
		http.Error(w, "bad commit id", http.StatusBadRequest)
		return
	}
	if err := git.Revert(wk.dir, req.SHA); err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	s.changed(wk.p, wk.a, wk.dir, wk.g.Base)
	w.WriteHeader(http.StatusNoContent)
}

// hunks removes the selected hunks from the agent's work, or promotes them into its manager's work.
func (s *Server) hunks(w http.ResponseWriter, r *http.Request) {
	wk, ok := s.editable(w, r)
	if !ok {
		return
	}
	var req struct {
		Action string
		IDs    []string
	}
	if !readJSON(w, r, &req) {
		return
	}
	ids := map[string]bool{}
	for _, id := range req.IDs {
		ids[id] = true
	}
	fs, err := git.Diff(wk.dir, wk.g.Base)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	patch, n := git.HunkPatch(fs, ids)
	if n == 0 || n != len(ids) {
		http.Error(w, "the diff changed since you loaded it; refresh and select again", http.StatusConflict)
		return
	}
	switch req.Action {
	case "remove":
		if _, err := git.RunIn(wk.dir, patch, "apply", "-R", "-"); err != nil {
			fail(w, http.StatusConflict, err)
			return
		}
		git.Commit(wk.dir, fmt.Sprintf("arranger: user removed %d change(s)", n))
		notes := wk.g.Notes + patch
		if len(notes) > 8000 { // keep the newest removals; the prompt shouldn't grow without bound
			notes = notes[len(notes)-8000:]
		}
		s.st.SetGoal(wk.a.ID, map[string]any{"notes": notes})
		s.changed(wk.p, wk.a, wk.dir, wk.g.Base)
	case "promote":
		mgr, _, err := s.st.Agent(wk.a.Parent)
		if wk.a.Parent == "" || err != nil {
			http.Error(w, wk.a.Name+" has no manager to promote to", http.StatusBadRequest)
			return
		}
		if s.o.IsRunning(mgr.ID) {
			http.Error(w, "stop "+mgr.Name+" before changing its work", http.StatusConflict)
			return
		}
		mdir, mbase, err := s.o.Workspace(wk.p, mgr)
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		applied, present, err := git.ApplyHunks(mdir, fs, ids)
		if err != nil {
			fail(w, http.StatusConflict, fmt.Errorf("couldn't copy into %s's work: %w", mgr.Name, err))
			return
		}
		if applied > 0 {
			git.Commit(mdir, fmt.Sprintf("arranger: promote %d change(s) from %s", applied, wk.a.Name))
			s.changed(wk.p, mgr, mdir, mbase)
		}
		writeJSON(w, map[string]any{"applied": applied, "present": present, "to": mgr.Name})
		return
	default:
		http.Error(w, "action must be remove or promote", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// merge previews (GET) or performs (POST) merging an agent's branch into a branch of the user's repo.
func (s *Server) merge(w http.ResponseWriter, r *http.Request) {
	wk, ok := s.agentWork(w, r)
	if !ok {
		return
	}
	if wk.dir == "" {
		http.Error(w, wk.a.Name+" has no work to merge yet", http.StatusBadRequest)
		return
	}
	var req struct{ Target, Strategy, Message string }
	if r.Method == http.MethodPost && !readJSON(w, r, &req) {
		return
	}
	if r.Method == http.MethodGet {
		req.Target = r.URL.Query().Get("target")
	}
	if req.Target = strings.TrimSpace(req.Target); req.Target == "" {
		req.Target = git.DefaultTarget(wk.p.Repo)
	}
	if _, err := git.Run(wk.p.Repo, "check-ref-format", "--branch", req.Target); err != nil || strings.HasPrefix(req.Target, "arranger/") {
		fail(w, http.StatusBadRequest, fmt.Errorf("%q isn't a branch name you can merge into", req.Target))
		return
	}
	if s.o.IsRunning(wk.a.ID) {
		fail(w, http.StatusConflict, errors.New("stop "+wk.a.Name+" before merging its work"))
		return
	}
	git.Commit(wk.dir, "arranger: save work before merge") // include anything not yet committed
	if r.Method == http.MethodGet {
		m, err := git.PreviewMerge(wk.p.Repo, git.BranchOf(wk.a.ID), req.Target, wk.p.Base)
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, m)
		return
	}
	if req.Message = strings.TrimSpace(req.Message); req.Message == "" {
		req.Message = wk.g.Title
	}
	if req.Message == "" {
		req.Message = "Merge work from " + wk.a.Name
	}
	sha, err := git.MergeInto(wk.p.Repo, git.BranchOf(wk.a.ID), req.Target, wk.p.Base, req.Strategy, req.Message)
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	e := store.LogEvent{Agent: wk.a.ID, Kind: "merge", Text: fmt.Sprintf("merged into %s (%s) → %s", req.Target, req.Strategy, sha)}
	s.st.AddEvent(&e)
	s.o.Hub.Publish(wk.p.ID, map[string]any{"type": "event", "event": e})
	writeJSON(w, map[string]string{"sha": sha, "target": req.Target})
}
