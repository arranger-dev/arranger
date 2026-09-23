package store

import (
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "a.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestOpenSeedsDemoAndReopens(t *testing.T) {
	s, path := open(t)
	ps, _ := s.Projects()
	if len(ps) != 1 || ps[0].Name != "Demo" {
		t.Fatalf("projects: %+v", ps)
	}
	as, _ := s.Agents(ps[0].ID)
	if len(as) != len(DemoAgents) || as[0].Name != "Lead" || as[1].Parent != as[0].ID {
		t.Fatalf("demo agents: %+v", as)
	}
	s.Close()
	// reopening runs the migrations again (they must be harmless) and doesn't seed twice
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if ps, _ := s2.Projects(); len(ps) != 1 {
		t.Fatalf("reseeded: %d projects", len(ps))
	}
}

func TestSaveArrangement(t *testing.T) {
	s, _ := open(t)
	p, _ := s.CreateProject(Project{Name: "p"})
	x, y := 120.0, 40.0
	if err := s.SaveArrangement(p.ID, []Agent{
		{ID: "m", Name: "M", Role: "manager"},
		{ID: "c", Name: "C", Role: "coder", Parent: "m", Model: "opus", Prompt: "from a type", Color: "#ff0000", X: &x, Y: &y},
	}); err != nil {
		t.Fatal(err)
	}
	c, pid, err := s.Agent("c")
	if err != nil || pid != p.ID || c.Runtime != "claude" || c.Model != "opus" || c.Color != "#ff0000" || *c.X != 120 || *c.Y != 40 {
		t.Fatalf("new agent: %+v %s %v", c, pid, err)
	}
	if m, _, _ := s.Agent("m"); m.X != nil {
		t.Fatal("an unplaced agent should have no position")
	}

	// later saves move and reparent, but never overwrite settings chosen in the Settings tab
	c.Model, c.Hard = "haiku", 5000
	s.UpdateAgent(c)
	s.SaveGoal("m", Goal{Title: "g", Checks: "true"})
	nx := 300.0
	if err := s.SaveArrangement(p.ID, []Agent{{ID: "c", Name: "C2", Role: "coder", Model: "ignored", X: &nx, Y: &y}}); err != nil {
		t.Fatal(err)
	}
	c, _, _ = s.Agent("c")
	if c.Name != "C2" || c.Parent != "" || c.Model != "haiku" || c.Hard != 5000 || *c.X != 300 {
		t.Fatalf("after save: %+v", c)
	}
	if _, _, err := s.Agent("m"); err == nil {
		t.Fatal("removed agent should be deleted")
	}
	if g, _ := s.Goal("m"); g.Title != "" || g.Status != "idle" {
		t.Fatalf("removed agent's goal should be deleted: %+v", g)
	}

	other, _ := s.CreateProject(Project{Name: "other"})
	if err := s.SaveArrangement(other.ID, []Agent{{ID: "c", Name: "steal"}}); err == nil {
		t.Fatal("an agent must not move between projects")
	}
}

func TestGoalsEventsAndUsage(t *testing.T) {
	s, _ := open(t)
	p, _ := s.CreateProject(Project{Name: "p"})
	s.SaveArrangement(p.ID, []Agent{{ID: "a", Name: "A", Runtime: "claude"}, {ID: "b", Name: "B", Runtime: "codex"}})

	s.SetGoal("b", map[string]any{"status": "running"}) // creates the row on demand
	if g, _ := s.Goal("b"); g.Status != "running" {
		t.Fatalf("SetGoal: %+v", g)
	}
	s.SaveGoal("a", Goal{Title: "t", Checks: "true"})
	s.SetGoal("a", map[string]any{"status": "done", "attempts": 1, "adds": 3, "dels": 1})

	for i := 0; i < 5; i++ {
		s.AddEvent(&LogEvent{Agent: "a", Kind: "msg", Text: string(rune('0' + i))})
	}
	es, _ := s.Events("a", 3)
	if len(es) != 3 || es[0].Text != "2" || es[2].Text != "4" || es[0].ID == 0 {
		t.Fatalf("events should be the newest 3, oldest first: %+v", es)
	}

	old := s.NewRun("a", 1)
	s.UpdateRun(old, 0.5, 100, 10, 0)
	time.Sleep(5 * time.Millisecond)
	s.SetGoal("a", map[string]any{"since": time.Now().UnixMilli()})
	cur := s.NewRun("a", 1)
	s.UpdateRun(cur, 0.25, 40, 2, -1) // still running
	s.UpdateRun(s.NewRun("b", 1), 1, 1000, 0, 1)

	sum, _ := s.Summary(p.ID)
	a := sum.Agents["a"]
	if a.Tokens != 42 || a.AllTokens != 152 || a.Cost != 0.25 || a.AllCost != 0.75 || sum.Cost != 1.75 {
		t.Fatalf("summary: %+v total %v", a, sum.Cost)
	}

	st, _ := s.Stats(p.ID)
	if st.Goals != 1 || st.Usage.Done != 1 || st.FirstTry != 1 || st.Runs != 3 || st.Tokens != 1152 || st.Adds != 3 {
		t.Fatalf("stats: %+v", st)
	}
	if len(st.Runtimes) != 2 || st.Runtimes[0].Runtime != "codex" { // sorted by cost
		t.Fatalf("runtime stats: %+v", st.Runtimes)
	}
}

func TestTypes(t *testing.T) {
	s, _ := open(t)
	ty := AgentType{Name: "security", Color: "#d64545", Runtime: "opencode"}
	if err := s.SaveType(&ty); err != nil || ty.ID == "" {
		t.Fatal(err)
	}
	ty.Color = "#000000"
	s.SaveType(&ty)
	ts, _ := s.Types()
	if len(ts) != 1 || ts[0].Color != "#000000" {
		t.Fatalf("types: %+v", ts)
	}
	s.DeleteType(ty.ID)
	if ts, _ := s.Types(); len(ts) != 0 {
		t.Fatal("type not deleted")
	}
}
