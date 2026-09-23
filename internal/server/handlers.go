package server

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"arranger/internal/agents"
	"arranger/internal/git"
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
	if err := s.st.SaveArrangement(r.PathValue("id"), as); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
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
	writeJSON(w, map[string]any{"agent": a, "goal": g})
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

func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	s.o.Stop(r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

// runAll starts every top-level agent that has a goal; managers run their own subtrees.
func (s *Server) runAll(w http.ResponseWriter, r *http.Request) {
	as, err := s.st.Agents(r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	started, errs := 0, []string{}
	for _, a := range as {
		if g, _ := s.st.Goal(a.ID); a.Parent != "" || g.Title == "" {
			continue
		}
		if err := s.o.Start(a.ID); err != nil {
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
	t.Name = strings.ToLower(strings.TrimSpace(t.Name))
	switch _, ok := agents.Runtimes[t.Runtime]; {
	case !safeID.MatchString(t.Name):
		http.Error(w, "type name: letters, digits, - or _ only", http.StatusBadRequest)
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
			flusher.Flush()
		}
	}
}
