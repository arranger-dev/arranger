package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// QueueItem is a goal waiting in an agent's queue, or one the queue already ran.
type QueueItem struct {
	ID       int64  `json:"id"`
	Agent    string `json:"agent"`
	Pos      int    `json:"pos"`
	Title    string `json:"title"`
	Body     string `json:"body"`
	Criteria string `json:"criteria"`
	Checks   string `json:"checks"`
	Status   string `json:"status"`  // queued | running | done | failed | blocked | stopped | skipped
	Session  int64  `json:"session"` // its run in the log, once it ran
	Finished int64  `json:"finished"`
}

// Queue is an agent's queue: its goals and how it runs them.
type Queue struct {
	Active bool        `json:"active"` // runs the next goal when one ends
	OnFail string      `json:"onFail"` // pause | skip
	Items  []QueueItem `json:"items"`
}

const queueCols = `id, agent_id, pos, title, body, criteria, checks, status, session, finished`

func scanQueueItem(sc interface{ Scan(...any) error }) (q QueueItem, err error) {
	err = sc.Scan(&q.ID, &q.Agent, &q.Pos, &q.Title, &q.Body, &q.Criteria, &q.Checks, &q.Status, &q.Session, &q.Finished)
	return
}

// Queue returns the agent's queue, waiting goals in order and then finished ones, newest first.
func (s *Store) Queue(agentID string) (Queue, error) {
	q := Queue{OnFail: "pause", Items: []QueueItem{}}
	if err := s.db.QueryRow(`SELECT queue_active, queue_on_fail FROM agents WHERE id=?`, agentID).Scan(&q.Active, &q.OnFail); err != nil {
		return q, err
	}
	rows, err := s.db.Query(`SELECT `+queueCols+` FROM queue WHERE agent_id=?
		ORDER BY status IN ('queued', 'running') DESC, CASE WHEN status IN ('queued', 'running') THEN pos ELSE -finished END, id`, agentID)
	if err != nil {
		return q, err
	}
	defer rows.Close()
	for rows.Next() {
		it, err := scanQueueItem(rows)
		if err != nil {
			return q, err
		}
		q.Items = append(q.Items, it)
	}
	return q, rows.Err()
}

// cleanItem checks a goal for the queue: it needs a title and a check, like any goal.
func cleanItem(it *QueueItem) error {
	it.Title, it.Checks = strings.TrimSpace(it.Title), strings.TrimSpace(it.Checks)
	if it.Title == "" {
		return errors.New("give the goal a title")
	}
	if it.Checks == "" {
		return errors.New("add at least one check: nothing is done until a check passes")
	}
	return nil
}

// AddQueueItem puts a goal at the end of the agent's queue.
func (s *Store) AddQueueItem(it *QueueItem) error {
	if err := cleanItem(it); err != nil {
		return err
	}
	res, err := s.db.Exec(`INSERT INTO queue(agent_id, pos, title, body, criteria, checks)
		VALUES(?, (SELECT coalesce(max(pos), 0) + 1 FROM queue WHERE agent_id=?), ?, ?, ?, ?)`,
		it.Agent, it.Agent, it.Title, it.Body, it.Criteria, it.Checks)
	if err != nil {
		return err
	}
	it.ID, _ = res.LastInsertId()
	it.Status = "queued"
	return nil
}

// errNotQueued refuses changes to a goal the queue already started or finished.
var errNotQueued = errors.New("that goal already ran or is running; only waiting goals can change")

// UpdateQueueItem changes a waiting goal.
func (s *Store) UpdateQueueItem(it QueueItem) error {
	if err := cleanItem(&it); err != nil {
		return err
	}
	return s.onlyQueued(s.db.Exec(`UPDATE queue SET title=?, body=?, criteria=?, checks=? WHERE id=? AND agent_id=? AND status='queued'`,
		it.Title, it.Body, it.Criteria, it.Checks, it.ID, it.Agent))
}

// SkipQueueItem marks a waiting goal as skipped; it stays in the history.
func (s *Store) SkipQueueItem(agentID string, id int64) error {
	return s.onlyQueued(s.db.Exec(`UPDATE queue SET status='skipped', finished=? WHERE id=? AND agent_id=? AND status='queued'`,
		time.Now().UnixMilli(), id, agentID))
}

// DeleteQueueItem removes a goal that isn't running.
func (s *Store) DeleteQueueItem(agentID string, id int64) error {
	res, err := s.db.Exec(`DELETE FROM queue WHERE id=? AND agent_id=? AND status != 'running'`, id, agentID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("that goal is running; stop it first")
	}
	return nil
}

func (s *Store) onlyQueued(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotQueued
	}
	return nil
}

// ReorderQueue puts the agent's waiting goals in the order of ids.
func (s *Store) ReorderQueue(agentID string, ids []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, id := range ids {
		if _, err := tx.Exec(`UPDATE queue SET pos=? WHERE id=? AND agent_id=? AND status='queued'`, i+1, id, agentID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetQueue changes how the agent's queue runs: active, and what a failure does ("pause" or "skip").
func (s *Store) SetQueue(agentID string, active *bool, onFail string) error {
	if onFail != "" && onFail != "pause" && onFail != "skip" {
		return fmt.Errorf("when a goal fails: pause or skip, not %q", onFail)
	}
	if active != nil {
		if _, err := s.db.Exec(`UPDATE agents SET queue_active=? WHERE id=?`, *active, agentID); err != nil {
			return err
		}
	}
	if onFail != "" {
		_, err := s.db.Exec(`UPDATE agents SET queue_on_fail=? WHERE id=?`, onFail, agentID)
		return err
	}
	return nil
}

// QueueItem returns one goal of the agent's list.
func (s *Store) QueueItem(agentID string, id int64) (QueueItem, error) {
	return scanQueueItem(s.db.QueryRow(`SELECT `+queueCols+` FROM queue WHERE agent_id=? AND id=?`, agentID, id))
}

// NextQueueItem is the agent's next waiting goal; sql.ErrNoRows when there's none.
func (s *Store) NextQueueItem(agentID string) (QueueItem, error) {
	return scanQueueItem(s.db.QueryRow(`SELECT `+queueCols+` FROM queue WHERE agent_id=? AND status='queued' ORDER BY pos, id LIMIT 1`, agentID))
}

// RunningQueueItem is the goal the agent's queue is running; sql.ErrNoRows when there's none.
func (s *Store) RunningQueueItem(agentID string) (QueueItem, error) {
	return scanQueueItem(s.db.QueryRow(`SELECT `+queueCols+` FROM queue WHERE agent_id=? AND status='running' LIMIT 1`, agentID))
}

// StartQueueItem marks a goal as running in the given log session.
func (s *Store) StartQueueItem(id, session int64) {
	s.db.Exec(`UPDATE queue SET status='running', session=? WHERE id=?`, session, id)
}

// FinishQueueItem records how a goal's run ended.
func (s *Store) FinishQueueItem(id int64, status string) {
	s.db.Exec(`UPDATE queue SET status=?, finished=? WHERE id=?`, status, time.Now().UnixMilli(), id)
}
