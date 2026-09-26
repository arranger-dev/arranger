package store

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Template is a team to drop onto the canvas: a starter team, or an arrangement the user saved.
type Template struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	About   string          `json:"about"`
	Agents  []TemplateAgent `json:"agents"`
	BuiltIn bool            `json:"builtIn"`
}

// TemplateAgent is one box of a template; Key and Parent link boxes within it, X and Y are relative.
type TemplateAgent struct {
	Key     string  `json:"key"`
	Name    string  `json:"name"`
	Role    string  `json:"role"`
	Parent  string  `json:"parent"`
	Runtime string  `json:"runtime"`
	Model   string  `json:"model"`
	Args    string  `json:"args"`
	Prompt  string  `json:"prompt"`
	Color   string  `json:"color"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
}

// StarterTeams are the teams a new project can start from.
var StarterTeams = []Template{
	{ID: "starter-feature", Name: "Feature team", About: "A manager with a product manager, two programmers and a tester.", BuiltIn: true, Agents: []TemplateAgent{
		{Key: "lead", Name: "Lead", Role: "manager", X: 360, Y: 0},
		{Key: "pm", Name: "Product Manager", Role: "product-manager", Parent: "lead", X: 0, Y: 150},
		{Key: "be", Name: "Backend", Role: "programmer", Parent: "lead", X: 240, Y: 150},
		{Key: "fe", Name: "Frontend", Role: "programmer", Parent: "lead", X: 480, Y: 150},
		{Key: "qa", Name: "Tester", Role: "tester", Parent: "lead", X: 720, Y: 150},
	}},
	{ID: "starter-bugfix", Name: "Bug fix", About: "A manager with a programmer who fixes it and a tester who proves it.", BuiltIn: true, Agents: []TemplateAgent{
		{Key: "lead", Name: "Lead", Role: "manager", X: 120, Y: 0},
		{Key: "fix", Name: "Fixer", Role: "programmer", Parent: "lead", X: 0, Y: 150},
		{Key: "qa", Name: "Tester", Role: "tester", Parent: "lead", X: 240, Y: 150},
	}},
	{ID: "starter-docs", Name: "Docs", About: "A technical content writer on its own.", BuiltIn: true, Agents: []TemplateAgent{
		{Key: "doc", Name: "Technical Content Writer", Role: "writer", X: 0, Y: 0},
	}},
}

// Templates are the starter teams, then the user's saved ones.
func (s *Store) Templates() ([]Template, error) {
	ts := append([]Template{}, StarterTeams...)
	for i := range ts {
		for j := range ts[i].Agents {
			a := &ts[i].Agents[j]
			if a.Prompt == "" {
				a.Prompt = RolePrompts[a.Role]
			}
		}
	}
	rows, err := s.db.Query(`SELECT id, name, about, json FROM templates ORDER BY created`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t Template
		var raw string
		if err := rows.Scan(&t.ID, &t.Name, &t.About, &raw); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(raw), &t.Agents)
		ts = append(ts, t)
	}
	return ts, rows.Err()
}

// SaveTemplate stores the user's arrangement as a new template.
func (s *Store) SaveTemplate(t *Template) error {
	t.Name = strings.TrimSpace(t.Name)
	if t.Name == "" {
		return errors.New("give the template a name")
	}
	if len(t.Agents) == 0 {
		return errors.New("the canvas is empty; add agents first")
	}
	b, err := json.Marshal(t.Agents)
	if err != nil {
		return err
	}
	t.ID, t.BuiltIn = newID("tpl"), false
	_, err = s.db.Exec(`INSERT INTO templates(id, name, about, json, created) VALUES(?,?,?,?,?)`, t.ID, t.Name, t.About, string(b), time.Now().UnixMilli())
	return err
}

// DeleteTemplate removes a saved template; starter teams stay.
func (s *Store) DeleteTemplate(id string) error {
	if strings.HasPrefix(id, "starter-") {
		return errors.New("starter teams can't be deleted")
	}
	_, err := s.db.Exec(`DELETE FROM templates WHERE id=?`, id)
	return err
}
