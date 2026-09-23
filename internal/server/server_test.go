package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"arranger/internal/git"
	"arranger/internal/orch"
	"arranger/internal/store"
)

func TestValidate(t *testing.T) {
	if err := validate(store.DemoAgents); err != nil {
		t.Fatal("demo data:", err)
	}
	bad := map[string][]store.Agent{
		"cycle":    {{ID: "a", Name: "A", Parent: "b"}, {ID: "b", Name: "B", Parent: "a"}},
		"self":     {{ID: "a", Name: "A", Parent: "a"}},
		"unknown":  {{ID: "a", Name: "A", Parent: "zz"}},
		"dup":      {{ID: "a", Name: "A"}, {ID: "a", Name: "A"}},
		"empty":    {{ID: "", Name: "A"}},
		"noname":   {{ID: "a", Name: " "}},
		"traverse": {{ID: "../x", Name: "A"}},
		"color":    {{ID: "a", Name: "A", Color: "red; x"}},
	}
	for name, as := range bad {
		if validate(as) == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

// app is a server on a fresh database, driven through its real HTTP handler.
type app struct {
	t   *testing.T
	st  *store.Store
	o   *orch.Orchestrator
	h   http.Handler
	pid string // the seeded demo project
}

func newApp(t *testing.T) *app {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	o := orch.New(st, filepath.Join(dir, "worktrees"), 2)
	ps, _ := st.Projects()
	return &app{t: t, st: st, o: o, h: New(st, o, true), pid: ps[0].ID}
}

// do sends a same-origin request and returns the status and body.
func (a *app) do(method, path, body string) (int, string) {
	a.t.Helper()
	r := httptest.NewRequest(method, "http://localhost:7777"+path, strings.NewReader(body))
	if method != http.MethodGet {
		r.Header.Set("Origin", "http://localhost:7777")
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, r)
	b, _ := io.ReadAll(w.Result().Body)
	return w.Code, string(b)
}

func (a *app) must(code int, method, path, body string) string {
	a.t.Helper()
	got, out := a.do(method, path, body)
	if got != code {
		a.t.Fatalf("%s %s: got %d, want %d: %s", method, path, got, code, out)
	}
	return out
}

func TestGuard(t *testing.T) {
	a := newApp(t)
	r := httptest.NewRequest("GET", "http://evil.example/arrange", nil)
	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("foreign host (DNS rebinding) got %d", w.Code)
	}
	r = httptest.NewRequest("POST", "http://localhost:7777/api/projects/"+a.pid+"/run", nil)
	r.Header.Set("Origin", "https://evil.example")
	w = httptest.NewRecorder()
	a.h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-site POST got %d", w.Code)
	}
}

func TestPages(t *testing.T) {
	a := newApp(t)
	landing := a.must(200, "GET", "/", "")
	if !strings.Contains(landing, GitHubURL) || !strings.Contains(landing, "/arranger.png") {
		t.Fatal("landing page should link GitHub and show the logo")
	}
	page := a.must(200, "GET", "/arrange?p="+a.pid, "")
	if !strings.Contains(page, `rel="icon"`) || !strings.Contains(page, "API Coder") {
		t.Fatal("arrange page should carry the favicon and the demo agents")
	}
	r := httptest.NewRequest("GET", "http://localhost:7777/arranger.png", nil)
	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("logo: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	a.must(404, "GET", "/nope", "")
}

func TestProjectsArrangementAndSettings(t *testing.T) {
	a := newApp(t)
	a.must(400, "POST", "/api/projects", `{"name":"x","repo":"`+t.TempDir()+`"}`) // not a git repo
	var p store.Project
	json.Unmarshal([]byte(a.must(200, "POST", "/api/projects", `{"name":" Real ","repo":"`+newRepo(t)+`"}`)), &p)
	if p.Name != "Real" || p.Base != "main" {
		t.Fatalf("project: %+v", p)
	}

	a.must(400, "POST", "/api/projects/"+p.ID+"/arrangement", `[{"id":"a","name":"A","parent":"a"}]`)
	a.must(204, "POST", "/api/projects/"+p.ID+"/arrangement", `[{"id":"m","name":"M","role":"manager","x":40,"y":30},{"id":"w","name":"W","role":"coder","parent":"m"}]`)
	if m, _, _ := a.st.Agent("m"); m.X == nil || *m.X != 40 {
		t.Fatalf("position not saved: %+v", m)
	}

	for body, why := range map[string]string{
		`{"name":"W","runtime":"nope"}`:                                "unknown runtime",
		`{"name":" ","runtime":"claude"}`:                              "no name",
		`{"name":"W","runtime":"claude","tokenSoft":10,"tokenHard":5}`: "soft above hard",
		`{"name":"W","runtime":"claude","color":"blue"}`:               "bad color",
	} {
		if code, _ := a.do("PUT", "/api/agents/w", body); code != 400 {
			t.Errorf("%s: got %d", why, code)
		}
	}
	a.must(204, "PUT", "/api/agents/w", `{"name":"W","runtime":"codex","color":"#ff5a1f","tokenHard":900}`)
	var got struct{ Agent store.Agent }
	json.Unmarshal([]byte(a.must(200, "GET", "/api/agents/w", "")), &got)
	if got.Agent.Color != "#ff5a1f" || got.Agent.Runtime != "codex" || got.Agent.Hard != 900 {
		t.Fatalf("settings: %+v", got.Agent)
	}
	if out := a.must(400, "POST", "/api/agents/w/run", ""); !strings.Contains(out, "no goal") {
		t.Fatalf("run without a goal: %s", out)
	}
	a.must(404, "GET", "/api/agents/ghost", "")
}

func TestTypesAPI(t *testing.T) {
	a := newApp(t)
	a.must(400, "POST", "/api/types", `{"name":"bad name","color":"#000000","runtime":"claude"}`)
	a.must(400, "POST", "/api/types", `{"name":"sec","color":"red","runtime":"claude"}`)
	var ty store.AgentType
	json.Unmarshal([]byte(a.must(200, "POST", "/api/types", `{"name":"Sec","color":"#d64545","runtime":"opencode"}`)), &ty)
	if ty.ID == "" || ty.Name != "sec" {
		t.Fatalf("type: %+v", ty)
	}
	a.must(200, "PUT", "/api/types/"+ty.ID, `{"name":"sec","color":"#000000","runtime":"opencode"}`)
	a.must(204, "DELETE", "/api/types/"+ty.ID, "")
	if ts, _ := a.st.Types(); len(ts) != 0 {
		t.Fatal("type not deleted")
	}
}

// A full run with a shell-script agent (the generic runtime, no model needed), then the
// user-facing operations on its work: diff, remove, revert, stats and merge.
func TestRunThenWorkWithTheChanges(t *testing.T) {
	a := newApp(t)
	repo := newRepo(t)
	os.WriteFile(filepath.Join(repo, "make.sh"), []byte("printf 'one\\n2\\n' > a.txt\necho new > b.txt\n"), 0o644)
	git.Commit(repo, "add make.sh")
	var p store.Project
	json.Unmarshal([]byte(a.must(200, "POST", "/api/projects", `{"name":"r","repo":"`+repo+`"}`)), &p)
	a.must(204, "POST", "/api/projects/"+p.ID+"/arrangement", `[{"id":"k","name":"Kid","role":"coder"}]`)
	a.must(204, "PUT", "/api/agents/k", `{"name":"Kid","runtime":"generic","args":"sh make.sh"}`)
	a.must(204, "PUT", "/api/agents/k/goal", `{"title":"write files","checks":"test -f b.txt"}`)
	a.must(202, "POST", "/api/agents/k/run", "")
	for deadline := time.Now().Add(20 * time.Second); a.o.IsRunning("k"); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("run never finished")
		}
	}
	if g, _ := a.st.Goal("k"); g.Status != "done" {
		t.Fatalf("goal: %+v", g)
	}

	var d struct{ Files []git.FileDiff }
	json.Unmarshal([]byte(a.must(200, "GET", "/api/agents/k/diff", "")), &d)
	if len(d.Files) != 2 {
		t.Fatalf("diff: %+v", d.Files)
	}
	a.must(409, "POST", "/api/agents/k/hunks", `{"action":"remove","ids":["stale"]}`)
	a.must(400, "POST", "/api/agents/k/hunks", `{"action":"promote","ids":["`+d.Files[1].Hunks[0].ID+`"]}`) // no manager
	a.must(204, "POST", "/api/agents/k/hunks", `{"action":"remove","ids":["`+d.Files[1].Hunks[0].ID+`"]}`)
	json.Unmarshal([]byte(a.must(200, "GET", "/api/agents/k/diff", "")), &d)
	if len(d.Files) != 1 || d.Files[0].Path != "a.txt" {
		t.Fatalf("after remove: %+v", d.Files)
	}
	if g, _ := a.st.Goal("k"); !strings.Contains(g.Notes, "b.txt") {
		t.Fatal("the agent should be told not to re-add the removed change")
	}

	var cps []git.Checkpoint
	json.Unmarshal([]byte(a.must(200, "GET", "/api/agents/k/checkpoints", "")), &cps)
	a.must(400, "POST", "/api/agents/k/revert", `{"sha":"; rm -rf /"}`)
	a.must(204, "POST", "/api/agents/k/revert", `{"sha":"`+cps[0].SHA+`"}`)
	json.Unmarshal([]byte(a.must(200, "GET", "/api/agents/k/diff", "")), &d)
	if len(d.Files) != 2 {
		t.Fatalf("revert should bring b.txt back: %+v", d.Files)
	}

	var st store.Stats
	json.Unmarshal([]byte(a.must(200, "GET", "/api/projects/"+p.ID+"/stats", "")), &st)
	if st.Goals != 1 || st.Done != 1 || st.FirstTry != 1 || st.Runs != 1 {
		t.Fatalf("stats: %+v", st)
	}

	a.must(400, "GET", "/api/agents/k/merge?target=arranger/x", "")
	var m git.MergePreview
	json.Unmarshal([]byte(a.must(200, "GET", "/api/agents/k/merge?target=feature", "")), &m)
	if m.Exists || m.Files != 2 {
		t.Fatalf("preview: %+v", m)
	}
	a.must(200, "POST", "/api/agents/k/merge", `{"target":"feature","strategy":"squash"}`)
	if out, _ := git.Run(repo, "log", "-1", "--format=%s", "feature"); strings.TrimSpace(out) != "write files" {
		t.Fatalf("squash commit message should default to the goal: %q", out)
	}
}

// newRepo makes a temp git repo on branch main with one committed file.
func newRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\ntwo\n"), 0o644)
	if _, err := git.Commit(repo, "a"); err != nil {
		t.Fatal(err)
	}
	return repo
}
