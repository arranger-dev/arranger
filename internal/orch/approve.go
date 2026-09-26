package orch

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"arranger/internal/store"
)

// planTimeout is how long a plan waits for the user's decision before the run stops.
var planTimeout = 24 * time.Hour

// ErrStale means there's no plan waiting under that draft id: it was decided (maybe in another
// tab), replaced by a new plan, or its run ended.
var ErrStale = errors.New("this plan isn't waiting for a decision anymore; reload it")

// Decision is the user's answer to a manager's draft plan.
type Decision struct {
	Draft    int64      `json:"draft"`
	Action   string     `json:"action"`   // approve | replan | cancel
	Subgoals []subgoal  `json:"subgoals"` // approve, kind "plan": the plan as the user left it
	Changes  []revision `json:"changes"`  // approve, kind "revision"
	Feedback string     `json:"feedback"` // replan: what the new plan should do differently
}

// Awaiting reports whether the agent's run is waiting for the user to decide on its plan.
func (o *Orchestrator) Awaiting(agentID string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.approvals[agentID]
	return ok
}

// Decide hands the user's decision to the run waiting on the draft. An approved plan is checked
// the same way the manager's own plan is.
func (o *Orchestrator) Decide(agentID string, d Decision) error {
	o.mu.Lock()
	ch := o.approvals[agentID]
	o.mu.Unlock()
	p, err := o.Store.PendingPlan(agentID)
	if ch == nil || err != nil || p.ID != d.Draft {
		return ErrStale
	}
	var status string
	var final any
	switch d.Action {
	case "approve":
		kids, err := o.Store.Children(agentID)
		if err != nil {
			return err
		}
		byID, goals := map[string]store.Agent{}, map[string]store.Goal{}
		for _, k := range kids {
			byID[k.ID] = k
			goals[k.ID], _ = o.Store.Goal(k.ID)
		}
		if p.Kind == "revision" {
			if err := validateRevision(d.Changes, byID, goals); err != nil {
				return err
			}
			d.Subgoals, final = nil, revisionDraft{Changes: d.Changes}
		} else {
			if err := validatePlan(d.Subgoals, byID); err != nil {
				return err
			}
			d.Changes, final = nil, planDraft{Subgoals: d.Subgoals}
		}
		status = "approved"
	case "replan":
		status = "replanned"
	case "cancel":
		status = "cancelled"
	default:
		return fmt.Errorf("action must be approve, replan or cancel")
	}
	if !o.Store.DecidePlan(p.ID, status, d.Feedback, final) {
		return ErrStale
	}
	ch <- d // buffered, and DecidePlan lets only one decision through
	return nil
}

// await saves the proposed plan as a draft and blocks until the user decides on it. It returns
// the decision, or the run's final status when the run ended instead (stopped, or no decision
// in time). Waiting holds no parallel slot: those are only taken while an agent CLI runs.
func (j *job) await(kind string, proposed any) (Decision, string) {
	st := j.o.Store
	id, err := st.SavePlan(j.a.ID, kind, proposed)
	if err != nil {
		return Decision{}, j.fail("failed", "couldn't save the plan: "+err.Error())
	}
	ch := make(chan Decision, 1)
	j.o.mu.Lock()
	j.o.approvals[j.a.ID] = ch
	j.o.mu.Unlock()
	defer func() {
		j.o.mu.Lock()
		delete(j.o.approvals, j.a.ID)
		j.o.mu.Unlock()
	}()
	j.status("awaiting", nil)
	j.emit("plan", "plan ready: waiting for your approval", "")
	j.o.Hub.Publish(j.p.ID, map[string]any{"type": "plan", "agent": j.a.ID})

	timer := time.NewTimer(planTimeout)
	defer timer.Stop()
	select {
	case d := <-ch:
		return d, ""
	case <-j.ctx.Done():
		st.DecidePlan(id, "cancelled", "", nil)
		return Decision{}, j.fail("stopped", "stopped by user")
	case <-timer.C:
		if st.DecidePlan(id, "expired", "", nil) {
			return Decision{}, j.fail("stopped", "the plan wasn't approved in time")
		}
		return <-ch, "" // the user decided just as time ran out
	}
}

// sameJSON reports whether a and b encode the same, e.g. a plan and the plan the user approved.
func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
