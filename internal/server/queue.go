package server

import (
	"encoding/json"
	"net/http"
	"strconv"

	"arranger/internal/store"
)

// Queue endpoints: an agent's goals waiting to run one after another (see orch.StartQueue).

func (s *Server) getQueue(w http.ResponseWriter, r *http.Request) {
	q, err := s.st.Queue(r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, q)
}

func (s *Server) addQueueItem(w http.ResponseWriter, r *http.Request) {
	var it store.QueueItem
	if !readJSON(w, r, &it) {
		return
	}
	it.Agent = r.PathValue("id")
	if _, _, err := s.st.Agent(it.Agent); err != nil {
		http.Error(w, "agent not found (save the arrangement first)", http.StatusNotFound)
		return
	}
	if err := s.st.AddQueueItem(&it); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	s.o.Hub.Publish(s.projectOf(it.Agent), map[string]any{"type": "queue", "agent": it.Agent})
	writeJSON(w, it)
}

// queueItem runs a change on one goal of the queue, named by the {item} path value.
func (s *Server) queueItem(change func(agent string, id int64, r *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("item"), 10, 64)
		if err != nil {
			http.Error(w, "bad goal id", http.StatusBadRequest)
			return
		}
		agent := r.PathValue("id")
		if err := change(agent, id, r); err != nil {
			fail(w, http.StatusConflict, err)
			return
		}
		s.o.Hub.Publish(s.projectOf(agent), map[string]any{"type": "queue", "agent": agent})
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) editQueueItem(agent string, id int64, r *http.Request) error {
	var it store.QueueItem
	if err := json.NewDecoder(r.Body).Decode(&it); err != nil {
		return err
	}
	it.ID, it.Agent = id, agent
	return s.st.UpdateQueueItem(it)
}

func (s *Server) reorderQueue(w http.ResponseWriter, r *http.Request) {
	var req struct{ IDs []int64 }
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.st.ReorderQueue(r.PathValue("id"), req.IDs); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	s.o.Hub.Publish(s.projectOf(r.PathValue("id")), map[string]any{"type": "queue", "agent": r.PathValue("id")})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) startQueue(w http.ResponseWriter, r *http.Request) {
	if err := s.o.StartQueue(r.PathValue("id")); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) pauseQueue(w http.ResponseWriter, r *http.Request) {
	if err := s.o.PauseQueue(r.PathValue("id")); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) queueSettings(w http.ResponseWriter, r *http.Request) {
	var req struct{ OnFail string }
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.st.SetQueue(r.PathValue("id"), nil, req.OnFail); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) projectOf(agentID string) string {
	_, pid, _ := s.st.Agent(agentID)
	return pid
}
