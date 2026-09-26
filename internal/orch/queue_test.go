package orch

import (
	"testing"
	"time"

	"arranger/internal/store"
)

// waitQueue waits until the agent's queue stops running and nothing is running.
func waitQueue(t *testing.T, o *Orchestrator, id string) store.Queue {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if q, _ := o.Store.Queue(id); !q.Active && !o.IsRunning(id) {
			return q
		}
	}
	t.Fatal("queue never finished")
	return store.Queue{}
}

// A queue runs its goals one after another, each as its own run; a failure pauses it or is
// skipped, as chosen.
func TestQueue(t *testing.T) {
	for _, onFail := range []string{"pause", "skip"} {
		t.Run(onFail, func(t *testing.T) {
			o := setup(t)
			p, _ := o.Store.CreateProject(store.Project{Name: "t", Repo: newRepo(t), Base: "main"})
			o.Store.SaveArrangement(p.ID, []store.Agent{{ID: "w", Name: "W", Role: "programmer", Runtime: "generic", Args: "true"}})
			if err := o.StartQueue("w"); err == nil {
				t.Fatal("an empty queue shouldn't start")
			}
			for _, it := range []store.QueueItem{
				{Agent: "w", Title: "first", Checks: "true"},
				{Agent: "w", Title: "second", Checks: "false"}, // fails every attempt
				{Agent: "w", Title: "third", Checks: "true"},
			} {
				if err := o.Store.AddQueueItem(&it); err != nil {
					t.Fatal(err)
				}
			}
			o.Store.SetQueue("w", nil, onFail)
			if err := o.StartQueue("w"); err != nil {
				t.Fatal(err)
			}
			q := waitQueue(t, o, "w")
			got := map[string]string{}
			sessions := map[int64]bool{}
			for _, it := range q.Items {
				got[it.Title] = it.Status
				if it.Session != 0 {
					sessions[it.Session] = true
				}
			}
			want := map[string]string{"first": "done", "second": "failed", "third": "queued"}
			if onFail == "skip" {
				want["third"] = "done"
			}
			for title, st := range want {
				if got[title] != st {
					t.Errorf("%s: %q, want %q (all: %v)", title, got[title], st, got)
				}
			}
			if len(sessions) != len(want)-map[string]int{"pause": 1, "skip": 0}[onFail] {
				t.Errorf("each goal that ran should have its own run: %v", sessions)
			}
			s, _ := o.Store.Summary(p.ID)
			if paused := s.Agents["w"].QueuePaused; (onFail == "pause") != (paused == "second") {
				t.Errorf("paused on: %q", paused)
			}
		})
	}
}

// Stopping the goal a queue is running pauses the queue instead of moving on.
func TestStopPausesQueue(t *testing.T) {
	o := setup(t)
	p, _ := o.Store.CreateProject(store.Project{Name: "t", Repo: newRepo(t), Base: "main"})
	o.Store.SaveArrangement(p.ID, []store.Agent{{ID: "w", Name: "W", Role: "programmer", Runtime: "generic", Args: "sleep 30"}})
	for _, title := range []string{"slow", "next"} {
		o.Store.AddQueueItem(&store.QueueItem{Agent: "w", Title: title, Checks: "true"})
	}
	o.Store.SetQueue("w", nil, "skip") // even with skip, a stop means stop
	if err := o.StartQueue("w"); err != nil {
		t.Fatal(err)
	}
	for !o.IsRunning("w") {
		time.Sleep(20 * time.Millisecond)
	}
	o.Stop("w")
	q := waitQueue(t, o, "w")
	if q.Active || q.Items[0].Title != "next" || q.Items[0].Status != "queued" {
		t.Fatalf("queue after stop: %+v", q)
	}
}
