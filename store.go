package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

var db *sql.DB

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
-- a crash mid-run leaves stale statuses behind
UPDATE goals SET status = 'stopped' WHERE status IN ('running', 'verifying');
`

type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Repo string `json:"repo"`
	Base string `json:"base"`
}

type Agent struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Role    string `json:"role"`
	Parent  string `json:"parent"`
	Runtime string `json:"runtime"`
	Model   string `json:"model"`
	Prompt  string `json:"prompt"`
	Args    string `json:"args"`
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
}

var demoAgents = []Agent{
	{ID: "lead", Name: "Lead", Role: "manager"},
	{ID: "be", Name: "Backend", Role: "manager", Parent: "lead"},
	{ID: "fe", Name: "Frontend", Role: "coder", Parent: "lead"},
	{ID: "rev", Name: "Reviewer", Role: "reviewer", Parent: "lead"},
	{ID: "api", Name: "API Coder", Role: "coder", Parent: "be"},
	{ID: "db", Name: "DB Coder", Role: "coder", Parent: "be"},
	{ID: "test", Name: "Tester", Role: "tester", Parent: "api"},
}

func openDB(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var err error
	db, err = sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1) // ponytail: one writer connection, enough for a local tool
	if _, err = db.Exec(schema); err != nil {
		return err
	}
	var n int
	db.QueryRow(`SELECT count(*) FROM projects`).Scan(&n)
	if n > 0 {
		return nil
	}
	// First start: a demo project so the arrange screen isn't empty.
	p, err := createProject(Project{Name: "Demo"})
	if err != nil {
		return err
	}
	as := make([]Agent, len(demoAgents))
	for i, a := range demoAgents {
		a.ID = p.ID + "-" + a.ID
		if a.Parent != "" {
			a.Parent = p.ID + "-" + a.Parent
		}
		as[i] = a
	}
	return saveArrangement(p.ID, as)
}

func newID(prefix string) string {
	return fmt.Sprintf("%s%x", prefix, time.Now().UnixNano())
}

func createProject(p Project) (Project, error) {
	p.ID = newID("p")
	_, err := db.Exec(`INSERT INTO projects(id, name, repo, base) VALUES(?,?,?,?)`, p.ID, p.Name, p.Repo, p.Base)
	return p, err
}

func updateProject(p Project) error {
	_, err := db.Exec(`UPDATE projects SET name=?, repo=?, base=? WHERE id=?`, p.Name, p.Repo, p.Base, p.ID)
	return err
}

func getProject(id string) (p Project, err error) {
	err = db.QueryRow(`SELECT id, name, repo, base FROM projects WHERE id=?`, id).Scan(&p.ID, &p.Name, &p.Repo, &p.Base)
	return
}

func listProjects() ([]Project, error) {
	rows, err := db.Query(`SELECT id, name, repo, base FROM projects ORDER BY rowid`)
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

const agentCols = `id, name, role, parent, runtime, model, prompt, args`

func scanAgent(s interface{ Scan(...any) error }) (a Agent, err error) {
	err = s.Scan(&a.ID, &a.Name, &a.Role, &a.Parent, &a.Runtime, &a.Model, &a.Prompt, &a.Args)
	return
}

func listAgents(projectID string) ([]Agent, error) {
	rows, err := db.Query(`SELECT `+agentCols+` FROM agents WHERE project_id=? ORDER BY pos`, projectID)
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

// getAgent returns the agent and its project id.
func getAgent(id string) (a Agent, pid string, err error) {
	err = db.QueryRow(`SELECT `+agentCols+`, project_id FROM agents WHERE id=?`, id).
		Scan(&a.ID, &a.Name, &a.Role, &a.Parent, &a.Runtime, &a.Model, &a.Prompt, &a.Args, &pid)
	return
}

func updateAgent(a Agent) error {
	_, err := db.Exec(`UPDATE agents SET name=?, runtime=?, model=?, prompt=?, args=? WHERE id=?`,
		a.Name, a.Runtime, a.Model, a.Prompt, a.Args, a.ID)
	return err
}

// saveArrangement makes the project's tree match as: upserts name/role/parent/order,
// keeps each agent's settings, and deletes agents (and goals) that are gone.
func saveArrangement(projectID string, as []Agent) error {
	tx, err := db.Begin()
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
		if _, err := tx.Exec(`INSERT INTO agents(id, project_id, name, role, parent, pos, runtime) VALUES(?,?,?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET name=excluded.name, role=excluded.role, parent=excluded.parent, pos=excluded.pos`,
			a.ID, projectID, a.Name, a.Role, a.Parent, i, rt); err != nil {
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

func getGoal(agentID string) (g Goal, err error) {
	err = db.QueryRow(`SELECT title, body, criteria, checks, status, attempts, feedback, passed, total, adds, dels FROM goals WHERE agent_id=?`, agentID).
		Scan(&g.Title, &g.Body, &g.Criteria, &g.Checks, &g.Status, &g.Attempts, &g.Feedback, &g.Passed, &g.Total, &g.Adds, &g.Dels)
	if err == sql.ErrNoRows {
		return Goal{Status: "idle"}, nil
	}
	return
}

func saveGoal(agentID string, g Goal) error {
	_, err := db.Exec(`INSERT INTO goals(agent_id, title, body, criteria, checks) VALUES(?,?,?,?,?)
		ON CONFLICT(agent_id) DO UPDATE SET title=excluded.title, body=excluded.body, criteria=excluded.criteria, checks=excluded.checks`,
		agentID, g.Title, g.Body, g.Criteria, g.Checks)
	return err
}

// setGoal updates run-time fields of a goal; col must be a trusted column name.
func setGoal(agentID string, kv map[string]any) {
	for col, v := range kv {
		if _, err := db.Exec(`UPDATE goals SET `+col+`=? WHERE agent_id=?`, v, agentID); err != nil {
			log.Printf("setGoal %s: %v", col, err)
		}
	}
}

type LogEvent struct {
	ID    int64  `json:"id"`
	Agent string `json:"agent"`
	Run   int64  `json:"run"`
	TS    int64  `json:"ts"`
	Kind  string `json:"kind"`
	Text  string `json:"text"`
	Raw   string `json:"raw,omitempty"`
}

func addEvent(e *LogEvent) {
	e.TS = time.Now().UnixMilli()
	res, err := db.Exec(`INSERT INTO events(run_id, agent_id, ts, kind, text, raw) VALUES(?,?,?,?,?,?)`, e.Run, e.Agent, e.TS, e.Kind, e.Text, e.Raw)
	if err != nil {
		log.Printf("addEvent: %v", err)
		return
	}
	e.ID, _ = res.LastInsertId()
}

// listEvents returns the last `limit` events for an agent, oldest first.
func listEvents(agentID string, limit int) ([]LogEvent, error) {
	rows, err := db.Query(`SELECT * FROM (SELECT id, run_id, ts, kind, text, raw FROM events WHERE agent_id=? ORDER BY id DESC LIMIT ?) ORDER BY id`, agentID, limit)
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

type Summary struct {
	Agents map[string]AgentStatus `json:"agents"`
	Cost   float64                `json:"cost"`
}

type AgentStatus struct {
	Goal
	Now string `json:"now"`
}

func projectSummary(projectID string) (Summary, error) {
	s := Summary{Agents: map[string]AgentStatus{}}
	rows, err := db.Query(`SELECT a.id, coalesce(g.title,''), coalesce(g.status,'idle'), coalesce(g.attempts,0),
		coalesce(g.passed,0), coalesce(g.total,0), coalesce(g.adds,0), coalesce(g.dels,0)
		FROM agents a LEFT JOIN goals g ON g.agent_id = a.id WHERE a.project_id=?`, projectID)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var st AgentStatus
		if err := rows.Scan(&id, &st.Title, &st.Status, &st.Attempts, &st.Passed, &st.Total, &st.Adds, &st.Dels); err != nil {
			return s, err
		}
		st.Now = hub.now(id)
		s.Agents[id] = st
	}
	db.QueryRow(`SELECT coalesce(sum(r.cost),0) FROM runs r JOIN agents a ON a.id = r.agent_id WHERE a.project_id=?`, projectID).Scan(&s.Cost)
	return s, rows.Err()
}
