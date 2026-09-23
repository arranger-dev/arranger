// Package store keeps arranger's state in one SQLite file: projects, agents, goals, runs,
// the event log, decisions and custom agent types.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, so the binary stays cgo-free
)

const schema = `
CREATE TABLE IF NOT EXISTS projects (id TEXT PRIMARY KEY, name TEXT NOT NULL, repo TEXT NOT NULL DEFAULT '', base TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS agents (
  id TEXT PRIMARY KEY, project_id TEXT NOT NULL, name TEXT NOT NULL, role TEXT NOT NULL DEFAULT '',
  parent TEXT NOT NULL DEFAULT '', pos INTEGER NOT NULL DEFAULT 0,
  runtime TEXT NOT NULL DEFAULT 'claude', model TEXT NOT NULL DEFAULT '', prompt TEXT NOT NULL DEFAULT '', args TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS goals (
  agent_id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', body TEXT NOT NULL DEFAULT '',
  criteria TEXT NOT NULL DEFAULT '', checks TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'idle', attempts INTEGER NOT NULL DEFAULT 0, feedback TEXT NOT NULL DEFAULT '',
  passed INTEGER NOT NULL DEFAULT 0, total INTEGER NOT NULL DEFAULT 0, adds INTEGER NOT NULL DEFAULT 0, dels INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS runs (
  id INTEGER PRIMARY KEY, agent_id TEXT NOT NULL, attempt INTEGER NOT NULL, started INTEGER NOT NULL, ended INTEGER NOT NULL DEFAULT 0,
  exit INTEGER NOT NULL DEFAULT 0, cost REAL NOT NULL DEFAULT 0, tok_in INTEGER NOT NULL DEFAULT 0, tok_out INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY, run_id INTEGER NOT NULL, agent_id TEXT NOT NULL, ts INTEGER NOT NULL,
  kind TEXT NOT NULL, text TEXT NOT NULL, raw TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS events_agent ON events(agent_id, id);
CREATE TABLE IF NOT EXISTS agent_types (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, color TEXT NOT NULL DEFAULT '#3d6fe0', runtime TEXT NOT NULL DEFAULT 'claude',
  model TEXT NOT NULL DEFAULT '', args TEXT NOT NULL DEFAULT '', prompt TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS decisions (
  id INTEGER PRIMARY KEY, agent_id TEXT NOT NULL, ts INTEGER NOT NULL, kind TEXT NOT NULL, json TEXT NOT NULL);
`

// migrations add columns to tables created by older versions; "duplicate column" errors are expected.
var migrations = []string{
	`ALTER TABLE goals ADD COLUMN base TEXT NOT NULL DEFAULT ''`,          // commit the agent's work is diffed against
	`ALTER TABLE goals ADD COLUMN notes TEXT NOT NULL DEFAULT ''`,         // changes the user removed; the agent must not re-add them
	`ALTER TABLE agents ADD COLUMN token_soft INTEGER NOT NULL DEFAULT 0`, // warn above this many tokens per run; 0 = off
	`ALTER TABLE agents ADD COLUMN token_hard INTEGER NOT NULL DEFAULT 0`, // stop above this many tokens per run; 0 = off
	`ALTER TABLE goals ADD COLUMN since INTEGER NOT NULL DEFAULT 0`,       // when the current run started (ms)
	`ALTER TABLE agents ADD COLUMN color TEXT NOT NULL DEFAULT ''`,        // overrides the role's color; '' = role color
	`ALTER TABLE agents ADD COLUMN x REAL`,                                // canvas position; NULL = lay out automatically
	`ALTER TABLE agents ADD COLUMN y REAL`,
	// a crash mid-run leaves stale statuses behind
	`UPDATE goals SET status = 'stopped' WHERE status IN ('running', 'verifying', 'planning', 'waiting', 'reviewing')`,
}

type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Repo string `json:"repo"`
	Base string `json:"base"`
}

type Agent struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Role    string   `json:"role"`
	Parent  string   `json:"parent"`
	Runtime string   `json:"runtime"`
	Model   string   `json:"model"`
	Prompt  string   `json:"prompt"`
	Args    string   `json:"args"`
	Soft    int      `json:"tokenSoft"`
	Hard    int      `json:"tokenHard"`
	Color   string   `json:"color"`
	X       *float64 `json:"x"` // nil until the user places the box
	Y       *float64 `json:"y"`
}

// AgentType is a user-defined palette entry: a role with default settings for new agents.
type AgentType struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Color   string `json:"color"`
	Runtime string `json:"runtime"`
	Model   string `json:"model"`
	Args    string `json:"args"`
	Prompt  string `json:"prompt"`
}

type Goal struct {
	Title    string `json:"title"`
	Body     string `json:"body"`
	Criteria string `json:"criteria"`
	Checks   string `json:"checks"` // one shell command per line
	Status   string `json:"status"`
	Attempts int    `json:"attempts"`
	Feedback string `json:"feedback"`
	Passed   int    `json:"passed"`
	Total    int    `json:"total"`
	Adds     int    `json:"adds"`
	Dels     int    `json:"dels"`
	Base     string `json:"base"`
	Notes    string `json:"notes"`
}

// LogEvent is one line of an agent's activity log.
type LogEvent struct {
	ID    int64  `json:"id"`
	Agent string `json:"agent"`
	Run   int64  `json:"run"`
	TS    int64  `json:"ts"`
	Kind  string `json:"kind"`
	Text  string `json:"text"`
	Raw   string `json:"raw,omitempty"`
}

// DemoAgents seed the first project so the arrange screen isn't empty.
var DemoAgents = []Agent{
	{ID: "lead", Name: "Lead", Role: "manager"},
	{ID: "be", Name: "Backend", Role: "manager", Parent: "lead"},
	{ID: "fe", Name: "Frontend", Role: "coder", Parent: "lead"},
	{ID: "rev", Name: "Reviewer", Role: "reviewer", Parent: "lead"},
	{ID: "api", Name: "API Coder", Role: "coder", Parent: "be"},
	{ID: "db", Name: "DB Coder", Role: "coder", Parent: "be"},
	{ID: "test", Name: "Tester", Role: "tester", Parent: "be"},
}

type Store struct{ db *sql.DB }

// Open opens (or creates) the database at path, migrates it, and seeds a demo project on first use.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // ponytail: one writer connection, enough for a local tool
	if _, err = db.Exec(schema); err != nil {
		return nil, err
	}
	for _, m := range migrations {
		db.Exec(m)
	}
	s := &Store{db: db}
	var n int
	db.QueryRow(`SELECT count(*) FROM projects`).Scan(&n)
	if n > 0 {
		return s, nil
	}
	p, err := s.CreateProject(Project{Name: "Demo"})
	if err != nil {
		return nil, err
	}
	as := make([]Agent, len(DemoAgents))
	for i, a := range DemoAgents {
		a.ID = p.ID + "-" + a.ID
		if a.Parent != "" {
			a.Parent = p.ID + "-" + a.Parent
		}
		as[i] = a
	}
	return s, s.SaveArrangement(p.ID, as)
}

func (s *Store) Close() error { return s.db.Close() }

// SQL exposes the database for tests and one-off diagnostics.
func (s *Store) SQL() *sql.DB { return s.db }

func newID(prefix string) string {
	return fmt.Sprintf("%s%x", prefix, time.Now().UnixNano())
}

func (s *Store) CreateProject(p Project) (Project, error) {
	p.ID = newID("p")
	_, err := s.db.Exec(`INSERT INTO projects(id, name, repo, base) VALUES(?,?,?,?)`, p.ID, p.Name, p.Repo, p.Base)
	return p, err
}

func (s *Store) UpdateProject(p Project) error {
	_, err := s.db.Exec(`UPDATE projects SET name=?, repo=?, base=? WHERE id=?`, p.Name, p.Repo, p.Base, p.ID)
	return err
}

func (s *Store) Project(id string) (p Project, err error) {
	err = s.db.QueryRow(`SELECT id, name, repo, base FROM projects WHERE id=?`, id).Scan(&p.ID, &p.Name, &p.Repo, &p.Base)
	return
}

func (s *Store) Projects() ([]Project, error) {
	rows, err := s.db.Query(`SELECT id, name, repo, base FROM projects ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ps []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Repo, &p.Base); err != nil {
			return nil, err
		}
		ps = append(ps, p)
	}
	return ps, rows.Err()
}

const agentCols = `id, name, role, parent, runtime, model, prompt, args, token_soft, token_hard, color, x, y`

func scanAgent(sc interface{ Scan(...any) error }, extra ...any) (a Agent, err error) {
	var x, y sql.NullFloat64
	err = sc.Scan(append([]any{&a.ID, &a.Name, &a.Role, &a.Parent, &a.Runtime, &a.Model, &a.Prompt, &a.Args, &a.Soft, &a.Hard, &a.Color, &x, &y}, extra...)...)
	if x.Valid && y.Valid {
		a.X, a.Y = &x.Float64, &y.Float64
	}
	return
}

// Agents lists a project's agents in the order the user arranged them.
func (s *Store) Agents(projectID string) ([]Agent, error) {
	return s.queryAgents("project_id", projectID)
}

// Children lists the agents that report to agentID.
func (s *Store) Children(agentID string) ([]Agent, error) { return s.queryAgents("parent", agentID) }

// queryAgents returns agents where col = v, in tree order; col must be a trusted column name.
func (s *Store) queryAgents(col, v string) ([]Agent, error) {
	rows, err := s.db.Query(`SELECT `+agentCols+` FROM agents WHERE `+col+`=? ORDER BY pos`, v)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var as []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		as = append(as, a)
	}
	return as, rows.Err()
}

// Agent returns the agent and its project id.
func (s *Store) Agent(id string) (a Agent, projectID string, err error) {
	a, err = scanAgent(s.db.QueryRow(`SELECT `+agentCols+`, project_id FROM agents WHERE id=?`, id), &projectID)
	return
}

// UpdateAgent saves an agent's settings (not its place in the tree).
func (s *Store) UpdateAgent(a Agent) error {
	_, err := s.db.Exec(`UPDATE agents SET name=?, runtime=?, model=?, prompt=?, args=?, token_soft=?, token_hard=?, color=? WHERE id=?`,
		a.Name, a.Runtime, a.Model, a.Prompt, a.Args, a.Soft, a.Hard, a.Color, a.ID)
	return err
}

// SaveArrangement makes the project's tree match as: upserts name, role, parent, order and
// position, keeps each existing agent's settings, and deletes agents (and goals) that are gone.
func (s *Store) SaveArrangement(projectID string, as []Agent) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	keep := map[string]bool{}
	for i, a := range as {
		var owner string
		tx.QueryRow(`SELECT project_id FROM agents WHERE id=?`, a.ID).Scan(&owner)
		if owner != "" && owner != projectID {
			return fmt.Errorf("agent %s belongs to another project", a.ID)
		}
		rt := a.Runtime
		if rt == "" {
			rt = "claude"
		}
		// settings only apply to new agents (e.g. from a custom type); existing ones keep theirs
		if _, err := tx.Exec(`INSERT INTO agents(id, project_id, name, role, parent, pos, runtime, model, args, prompt, color, token_soft, token_hard, x, y) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET name=excluded.name, role=excluded.role, parent=excluded.parent, pos=excluded.pos, x=excluded.x, y=excluded.y`,
			a.ID, projectID, a.Name, a.Role, a.Parent, i, rt, a.Model, a.Args, a.Prompt, a.Color, a.Soft, a.Hard, a.X, a.Y); err != nil {
			return err
		}
		keep[a.ID] = true
	}
	rows, err := tx.Query(`SELECT id FROM agents WHERE project_id=?`, projectID)
	if err != nil {
		return err
	}
	var gone []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		if !keep[id] {
			gone = append(gone, id)
		}
	}
	rows.Close()
	for _, id := range gone {
		if _, err := tx.Exec(`DELETE FROM agents WHERE id=?`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM goals WHERE agent_id=?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Goal returns an agent's goal; an agent without one gets an empty, idle goal.
func (s *Store) Goal(agentID string) (g Goal, err error) {
	err = s.db.QueryRow(`SELECT title, body, criteria, checks, status, attempts, feedback, passed, total, adds, dels, base, notes FROM goals WHERE agent_id=?`, agentID).
		Scan(&g.Title, &g.Body, &g.Criteria, &g.Checks, &g.Status, &g.Attempts, &g.Feedback, &g.Passed, &g.Total, &g.Adds, &g.Dels, &g.Base, &g.Notes)
	if err == sql.ErrNoRows {
		return Goal{Status: "idle"}, nil
	}
	return
}

// SaveGoal stores what the user (or a manager) asked for; run-time fields are untouched.
func (s *Store) SaveGoal(agentID string, g Goal) error {
	_, err := s.db.Exec(`INSERT INTO goals(agent_id, title, body, criteria, checks) VALUES(?,?,?,?,?)
		ON CONFLICT(agent_id) DO UPDATE SET title=excluded.title, body=excluded.body, criteria=excluded.criteria, checks=excluded.checks`,
		agentID, g.Title, g.Body, g.Criteria, g.Checks)
	return err
}

// SetGoal updates run-time fields of a goal, creating the row if needed; keys must be trusted column names.
func (s *Store) SetGoal(agentID string, kv map[string]any) {
	s.db.Exec(`INSERT OR IGNORE INTO goals(agent_id) VALUES(?)`, agentID)
	for col, v := range kv {
		if _, err := s.db.Exec(`UPDATE goals SET `+col+`=? WHERE agent_id=?`, v, agentID); err != nil {
			log.Printf("SetGoal %s: %v", col, err)
		}
	}
}

// AddEvent appends to the log, filling in the event's id and timestamp.
func (s *Store) AddEvent(e *LogEvent) {
	e.TS = time.Now().UnixMilli()
	res, err := s.db.Exec(`INSERT INTO events(run_id, agent_id, ts, kind, text, raw) VALUES(?,?,?,?,?,?)`, e.Run, e.Agent, e.TS, e.Kind, e.Text, e.Raw)
	if err != nil {
		log.Printf("AddEvent: %v", err)
		return
	}
	e.ID, _ = res.LastInsertId()
}

// Events returns the last limit events for an agent, oldest first.
func (s *Store) Events(agentID string, limit int) ([]LogEvent, error) {
	rows, err := s.db.Query(`SELECT * FROM (SELECT id, run_id, ts, kind, text, raw FROM events WHERE agent_id=? ORDER BY id DESC LIMIT ?) ORDER BY id`, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	es := []LogEvent{}
	for rows.Next() {
		e := LogEvent{Agent: agentID}
		if err := rows.Scan(&e.ID, &e.Run, &e.TS, &e.Kind, &e.Text, &e.Raw); err != nil {
			return nil, err
		}
		es = append(es, e)
	}
	return es, rows.Err()
}

// AddDecision records a manager's plan or review verdicts.
func (s *Store) AddDecision(agentID, kind string, v any) {
	b, _ := json.Marshal(v)
	if _, err := s.db.Exec(`INSERT INTO decisions(agent_id, ts, kind, json) VALUES(?,?,?,?)`, agentID, time.Now().UnixMilli(), kind, string(b)); err != nil {
		log.Printf("AddDecision: %v", err)
	}
}

// NewRun starts a run record for one attempt and returns its id.
func (s *Store) NewRun(agentID string, attempt int) int64 {
	res, err := s.db.Exec(`INSERT INTO runs(agent_id, attempt, started) VALUES(?,?,?)`, agentID, attempt, time.Now().UnixMilli())
	if err != nil {
		log.Printf("NewRun: %v", err)
		return 0
	}
	id, _ := res.LastInsertId()
	return id
}

// UpdateRun records usage so far; exit >= 0 also marks the run finished.
func (s *Store) UpdateRun(id int64, cost float64, in, out, exit int) {
	q, args := `UPDATE runs SET cost=?, tok_in=?, tok_out=? WHERE id=?`, []any{cost, in, out, id}
	if exit >= 0 {
		q, args = `UPDATE runs SET cost=?, tok_in=?, tok_out=?, exit=?, ended=? WHERE id=?`, []any{cost, in, out, exit, time.Now().UnixMilli(), id}
	}
	if _, err := s.db.Exec(q, args...); err != nil {
		log.Printf("UpdateRun: %v", err)
	}
}

type Summary struct {
	Agents map[string]AgentStatus `json:"agents"`
	Cost   float64                `json:"cost"`
}

// AgentStatus is what the arrange screen shows on each box.
type AgentStatus struct {
	Goal
	Now       string  `json:"now"`       // latest activity; filled in by the caller
	Since     int64   `json:"since"`     // current run's start, ms
	Cost      float64 `json:"cost"`      // this run
	Tokens    int     `json:"tokens"`    // this run
	AllCost   float64 `json:"allCost"`   // every run ever
	AllTokens int     `json:"allTokens"` // every run ever
	Soft      int     `json:"tokenSoft"`
	Hard      int     `json:"tokenHard"`
}

func (s *Store) Summary(projectID string) (Summary, error) {
	sum := Summary{Agents: map[string]AgentStatus{}}
	rows, err := s.db.Query(`SELECT a.id, coalesce(g.title,''), coalesce(g.status,'idle'), coalesce(g.attempts,0),
		coalesce(g.passed,0), coalesce(g.total,0), coalesce(g.adds,0), coalesce(g.dels,0), coalesce(g.since,0), a.token_soft, a.token_hard,
		(SELECT coalesce(sum(cost),0) FROM runs r WHERE r.agent_id=a.id AND r.started >= coalesce(g.since,0)),
		(SELECT coalesce(sum(tok_in+tok_out),0) FROM runs r WHERE r.agent_id=a.id AND r.started >= coalesce(g.since,0)),
		(SELECT coalesce(sum(cost),0) FROM runs r WHERE r.agent_id=a.id),
		(SELECT coalesce(sum(tok_in+tok_out),0) FROM runs r WHERE r.agent_id=a.id)
		FROM agents a LEFT JOIN goals g ON g.agent_id = a.id WHERE a.project_id=?`, projectID)
	if err != nil {
		return sum, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var st AgentStatus
		if err := rows.Scan(&id, &st.Title, &st.Status, &st.Attempts, &st.Passed, &st.Total, &st.Adds, &st.Dels, &st.Since,
			&st.Soft, &st.Hard, &st.Cost, &st.Tokens, &st.AllCost, &st.AllTokens); err != nil {
			return sum, err
		}
		sum.Agents[id] = st
		sum.Cost += st.AllCost
	}
	return sum, rows.Err()
}

// Usage is time, tokens and money spent, and how runs ended.
type Usage struct {
	Runs    int     `json:"runs"`
	Seconds float64 `json:"seconds"` // agent process time
	Tokens  int     `json:"tokens"`
	Cost    float64 `json:"cost"`
	Done    int     `json:"done"`
	Failed  int     `json:"failed"` // failed or blocked
}

type AgentStats struct {
	Usage
	ID       string `json:"id"`
	Name     string `json:"name"`
	Runtime  string `json:"runtime"`
	Status   string `json:"status"`
	Attempts int    `json:"attempts"`
	Adds     int    `json:"adds"`
	Dels     int    `json:"dels"`
}

type RuntimeStats struct {
	Usage
	Runtime string `json:"runtime"`
	Agents  int    `json:"agents"`
}

// Stats answers "how is this project going, and what is it costing": totals, first-try
// success, and breakdowns per agent and per runtime (useful for comparing agent CLIs).
type Stats struct {
	Usage
	Goals    int            `json:"goals"`
	FirstTry int            `json:"firstTry"` // goals done without a retry
	Adds     int            `json:"adds"`
	Dels     int            `json:"dels"`
	Agents   []AgentStats   `json:"agents"`
	Runtimes []RuntimeStats `json:"runtimes"`
}

func (s *Store) Stats(projectID string) (Stats, error) {
	st := Stats{Agents: []AgentStats{}, Runtimes: []RuntimeStats{}}
	rows, err := s.db.Query(`SELECT a.id, a.name, a.runtime, coalesce(g.title,''), coalesce(g.status,'idle'), coalesce(g.attempts,0),
		coalesce(g.adds,0), coalesce(g.dels,0),
		(SELECT count(*) FROM runs r WHERE r.agent_id=a.id),
		(SELECT coalesce(sum(CASE WHEN ended > started THEN ended-started ELSE 0 END),0)/1000.0 FROM runs r WHERE r.agent_id=a.id),
		(SELECT coalesce(sum(tok_in+tok_out),0) FROM runs r WHERE r.agent_id=a.id),
		(SELECT coalesce(sum(cost),0) FROM runs r WHERE r.agent_id=a.id)
		FROM agents a LEFT JOIN goals g ON g.agent_id=a.id WHERE a.project_id=? ORDER BY a.pos`, projectID)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	byRT := map[string]*RuntimeStats{}
	for rows.Next() {
		var a AgentStats
		var title string
		if err := rows.Scan(&a.ID, &a.Name, &a.Runtime, &title, &a.Status, &a.Attempts, &a.Adds, &a.Dels, &a.Runs, &a.Seconds, &a.Tokens, &a.Cost); err != nil {
			return st, err
		}
		switch a.Status {
		case "done":
			a.Done = 1
		case "failed", "blocked":
			a.Failed = 1
		}
		if title != "" {
			st.Goals++
		}
		if a.Done == 1 && a.Attempts <= 1 {
			st.FirstTry++
		}
		st.Agents = append(st.Agents, a)
		st.Usage.add(a.Usage)
		st.Adds, st.Dels = st.Adds+a.Adds, st.Dels+a.Dels
		r := byRT[a.Runtime]
		if r == nil {
			r = &RuntimeStats{Runtime: a.Runtime}
			byRT[a.Runtime] = r
		}
		r.Agents++
		r.Usage.add(a.Usage)
	}
	for _, r := range byRT {
		st.Runtimes = append(st.Runtimes, *r)
	}
	sort.Slice(st.Runtimes, func(i, j int) bool { return st.Runtimes[i].Cost > st.Runtimes[j].Cost })
	return st, rows.Err()
}

func (u *Usage) add(o Usage) {
	u.Runs += o.Runs
	u.Seconds += o.Seconds
	u.Tokens += o.Tokens
	u.Cost += o.Cost
	u.Done += o.Done
	u.Failed += o.Failed
}

func (s *Store) Types() ([]AgentType, error) {
	rows, err := s.db.Query(`SELECT id, name, color, runtime, model, args, prompt FROM agent_types ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ts := []AgentType{}
	for rows.Next() {
		var t AgentType
		if err := rows.Scan(&t.ID, &t.Name, &t.Color, &t.Runtime, &t.Model, &t.Args, &t.Prompt); err != nil {
			return nil, err
		}
		ts = append(ts, t)
	}
	return ts, rows.Err()
}

// SaveType creates the type when t.ID is empty, else updates it.
func (s *Store) SaveType(t *AgentType) error {
	if t.ID == "" {
		t.ID = newID("t")
		_, err := s.db.Exec(`INSERT INTO agent_types(id, name, color, runtime, model, args, prompt) VALUES(?,?,?,?,?,?,?)`,
			t.ID, t.Name, t.Color, t.Runtime, t.Model, t.Args, t.Prompt)
		return err
	}
	_, err := s.db.Exec(`UPDATE agent_types SET name=?, color=?, runtime=?, model=?, args=?, prompt=? WHERE id=?`,
		t.Name, t.Color, t.Runtime, t.Model, t.Args, t.Prompt, t.ID)
	return err
}

func (s *Store) DeleteType(id string) error {
	_, err := s.db.Exec(`DELETE FROM agent_types WHERE id=?`, id)
	return err
}
