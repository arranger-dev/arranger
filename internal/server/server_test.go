package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"arranger/internal/git"
	"arranger/internal/orch"
	"arranger/internal/store"
	"arranger/internal/version"
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
	// the landing page lives in its own repo now; / goes straight to the app
	r := httptest.NewRequest("GET", "http://localhost:7777/", nil)
	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, r)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/arrange" {
		t.Fatalf("/: %d → %q", w.Code, w.Header().Get("Location"))
	}
	page := a.must(200, "GET", "/arrange?p="+a.pid, "")
	if !strings.Contains(page, `rel="icon"`) || !strings.Contains(page, "API Programmer") {
		t.Fatal("arrange page should carry the favicon and the demo agents")
	}
	if v := version.Get(); !strings.Contains(page, `id="version"`) || !strings.Contains(page, v.Version) {
		t.Fatalf("arrange page should show the version %q", v.Version)
	}
	r = httptest.NewRequest("GET", "http://localhost:7777/arranger.png", nil)
	w = httptest.NewRecorder()
	a.h.ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("logo: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	a.must(404, "GET", "/nope", "")
	a.must(404, "GET", "/landing.html", "")

	var v version.Info
	if err := json.Unmarshal([]byte(a.must(200, "GET", "/api/version", "")), &v); err != nil || v.Version == "" || v.Go == "" {
		t.Fatalf("version api: %+v %v", v, err)
	}

	css := a.must(200, "GET", "/fonts.css", "")
	if !strings.Contains(css, "Plex Sans") || !strings.Contains(page, "/fonts.css") {
		t.Fatal("the page should load the embedded fonts")
	}
	r = httptest.NewRequest("GET", "http://localhost:7777/fonts/lilex-latin-400-normal.woff2", nil)
	w = httptest.NewRecorder()
	a.h.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.Len() < 1000 {
		t.Fatalf("font file: %d, %d bytes", w.Code, w.Body.Len())
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
	a.must(400, "POST", "/api/types", `{"name":"  ","color":"#000000","runtime":"claude"}`)
	a.must(400, "POST", "/api/types", `{"name":"`+strings.Repeat("x", 41)+`","color":"#000000","runtime":"claude"}`)
	a.must(400, "POST", "/api/types", `{"name":"sec","color":"red","runtime":"claude"}`)
	var ty store.AgentType
	// names keep their spaces and capitals
	json.Unmarshal([]byte(a.must(200, "POST", "/api/types", `{"name":"  Frontend   Guy ","color":"#d64545","runtime":"opencode"}`)), &ty)
	if ty.ID == "" || ty.Name != "Frontend Guy" {
		t.Fatalf("type: %+v", ty)
	}
	a.must(400, "POST", "/api/types", `{"name":"frontend guy","color":"#000000","runtime":"claude"}`)         // taken, ignoring case
	a.must(200, "PUT", "/api/types/"+ty.ID, `{"name":"Frontend guy","color":"#000000","runtime":"opencode"}`) // renaming itself is fine
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
	// the message is the change itself; the hunk removed and brought back by hand isn't a change
	if out, _ := git.Run(repo, "log", "-1", "--format=%B", "feature"); strings.TrimSpace(out) != "Write files" {
		t.Fatalf("squash commit message should list the change: %q", out)
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

// Request changes re-runs an agent with the user's change; an empty request is refused.
func TestRequestChangesAPI(t *testing.T) {
	a := newApp(t)
	repo := newRepo(t)
	os.WriteFile(filepath.Join(repo, "say.sh"), []byte("grep -q CAPS && echo LOUD > out.txt || echo quiet > out.txt\n"), 0o644)
	git.Commit(repo, "add say.sh")
	var p store.Project
	json.Unmarshal([]byte(a.must(200, "POST", "/api/projects", `{"name":"r","repo":"`+repo+`"}`)), &p)
	a.must(204, "POST", "/api/projects/"+p.ID+"/arrangement", `[{"id":"k","name":"Kid","role":"coder"}]`)
	a.must(204, "PUT", "/api/agents/k", `{"name":"Kid","runtime":"generic","args":"sh say.sh"}`) // reads the prompt on stdin
	a.must(204, "PUT", "/api/agents/k/goal", `{"title":"write out.txt","checks":"test -f out.txt"}`)
	wait := func() {
		for a.o.IsRunning("k") {
			time.Sleep(50 * time.Millisecond)
		}
	}
	a.must(202, "POST", "/api/agents/k/run", "")
	wait()
	a.must(400, "POST", "/api/agents/k/revise", `{"change":"  "}`)
	a.must(202, "POST", "/api/agents/k/revise", `{"change":"say it in CAPS"}`)
	wait()
	if out, _ := os.ReadFile(filepath.Join(a.o.Dir("k"), "out.txt")); strings.TrimSpace(string(out)) != "LOUD" {
		t.Fatalf("the agent should get the change in its prompt: out.txt = %q", out)
	}
	if g, _ := a.st.Goal("k"); g.Status != "done" || g.Title != "write out.txt" {
		t.Fatalf("goal kept, run done: %+v", g)
	}
}

// A manager that asks for approval waits with its plan; the page reads it, can't move its
// reports meanwhile, and approves it through the API.
func TestPlanApprovalAPI(t *testing.T) {
	a := newApp(t)
	repo := newRepo(t)
	os.WriteFile(filepath.Join(repo, "plan.sh"), []byte(`cat >/dev/null; echo '{"subgoals":[{"agent":"k","title":"do it","checks":["true"]}]}'`+"\n"), 0o644)
	git.Commit(repo, "add plan.sh")
	var p store.Project
	json.Unmarshal([]byte(a.must(200, "POST", "/api/projects", `{"name":"r","repo":"`+repo+`"}`)), &p)
	arrangement := `[{"id":"m","name":"Lead","role":"manager","approvePlan":true},{"id":"k","name":"Kid","role":"coder","parent":"m"}]`
	a.must(204, "POST", "/api/projects/"+p.ID+"/arrangement", arrangement)
	if m, _, _ := a.st.Agent("m"); !m.ApprovePlan {
		t.Fatal("a new agent should take approvePlan from the arrangement")
	}
	a.must(204, "PUT", "/api/agents/m", `{"name":"Lead","runtime":"generic","args":"sh plan.sh","approvePlan":true}`)
	a.must(204, "PUT", "/api/agents/k", `{"name":"Kid","runtime":"generic","args":"true"}`)
	a.must(204, "PUT", "/api/agents/m/goal", `{"title":"lead","checks":"true"}`)
	a.must(404, "GET", "/api/agents/m/plan", "")

	a.must(202, "POST", "/api/agents/m/run", "")
	var draft store.Plan
	for deadline := time.Now().Add(20 * time.Second); draft.ID == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no plan to approve")
		}
		if code, body := a.do("GET", "/api/agents/m/plan", ""); code == 200 {
			json.Unmarshal([]byte(body), &draft)
		}
	}
	if draft.Kind != "plan" || !strings.Contains(string(draft.JSON), "do it") {
		t.Fatalf("draft: %+v", draft)
	}
	a.must(409, "POST", "/api/projects/"+p.ID+"/arrangement", `[{"id":"m","name":"Lead","role":"manager"},{"id":"k","name":"Kid","role":"coder"}]`)
	id := strconv.FormatInt(draft.ID, 10)
	a.must(409, "POST", "/api/agents/m/plan", `{"draft":`+id+`1,"action":"approve"}`)
	a.must(400, "POST", "/api/agents/m/plan", `{"draft":`+id+`,"action":"approve","subgoals":[{"agent":"k","title":"no checks"}]}`)
	a.must(202, "POST", "/api/agents/m/plan", `{"draft":`+id+`,"action":"approve","subgoals":[{"agent":"k","title":"do it now","checks":["true"]}]}`)
	for deadline := time.Now().Add(20 * time.Second); a.o.IsRunning("m"); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("run never finished")
		}
	}
	if g, _ := a.st.Goal("m"); g.Status != "done" {
		t.Fatalf("manager: %+v", g)
	}
	if g, _ := a.st.Goal("k"); g.Title != "do it now" || g.Status != "done" {
		t.Fatalf("report runs the approved goal: %+v", g)
	}
}

// Removing an agent removes its worktree, its branch in the user's repo, and its history.
func TestRemovedAgentIsCleanedUp(t *testing.T) {
	a := newApp(t)
	repo := newRepo(t)
	var p store.Project
	json.Unmarshal([]byte(a.must(200, "POST", "/api/projects", `{"name":"r","repo":"`+repo+`"}`)), &p)
	a.must(204, "POST", "/api/projects/"+p.ID+"/arrangement", `[{"id":"k","name":"Kid","role":"coder"},{"id":"s","name":"Stay","role":"coder"}]`)
	a.must(204, "PUT", "/api/agents/k", `{"name":"Kid","runtime":"generic","args":"true"}`)
	a.must(204, "PUT", "/api/agents/k/goal", `{"title":"t","checks":"true"}`)
	a.must(202, "POST", "/api/agents/k/run", "")
	for deadline := time.Now().Add(20 * time.Second); a.o.IsRunning("k"); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("run never finished")
		}
	}
	if !git.BranchExists(repo, "arranger/k") {
		t.Fatal("the run should have made a branch")
	}
	a.must(204, "POST", "/api/projects/"+p.ID+"/arrangement", `[{"id":"s","name":"Stay","role":"coder"}]`)
	if _, err := os.Stat(a.o.Dir("k")); err == nil {
		t.Error("worktree still there")
	}
	if git.BranchExists(repo, "arranger/k") {
		t.Error("branch still there")
	}
	var rows int
	a.st.SQL().QueryRow(`SELECT (SELECT count(*) FROM runs WHERE agent_id='k') + (SELECT count(*) FROM events WHERE agent_id='k')`).Scan(&rows)
	if rows != 0 {
		t.Errorf("%d history rows left", rows)
	}
}

// Opening the merge preview doesn't change the agent's work; merging includes what wasn't committed.
func TestMergePreviewOnlyReads(t *testing.T) {
	a := newApp(t)
	repo := newRepo(t)
	var p store.Project
	json.Unmarshal([]byte(a.must(200, "POST", "/api/projects", `{"name":"r","repo":"`+repo+`"}`)), &p)
	a.must(204, "POST", "/api/projects/"+p.ID+"/arrangement", `[{"id":"k","name":"Kid","role":"coder"}]`)
	a.must(204, "PUT", "/api/agents/k", `{"name":"Kid","runtime":"generic","args":"true"}`)
	a.must(204, "PUT", "/api/agents/k/goal", `{"title":"t","checks":"true"}`)
	a.must(202, "POST", "/api/agents/k/run", "")
	for a.o.IsRunning("k") {
		time.Sleep(50 * time.Millisecond)
	}
	os.WriteFile(filepath.Join(a.o.Dir("k"), "late.txt"), []byte("late\n"), 0o644) // changed after its last checkpoint
	head, _ := git.Run(a.o.Dir("k"), "rev-parse", "HEAD")
	var m git.MergePreview
	json.Unmarshal([]byte(a.must(200, "GET", "/api/agents/k/merge?target=out", "")), &m)
	if after, _ := git.Run(a.o.Dir("k"), "rev-parse", "HEAD"); after != head || m.Uncommitted != 1 {
		t.Fatalf("preview committed (%v) or missed the uncommitted file: %+v", after != head, m)
	}
	a.must(200, "POST", "/api/agents/k/merge", `{"target":"out","strategy":"merge"}`)
	if _, err := git.Run(repo, "cat-file", "-e", "out:late.txt"); err != nil {
		t.Fatal("merging should include the uncommitted file")
	}
}

// The queue API: add, edit, reorder, skip, remove and run goals one after another.
func TestQueueAPI(t *testing.T) {
	a := newApp(t)
	repo := newRepo(t)
	var p store.Project
	json.Unmarshal([]byte(a.must(200, "POST", "/api/projects", `{"name":"r","repo":"`+repo+`"}`)), &p)
	a.must(204, "POST", "/api/projects/"+p.ID+"/arrangement", `[{"id":"k","name":"Kid","role":"programmer"}]`)
	a.must(204, "PUT", "/api/agents/k", `{"name":"Kid","runtime":"generic","args":"true"}`)
	a.must(400, "POST", "/api/agents/k/queue", `{"title":"no checks"}`)
	a.must(400, "POST", "/api/agents/k/queue/start", "")
	ids := []int64{}
	for _, title := range []string{"one", "two", "three", "four"} {
		var it store.QueueItem
		json.Unmarshal([]byte(a.must(200, "POST", "/api/agents/k/queue", `{"title":"`+title+`","checks":"true"}`)), &it)
		ids = append(ids, it.ID)
	}
	id := func(i int) string { return strconv.FormatInt(ids[i], 10) }
	a.must(204, "PUT", "/api/agents/k/queue/"+id(0), `{"title":"one, edited","checks":"true"}`)
	a.must(204, "POST", "/api/agents/k/queue/order", `{"ids":[`+id(1)+`,`+id(0)+`,`+id(2)+`,`+id(3)+`]}`)
	a.must(204, "POST", "/api/agents/k/queue/"+id(2)+"/skip", "")
	a.must(204, "DELETE", "/api/agents/k/queue/"+id(3), "")
	a.must(204, "PUT", "/api/agents/k/queue/settings", `{"onFail":"skip"}`)
	a.must(400, "PUT", "/api/agents/k/queue/settings", `{"onFail":"explode"}`)
	a.must(202, "POST", "/api/agents/k/queue/start", "")
	var q store.Queue
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		json.Unmarshal([]byte(a.must(200, "GET", "/api/agents/k/queue", "")), &q)
		if !q.Active && !a.o.IsRunning("k") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queue never finished")
		}
	}
	order := []string{}
	for _, it := range q.Items {
		order = append(order, it.Title+":"+it.Status)
	}
	// finished goals come newest first; the skipped one stays in the history, the removed one is gone
	if got := strings.Join(order, " "); !strings.Contains(got, "one, edited:done") || !strings.Contains(got, "two:done") ||
		!strings.Contains(got, "three:skipped") || strings.Contains(got, "four") || q.OnFail != "skip" {
		t.Fatalf("queue: %s (onFail %s)", got, q.OnFail)
	}
	if g, _ := a.st.Goal("k"); g.Title != "one, edited" {
		t.Fatalf("the last goal it ran is its goal now: %q", g.Title)
	}
	a.must(409, "PUT", "/api/agents/k/queue/"+id(1), `{"title":"too late","checks":"true"}`)
}

// Run all runs each top-level agent's goals.
func TestRunAllRunsGoals(t *testing.T) {
	a := newApp(t)
	repo := newRepo(t)
	var p store.Project
	json.Unmarshal([]byte(a.must(200, "POST", "/api/projects", `{"name":"r","repo":"`+repo+`"}`)), &p)
	a.must(204, "POST", "/api/projects/"+p.ID+"/arrangement", `[{"id":"k","name":"Kid","role":"programmer"},{"id":"idle","name":"Idle","role":"programmer"}]`)
	a.must(204, "PUT", "/api/agents/k", `{"name":"Kid","runtime":"generic","args":"true"}`)
	a.must(200, "POST", "/api/agents/k/queue", `{"title":"one","checks":"true"}`)
	a.must(200, "POST", "/api/agents/k/queue", `{"title":"two","checks":"true"}`)
	var r struct{ Started int }
	json.Unmarshal([]byte(a.must(200, "POST", "/api/projects/"+p.ID+"/run", "")), &r)
	if r.Started != 1 {
		t.Fatalf("started %d, want only the agent with goals", r.Started)
	}
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if q, _ := a.st.Queue("k"); !q.Active && !a.o.IsRunning("k") {
			if q.Items[0].Status != "done" || q.Items[1].Status != "done" {
				t.Fatalf("goals: %+v", q.Items)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("goals never finished")
		}
	}
}

// Request changes on an older finished goal makes that goal the agent's goal again for the change.
func TestReviseFinishedGoal(t *testing.T) {
	a := newApp(t)
	repo := newRepo(t)
	var p store.Project
	json.Unmarshal([]byte(a.must(200, "POST", "/api/projects", `{"name":"r","repo":"`+repo+`"}`)), &p)
	a.must(204, "POST", "/api/projects/"+p.ID+"/arrangement", `[{"id":"k","name":"Kid","role":"programmer"}]`)
	a.must(204, "PUT", "/api/agents/k", `{"name":"Kid","runtime":"generic","args":"true"}`)
	var first store.QueueItem
	json.Unmarshal([]byte(a.must(200, "POST", "/api/agents/k/queue", `{"title":"first","checks":"true"}`)), &first)
	a.must(200, "POST", "/api/agents/k/queue", `{"title":"second","checks":"true"}`)
	a.must(202, "POST", "/api/agents/k/queue/start", "")
	wait := func() {
		for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			if q, _ := a.st.Queue("k"); !q.Active && !a.o.IsRunning("k") {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("never finished")
			}
		}
	}
	wait()
	if g, _ := a.st.Goal("k"); g.Title != "second" {
		t.Fatalf("goal after the list: %q", g.Title)
	}
	a.must(404, "POST", "/api/agents/k/revise", `{"change":"x","goal":99999}`)
	a.must(202, "POST", "/api/agents/k/revise", `{"change":"make it nicer","goal":`+strconv.FormatInt(first.ID, 10)+`}`)
	wait()
	if g, _ := a.st.Goal("k"); g.Title != "first" || g.Status != "done" {
		t.Fatalf("the change should run with the first goal: %+v", g)
	}
}

// Merging an agent's work lists every change from all its goals, not just the last goal.
func TestMergeMessageListsEveryChange(t *testing.T) {
	a := newApp(t)
	repo := newRepo(t)
	os.WriteFile(filepath.Join(repo, "w.sh"), []byte("p=$(cat); date +%s%N > out.txt\ncase \"$p\" in *first*) echo 'Commit: add the first thing' ;; *) echo 'Commit: add the second thing' ;; esac\n"), 0o644)
	git.Commit(repo, "add w.sh")
	var p store.Project
	json.Unmarshal([]byte(a.must(200, "POST", "/api/projects", `{"name":"r","repo":"`+repo+`"}`)), &p)
	a.must(204, "POST", "/api/projects/"+p.ID+"/arrangement", `[{"id":"k","name":"Kid","role":"programmer"}]`)
	a.must(204, "PUT", "/api/agents/k", `{"name":"Kid","runtime":"generic","args":"sh w.sh"}`)
	a.must(200, "POST", "/api/agents/k/queue", `{"title":"first goal","checks":"true"}`)
	a.must(200, "POST", "/api/agents/k/queue", `{"title":"second goal","checks":"true"}`)
	a.must(202, "POST", "/api/agents/k/queue/start", "")
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if q, _ := a.st.Queue("k"); !q.Active && !a.o.IsRunning("k") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never finished")
		}
	}
	var m git.MergePreview
	json.Unmarshal([]byte(a.must(200, "GET", "/api/agents/k/merge?target=release", "")), &m)
	want := "2 changes: Add the first thing; Add the second thing\n\n- Add the first thing\n- Add the second thing\n"
	if m.Message != want {
		t.Fatalf("preview message:\n%q\nwant\n%q", m.Message, want)
	}
	a.must(200, "POST", "/api/agents/k/merge", `{"target":"release","strategy":"merge"}`)
	if out, _ := git.Run(repo, "log", "-1", "--format=%B", "release"); strings.TrimSpace(out) != strings.TrimSpace(want) {
		t.Fatalf("merge commit message:\n%s", out)
	}
}
