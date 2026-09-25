package orch

import (
	"database/sql"
	"errors"
	"fmt"

	"arranger/internal/store"
)

// A queue runs an agent's goals one after another, each as its own run on the code the previous
// one left. A goal that fails pauses the queue, or is skipped, as the user chose.

// StartQueue starts running the agent's queued goals: the next one now, or when its current run ends.
func (o *Orchestrator) StartQueue(agentID string) error {
	if _, err := o.Store.NextQueueItem(agentID); errors.Is(err, sql.ErrNoRows) {
		return errors.New("the queue is empty; add a goal first")
	} else if err != nil {
		return err
	}
	on := true
	if err := o.Store.SetQueue(agentID, &on, ""); err != nil {
		return err
	}
	o.queueChanged(agentID)
	if o.IsRunning(agentID) {
		return nil // the next goal starts when this run ends
	}
	return o.runNext(agentID)
}

// PauseQueue stops the queue after the goal it's running.
func (o *Orchestrator) PauseQueue(agentID string) error {
	off := false
	err := o.Store.SetQueue(agentID, &off, "")
	o.queueChanged(agentID)
	return err
}

// runNext makes the next queued goal the agent's goal and runs it. With nothing left, the queue stops.
func (o *Orchestrator) runNext(agentID string) error {
	for {
		it, err := o.Store.NextQueueItem(agentID)
		if errors.Is(err, sql.ErrNoRows) {
			off := false
			o.Store.SetQueue(agentID, &off, "")
			o.queueChanged(agentID)
			return nil
		}
		if err != nil {
			return err
		}
		if err := o.Store.SaveGoal(agentID, store.Goal{Title: it.Title, Body: it.Body, Criteria: it.Criteria, Checks: it.Checks}); err != nil {
			return err
		}
		session, err := o.start(agentID, "")
		if err == nil {
			o.Store.StartQueueItem(it.ID, session)
			o.queueChanged(agentID)
			return nil
		}
		// it couldn't even start (say, its runtime isn't installed): that's a failure like any other
		o.Store.FinishQueueItem(it.ID, "failed")
		if q, _ := o.Store.Queue(agentID); q.OnFail != "skip" {
			off := false
			o.Store.SetQueue(agentID, &off, "")
			o.queueChanged(agentID)
			return fmt.Errorf("%q couldn't start: %w", it.Title, err)
		}
	}
}

// afterRun records how a queued goal's run ended and, while the queue is on, moves on: to the
// next goal after a success; after a failure, to the next or nowhere, as the user chose. A run the
// user stopped pauses the queue.
func (o *Orchestrator) afterRun(agentID, status string, session int64) {
	if it, err := o.Store.RunningQueueItem(agentID); err == nil && it.Session == session {
		o.Store.FinishQueueItem(it.ID, status)
		o.queueChanged(agentID)
	}
	q, err := o.Store.Queue(agentID)
	if err != nil || !q.Active {
		return
	}
	if status == "done" || status != "stopped" && q.OnFail == "skip" {
		o.runNext(agentID)
		return
	}
	off := false
	o.Store.SetQueue(agentID, &off, "")
	o.queueChanged(agentID)
}

// queueChanged tells the page the agent's queue changed.
func (o *Orchestrator) queueChanged(agentID string) {
	if _, pid, err := o.Store.Agent(agentID); err == nil {
		o.Hub.Publish(pid, map[string]any{"type": "queue", "agent": agentID})
	}
}
