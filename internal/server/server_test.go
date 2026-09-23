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

	css := a.must(200, "GET", "/fonts.css", "")
	if !strings.Contains(css, "Plex Sans") || !strings.Contains(page, "/fonts.css") || !strings.Contains(landing, "/fonts.css") {
		t.Fatal("both pages should load the embedded fonts")
	}
	r = httptest.NewRequest("GET", "http://localhost:7777/fonts/lilex-latin-400-normal.woff2", nil)
	w = httptest.NewRecorder()
	a.h.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.Len() < 1000 {
		t.Fatalf("font file: %d, %d bytes", w.Code, w.Body.Len())
	}
	if strings.Contains(landing, `href="#why"`) || strings.Index(landing, `id="theme"`) > strings.Index(landing, `rel="noopener">GitHub</a>`) {
		t.Fatal("landing navbar: no Why/How links, theme toggle before GitHub")
	}
}

func TestProjectOnRepoWithoutCommits(t *testing.T) {
	a := newApp(t)
	repo := t.TempDir()
	exec.Command("git", "-C", repo, "init", "-q").Run()
	code, body := a.do("POST", "/api/projects", `{"name":"x","repo":"`+repo+`"}`)
	if code != 400 || !strings.Contains(body, "no commits yet") {
		t.Fatalf("%d %s", code, body)
	}
}

// Pressing Run must show the agent working at once: the 202 comes after the "starting"
// status went out on the live stream, through the logging middleware's flushes.
func TestRunShowsWorkingAtOnce(t *testing.T) {
	a := newApp(t)
	repo := newRepo(t)
	var p store.Project
	json.Unmarshal([]byte(a.must(200, "POST", "/api/projects", `{"name":"r","repo":"`+repo+`"}`)), &p)
	a.must(204, "POST", "/api/projects/"+p.ID+"/arrangement", `[{"id":"k","name":"Kid","role":"coder"}]`)
	a.must(204, "PUT", "/api/agents/k", `{"name":"Kid","runtime":"generic","args":"sleep 1"}`)
	a.must(204, "PUT", "/api/agents/k/goal", `{"title":"wait","checks":"true"}`)

	srv := httptest.NewServer(a.h)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/api/events?project="+p.ID, nil)
	req.Host = "localhost"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	lines := make(chan string, 64)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := res.Body.Read(buf)
			for _, l := range strings.Split(string(buf[:n]), "\n") {
				if l != "" {
					lines <- l
				}
			}
			if err != nil {
				close(lines)
				return
			}
		}
	}()
	if l := <-lines; !strings.Contains(l, "hello") {
		t.Fatalf("first message: %s", l)
	}

	a.must(202, "POST", "/api/agents/k/run", "")
	if g, _ := a.st.Goal("k"); g.Status == "idle" {
		t.Fatal("status should already be busy when Run returns")
	}
	a.must(409, "POST", "/api/projects/"+p.ID+"/arrangement", `[]`) // can't delete a running agent
	if _, body := a.do("POST", "/api/agents/nope/run", ""); !strings.Contains(body, "not found") {
		t.Fatalf("unknown agent: %s", body)
	}
	select {
	case l := <-lines:
		if !strings.Contains(l, `"status":"starting"`) {
			t.Fatalf("first live update: %s", l)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no live update after Run")
	}
	for deadline := time.Now().Add(20 * time.Second); a.o.IsRunning("k"); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("run never finished")
		}
	}
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

// Promote copies a report's changes into its manager's work: skipping what the manager already
// has (e.g. after it merged the report), and refusing, with the manager untouched, on a conflict.
func TestPromoteToManager(t *testing.T) {
	a := newApp(t)
	repo := newRepo(t)
	os.WriteFile(filepath.Join(repo, "make.sh"), []byte("printf 'one\\n2\\n' > a.txt\necho new > b.txt\n"), 0o644)
	git.Commit(repo, "add make.sh")
	var p store.Project
	json.Unmarshal([]byte(a.must(200, "POST", "/api/projects", `{"name":"r","repo":"`+repo+`"}`)), &p)
	a.must(204, "POST", "/api/projects/"+p.ID+"/arrangement", `[{"id":"lead","name":"Lead","role":"manager"},{"id":"k","name":"Kid","role":"coder","parent":"lead"}]`)
	a.must(204, "PUT", "/api/agents/k", `{"name":"Kid","runtime":"generic","args":"sh make.sh"}`)
	a.must(204, "PUT", "/api/agents/k/goal", `{"title":"write files","checks":"test -f b.txt"}`)
	a.must(202, "POST", "/api/agents/k/run", "")
	for a.o.IsRunning("k") {
		time.Sleep(50 * time.Millisecond)
	}
	var d struct{ Files []git.FileDiff }
	hunks := func() map[string]string { // path -> quoted hunk id
		json.Unmarshal([]byte(a.must(200, "GET", "/api/agents/k/diff", "")), &d)
		m := map[string]string{}
		for _, f := range d.Files {
			m[f.Path] = `"` + f.Hunks[0].ID + `"`
		}
		return m
	}
	var res struct{ Applied, Present int }
	promote := func(ids ...string) (int, string) {
		code, out := a.do("POST", "/api/agents/k/hunks", `{"action":"promote","ids":[`+strings.Join(ids, ",")+`]}`)
		res.Applied, res.Present = 0, 0
		json.Unmarshal([]byte(out), &res)
		return code, out
	}

	h := hunks()
	if code, out := promote(h["b.txt"]); code != 200 || res.Applied != 1 {
		t.Fatalf("first promote: %d %s", code, out)
	}
	if code, out := promote(h["a.txt"], h["b.txt"]); code != 200 || res.Applied != 1 || res.Present != 1 {
		t.Fatalf("b.txt is already in Lead, a.txt isn't: %d %s", code, out)
	}
	if code, out := promote(h["a.txt"], h["b.txt"]); code != 200 || res.Applied != 0 || res.Present != 2 {
		t.Fatalf("promoting what Lead has: %d %s", code, out)
	}
	json.Unmarshal([]byte(a.must(200, "GET", "/api/agents/lead/diff", "")), &d)
	if len(d.Files) != 2 {
		t.Fatalf("lead diff: %+v", d.Files)
	}

	// Lead rewrites the line the kid changes: a conflict, named by file, with Lead left clean
	lead := a.o.Dir("lead")
	os.WriteFile(filepath.Join(lead, "a.txt"), []byte("one\ndeux\n"), 0o644)
	git.Commit(lead, "lead edits a.txt")
	os.WriteFile(filepath.Join(a.o.Dir("k"), "a.txt"), []byte("one\nzwei\n"), 0o644)
	git.Commit(a.o.Dir("k"), "kid edits a.txt again")
	if code, out := promote(hunks()["a.txt"]); code != 409 || !strings.Contains(out, "a.txt") {
		t.Fatalf("conflicting promote: %d %s", code, out)
	}
	if st, _ := git.Run(lead, "status", "--porcelain"); strings.TrimSpace(st) != "" {
		t.Fatalf("lead's worktree should be untouched after a failed promote:\n%s", st)
	}
}

// Commits on the base branch after an agent ran reach it: the Diff tab says how far behind it
// is, Sync (or the next run) merges them in, and they don't show up as the agent's changes.
func TestSyncWithBaseBranch(t *testing.T) {
	a := newApp(t)
	repo := newRepo(t)
	var p store.Project
	json.Unmarshal([]byte(a.must(200, "POST", "/api/projects", `{"name":"r","repo":"`+repo+`"}`)), &p)
	a.must(204, "POST", "/api/projects/"+p.ID+"/arrangement", `[{"id":"k","name":"Kid","role":"coder"}]`)
	os.WriteFile(filepath.Join(repo, "kid.sh"), []byte("echo x >> kid.txt\n"), 0o644)
	git.Commit(repo, "add kid.sh")
	a.must(204, "PUT", "/api/agents/k", `{"name":"Kid","runtime":"generic","args":"sh kid.sh"}`)
	a.must(204, "PUT", "/api/agents/k/goal", `{"title":"t","checks":"test -f kid.txt"}`)
	run := func() {
		a.must(202, "POST", "/api/agents/k/run", "")
		for a.o.IsRunning("k") {
			time.Sleep(50 * time.Millisecond)
		}
	}
	run()
	a.must(404, "POST", "/api/agents/nope/sync", "")
	os.WriteFile(filepath.Join(repo, "user.txt"), []byte("new\n"), 0o644)
	git.Commit(repo, "user work on main")

	var d struct {
		Files  []git.FileDiff
		From   string
		Behind int
	}
	json.Unmarshal([]byte(a.must(200, "GET", "/api/agents/k/diff", "")), &d)
	if d.From != "main" || d.Behind != 1 || len(d.Files) != 1 {
		t.Fatalf("before sync: from %q behind %d", d.From, d.Behind)
	}
	a.must(200, "POST", "/api/agents/k/sync", "")
	json.Unmarshal([]byte(a.must(200, "GET", "/api/agents/k/diff", "")), &d)
	if d.Behind != 0 || len(d.Files) != 1 || d.Files[0].Path != "kid.txt" {
		t.Fatalf("after sync: behind %d, files %+v", d.Behind, d.Files)
	}

	// the next run syncs by itself
	os.WriteFile(filepath.Join(repo, "user2.txt"), []byte("more\n"), 0o644)
	git.Commit(repo, "more user work")
	a.must(204, "PUT", "/api/agents/k/goal", `{"title":"t","checks":"test -f user2.txt"}`) // only passes on synced code
	run()
	if g, _ := a.st.Goal("k"); g.Status != "done" {
		t.Fatalf("run on synced code: %+v", g)
	}
	json.Unmarshal([]byte(a.must(200, "GET", "/api/agents/k/diff", "")), &d)
	for _, f := range d.Files {
		if strings.HasPrefix(f.Path, "user") {
			t.Fatalf("upstream file %s shows as the agent's change", f.Path)
		}
	}
}
