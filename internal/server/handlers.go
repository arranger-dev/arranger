package server

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"go.uber.org/zap"

	"arranger/internal/agents"
	"arranger/internal/git"
	"arranger/internal/orch"
	"arranger/internal/store"
)

// cleanProject normalizes the repo path and checks it's a git repo with a valid base.
func cleanProject(p *store.Project) error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return errors.New("project needs a name")
	}
	p.Repo = strings.TrimSpace(p.Repo)
	if p.Repo == "" {
		return nil
	}
	if rest, ok := strings.CutPrefix(p.Repo, "~/"); ok {
		home, _ := os.UserHomeDir()
		p.Repo = filepath.Join(home, rest)
	}
	abs, err := filepath.Abs(p.Repo)
	if err != nil {
		return err
	}
	p.Repo = abs
	p.Base, err = git.CheckRepo(p.Repo, strings.TrimSpace(p.Base))
	return err
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var p store.Project
	if !readJSON(w, r, &p) {
		return
	}
	if err := cleanProject(&p); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	p, err := s.st.CreateProject(p)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, p)
}

func (s *Server) updateProject(w http.ResponseWriter, r *http.Request) {
	var p store.Project
	if !readJSON(w, r, &p) {
		return
	}
	p.ID = r.PathValue("id")
	if err := cleanProject(&p); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := s.st.UpdateProject(p); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, p)
}

func (s *Server) saveArrangement(w http.ResponseWriter, r *http.Request) {
	var as []store.Agent
	if !readJSON(w, r, &as) {
		return
	}
	if err := validate(as); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	// removing a working agent would leave its process running with nothing on the canvas to stop it
	keep := map[string]bool{}
	for _, a := range as {
		keep[a.ID] = true
	}
	old, err := s.st.Agents(r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	parent := map[string]string{}
	for _, a := range as {
		parent[a.ID] = a.Parent
	}
	names := map[string]string{}
	for _, a := range old {
		names[a.ID] = a.Name
	}
	for _, a := range old {
		if !keep[a.ID] && s.o.IsRunning(a.ID) {
			fail(w, http.StatusConflict, fmt.Errorf("%s is running; stop it before removing it", a.Name))
			return
		}
		// a waiting plan names its manager's reports; they stay put until it's decided
		if a.Parent != "" && s.o.Awaiting(a.Parent) && (!keep[a.ID] || parent[a.ID] != a.Parent) {
			fail(w, http.StatusConflict, fmt.Errorf("%s's plan is waiting for you; approve or cancel it before moving or removing %s", names[a.Parent], a.Name))
			return
		}
	}
	if err := s.st.SaveArrangement(r.PathValue("id"), as); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	// removed agents take their worktree and branch with them (the page warned about unmerged work)
	if p, err := s.st.Project(r.PathValue("id")); err == nil && p.Repo != "" {
		for _, a := range old {
			if !keep[a.ID] {
				if err := s.o.Remove(p.Repo, a.ID); err != nil {
					zap.L().Warn("clean up removed agent", zap.String("agent", a.Name), zap.Error(err))
				}
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request) {
	a, _, err := s.st.Agent(r.PathValue("id"))
	if err != nil {
		http.Error(w, "agent not found (save the arrangement first)", http.StatusNotFound)
		return
	}
	g, err := s.st.Goal(a.ID)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	// dir is where the agent works, "" until its first run made the worktree
	dir, shown := s.o.Dir(a.ID), ""
	if _, err := os.Stat(dir); err == nil {
		shown = dir
		if home, err := os.UserHomeDir(); err == nil {
			if rest, ok := strings.CutPrefix(dir, home+string(filepath.Separator)); ok {
				shown = "~/" + rest
			}
		}
	}
	writeJSON(w, map[string]any{"agent": a, "goal": g, "dir": shown, "branch": git.BranchOf(a.ID)})
}

func (s *Server) updateAgent(w http.ResponseWriter, r *http.Request) {
	var a store.Agent
	if !readJSON(w, r, &a) {
		return
	}
	a.ID = r.PathValue("id")
	switch _, ok := agents.Runtimes[a.Runtime]; {
	case !ok:
		http.Error(w, "unknown runtime "+a.Runtime, http.StatusBadRequest)
	case strings.TrimSpace(a.Name) == "":
		http.Error(w, "agent needs a name", http.StatusBadRequest)
	case a.Soft < 0 || a.Hard < 0 || (a.Hard > 0 && a.Soft > a.Hard):
		http.Error(w, "token limits: use 0 for none, and keep the soft limit below the hard one", http.StatusBadRequest)
	case a.Color != "" && !colorRe.MatchString(a.Color):
		http.Error(w, "color must look like #3d6fe0", http.StatusBadRequest)
	default:
		if err := s.st.UpdateAgent(a); err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) saveGoal(w http.ResponseWriter, r *http.Request) {
	var g store.Goal
	if !readJSON(w, r, &g) {
		return
	}
	if err := s.st.SaveGoal(r.PathValue("id"), g); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	if err := s.o.Start(r.PathValue("id")); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// revise re-runs an agent to make the change the user describes (see Orchestrator.Revise).
func (s *Server) revise(w http.ResponseWriter, r *http.Request) {
	var req struct{ Change string }
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.o.Revise(r.PathValue("id"), req.Change); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// getPlan returns the agent's plan waiting for the user's decision.
func (s *Server) getPlan(w http.ResponseWriter, r *http.Request) {
	p, err := s.st.PendingPlan(r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) || err == nil && !s.o.Awaiting(p.Agent) {
		http.Error(w, "no plan is waiting for a decision", http.StatusNotFound)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, p)
}

// decidePlan approves (possibly edited), sends back, or cancels the agent's waiting plan.
func (s *Server) decidePlan(w http.ResponseWriter, r *http.Request) {
	var d orch.Decision
	if !readJSON(w, r, &d) {
		return
	}
	if err := s.o.Decide(r.PathValue("id"), d); errors.Is(err, orch.ErrStale) {
		fail(w, http.StatusConflict, err)
		return
	} else if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	s.o.Stop(r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

// runAll starts every top-level agent's goals; managers run their own subtrees.
func (s *Server) runAll(w http.ResponseWriter, r *http.Request) {
	as, err := s.st.Agents(r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	started, errs := 0, []string{}
	for _, a := range as {
		if _, err := s.st.NextQueueItem(a.ID); a.Parent != "" || err != nil {
			continue // a report, or no goals to run
		}
		if err := s.o.StartQueue(a.ID); err != nil {
			errs = append(errs, err.Error())
		} else {
			started++
		}
	}
	writeJSON(w, map[string]any{"started": started, "errors": errs})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	es, err := s.st.Events(r.PathValue("id"), 1000)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, es)
}

// runs lists the agent's process calls with their time and tokens, for the Logs tab.
func (s *Server) runs(w http.ResponseWriter, r *http.Request) {
	rs, err := s.st.Runs(r.PathValue("id"), 500)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, rs)
}

func (s *Server) summary(w http.ResponseWriter, r *http.Request) {
	sum, err := s.st.Summary(r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	for id, st := range sum.Agents {
		st.Now = s.o.Hub.Now(id)
		sum.Agents[id] = st
	}
	writeJSON(w, sum)
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	st, err := s.st.Stats(r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, st)
}

func (s *Server) saveType(w http.ResponseWriter, r *http.Request) {
	var t store.AgentType
	if !readJSON(w, r, &t) {
		return
	}
	t.ID = r.PathValue("id") // "" on create
	// keep the name as typed ("Frontend Guy"), minus stray whitespace
	t.Name = strings.Join(strings.Fields(t.Name), " ")
	switch _, ok := agents.Runtimes[t.Runtime]; {
	case t.Name == "" || utf8.RuneCountInString(t.Name) > 40:
		http.Error(w, "type name: 1 to 40 characters", http.StatusBadRequest)
	case s.typeNameTaken(t):
		http.Error(w, "there's already a type called "+t.Name, http.StatusBadRequest)
	case !colorRe.MatchString(t.Color):
		http.Error(w, "color must look like #3d6fe0", http.StatusBadRequest)
	case !ok:
		http.Error(w, "unknown runtime "+t.Runtime, http.StatusBadRequest)
	default:
		if err := s.st.SaveType(&t); err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, t)
	}
}

// typeNameTaken reports whether another type already has t's name, ignoring case.
func (s *Server) typeNameTaken(t store.AgentType) bool {
	ts, _ := s.st.Types()
	for _, o := range ts {
		if o.ID != t.ID && strings.EqualFold(o.Name, t.Name) {
			return true
		}
	}
	return false
}

func (s *Server) deleteType(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteType(r.PathValue("id")); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sse streams a project's live updates as Server-Sent Events.
func (s *Server) sse(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	c := s.o.Hub.Subscribe(r.URL.Query().Get("project"))
	defer s.o.Hub.Unsubscribe(c)
	fmt.Fprintf(w, "data: {\"type\":\"hello\",\"build\":%q}\n\n", s.build) // lets an open page notice a restart onto new code
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-c:
			fmt.Fprintf(w, "data: %s\n\n", b)
			if s.o.Hub.Dropped(c) {
				fmt.Fprint(w, "data: {\"type\":\"resync\"}\n\n") // it missed updates: reload the state
			}
			flusher.Flush()
		}
	}
}
