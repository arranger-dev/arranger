package main

import (
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// githubURL is linked from the landing page.
// TODO: replace with the real repository once it's published.
const githubURL = "https://github.com/your-org/arranger"

// buildID changes on every start, so pages served by an older process can tell they're stale.
var buildID = fmt.Sprint(time.Now().UnixNano())

//go:embed *.html app.js
var files embed.FS

var tmpl = template.Must(template.ParseFS(files, "*.html"))

// Node is an Agent with its children, for rendering the tree.
type Node struct {
	Agent
	Children []*Node
}

// buildTree returns root nodes. Agents whose parent is missing become roots.
func buildTree(as []Agent) []*Node {
	nodes := map[string]*Node{}
	for _, a := range as {
		nodes[a.ID] = &Node{Agent: a}
	}
	var roots []*Node
	for _, a := range as {
		n := nodes[a.ID]
		if p, ok := nodes[a.Parent]; ok {
			p.Children = append(p.Children, n)
		} else {
			roots = append(roots, n)
		}
	}
	return roots
}

// safeID keeps agent ids usable as directory and git branch names.
var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// validate rejects bad/duplicate ids, missing names, unknown parents and cycles.
func validate(as []Agent) error {
	parent := map[string]string{}
	for _, a := range as {
		if !safeID.MatchString(a.ID) || strings.TrimSpace(a.Name) == "" {
			return fmt.Errorf("agent %q needs a name and an id of letters, digits, - or _", a.ID)
		}
		if _, dup := parent[a.ID]; dup {
			return errors.New("duplicate id " + a.ID)
		}
		parent[a.ID] = a.Parent
	}
	for id, p := range parent {
		if _, ok := parent[p]; p != "" && !ok {
			return errors.New("unknown parent " + p)
		}
		for seen := 0; p != ""; p = parent[p] {
			if p == id || seen > len(parent) {
				return errors.New("cycle at " + id)
			}
			seen++
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func landing(w http.ResponseWriter, r *http.Request) {
	if err := tmpl.ExecuteTemplate(w, "landing.html", map[string]string{"GitHub": githubURL}); err != nil {
		log.Println(err)
	}
}

func arrange(w http.ResponseWriter, r *http.Request) {
	ps, err := listProjects()
	if err != nil || len(ps) == 0 {
		http.Error(w, fmt.Sprint("no projects: ", err), http.StatusInternalServerError)
		return
	}
	cur := ps[0]
	for _, p := range ps {
		if p.ID == r.URL.Query().Get("p") {
			cur = p
		}
	}
	as, err := listAgents(cur.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	types, err := listTypes()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	installed := installedRuntimes()
	names := make([]string, 0, len(installed))
	for n := range installed {
		names = append(names, n)
	}
	sort.Strings(names)
	err = tmpl.ExecuteTemplate(w, "arrange.html", map[string]any{
		"Projects": ps, "Project": cur, "Roots": buildTree(as), "Runtimes": names, "Installed": installed, "Blank": &Node{},
		"Types": types, "Build": buildID,
	})
	if err != nil {
		log.Println(err)
	}
}

// cleanProject normalizes the repo path and checks it's a git repo with a valid base.
func cleanProject(p *Project) error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return errors.New("project needs a name")
	}
	p.Repo = strings.TrimSpace(p.Repo)
	if p.Repo == "" {
		return nil
	}
	if rest, ok := strings.CutPrefix(p.Repo, "~/"); ok {
		home, _ := os.UserHomeDir()
		p.Repo = filepath.Join(home, rest)
	}
	abs, err := filepath.Abs(p.Repo)
	if err != nil {
		return err
	}
	p.Repo = abs
	p.Base, err = checkRepo(p.Repo, strings.TrimSpace(p.Base))
	return err
}

func createProjectH(w http.ResponseWriter, r *http.Request) {
	var p Project
	if !readJSON(w, r, &p) {
		return
	}
	if err := cleanProject(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p, err := createProject(p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, p)
}

func updateProjectH(w http.ResponseWriter, r *http.Request) {
	var p Project
	if !readJSON(w, r, &p) {
		return
	}
	p.ID = r.PathValue("id")
	if err := cleanProject(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := updateProject(p); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, p)
}

func saveArrangementH(w http.ResponseWriter, r *http.Request) {
	var as []Agent
	if !readJSON(w, r, &as) {
		return
	}
	if err := validate(as); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := saveArrangement(r.PathValue("id"), as); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func getAgentH(w http.ResponseWriter, r *http.Request) {
	a, _, err := getAgent(r.PathValue("id"))
	if err != nil {
		http.Error(w, "agent not found (save the arrangement first)", http.StatusNotFound)
		return
	}
	g, err := getGoal(a.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"agent": a, "goal": g})
}

func updateAgentH(w http.ResponseWriter, r *http.Request) {
	var a Agent
	if !readJSON(w, r, &a) {
		return
	}
	a.ID = r.PathValue("id")
	if _, ok := runtimes[a.Runtime]; !ok {
		http.Error(w, "unknown runtime "+a.Runtime, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(a.Name) == "" {
		http.Error(w, "agent needs a name", http.StatusBadRequest)
		return
	}
	if a.Soft < 0 || a.Hard < 0 || (a.Hard > 0 && a.Soft > a.Hard) {
		http.Error(w, "token limits: use 0 for none, and keep the soft limit below the hard one", http.StatusBadRequest)
		return
	}
	if err := updateAgent(a); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func saveGoalH(w http.ResponseWriter, r *http.Request) {
	var g Goal
	if !readJSON(w, r, &g) {
		return
	}
	if err := saveGoal(r.PathValue("id"), g); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func runH(w http.ResponseWriter, r *http.Request) {
	if err := startGoal(r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func stopH(w http.ResponseWriter, r *http.Request) {
	stopGoal(r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

// runAllH starts every top-level agent that has a goal; managers run their own subtrees.
func runAllH(w http.ResponseWriter, r *http.Request) {
	as, err := listAgents(r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	started, errs := 0, []string{}
	for _, a := range as {
		if g, _ := getGoal(a.ID); a.Parent != "" || g.Title == "" {
			continue
		}
		if err := startGoal(a.ID); err != nil {
			errs = append(errs, err.Error())
		} else {
			started++
		}
	}
	writeJSON(w, map[string]any{"started": started, "errors": errs})
}

func eventsH(w http.ResponseWriter, r *http.Request) {
	es, err := listEvents(r.PathValue("id"), 1000)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, es)
}

// agentWork loads the agent named in the path along with its project, goal and worktree.
// dir is "" when the agent has never run.
func agentWork(w http.ResponseWriter, r *http.Request) (a Agent, p Project, g Goal, dir string, ok bool) {
	a, pid, err := getAgent(r.PathValue("id"))
	if err != nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	if p, err = getProject(pid); err == nil {
		g, err = getGoal(a.ID)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if g.Base == "" {
		g.Base = p.Base
	}
	dir = filepath.Join(worktreeRoot, a.ID)
	if _, err := os.Stat(dir); err != nil {
		dir = ""
	}
	return a, p, g, dir, true
}

// diffH returns the agent's changes, and mergedInto: the manager whose work already contains
// them (so removing a change here won't remove it there).
func diffH(w http.ResponseWriter, r *http.Request) {
	a, p, g, dir, ok := agentWork(w, r)
	if !ok {
		return
	}
	res := map[string]any{"files": []FileDiff{}, "mergedInto": ""}
	if dir == "" {
		writeJSON(w, res) // never ran, so no changes yet
		return
	}
	fs, err := worktreeDiff(dir, g.Base)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	res["files"] = fs
	if mgr, _, err := getAgent(a.Parent); err == nil && len(fs) > 0 && branchExists(p.Repo, branchOf(mgr.ID)) {
		if _, err := git(p.Repo, "merge-base", "--is-ancestor", branchOf(a.ID), branchOf(mgr.ID)); err == nil {
			res["mergedInto"] = mgr.Name
		}
	}
	writeJSON(w, res)
}

// mergeH previews (GET) or performs (POST) merging an agent's branch into a branch of the user's repo.
func mergeH(w http.ResponseWriter, r *http.Request) {
	a, p, g, dir, ok := agentWork(w, r)
	if !ok {
		return
	}
	if dir == "" {
		http.Error(w, a.Name+" has no work to merge yet", http.StatusBadRequest)
		return
	}
	var req struct{ Target, Strategy, Message string }
	if r.Method == http.MethodPost && !readJSON(w, r, &req) {
		return
	}
	if r.Method == http.MethodGet {
		req.Target = r.URL.Query().Get("target")
	}
	req.Target = strings.TrimSpace(req.Target)
	if req.Target == "" {
		req.Target = defaultTarget(p.Repo)
	}
	if _, err := git(p.Repo, "check-ref-format", "--branch", req.Target); err != nil || strings.HasPrefix(req.Target, "arranger/") {
		http.Error(w, fmt.Sprintf("%q isn't a branch name you can merge into", req.Target), http.StatusBadRequest)
		return
	}
	if isRunning(a.ID) {
		http.Error(w, "stop "+a.Name+" before merging its work", http.StatusConflict)
		return
	}
	checkpoint(dir, "arranger: save work before merge") // include anything not yet committed
	if r.Method == http.MethodGet {
		m, err := previewMerge(p.Repo, branchOf(a.ID), req.Target, p.Base)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, m)
		return
	}
	if req.Message = strings.TrimSpace(req.Message); req.Message == "" {
		req.Message = g.Title
	}
	if req.Message == "" {
		req.Message = "Merge work from " + a.Name
	}
	sha, err := mergeInto(p.Repo, branchOf(a.ID), req.Target, p.Base, req.Strategy, req.Message)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	e := LogEvent{Agent: a.ID, Kind: "merge", Text: fmt.Sprintf("merged into %s (%s) → %s", req.Target, req.Strategy, sha)}
	addEvent(&e)
	hub.publish(p.ID, map[string]any{"type": "event", "event": e})
	writeJSON(w, map[string]string{"sha": sha, "target": req.Target})
}

func typesH(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		if err := deleteType(r.PathValue("id")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var t AgentType
	if !readJSON(w, r, &t) {
		return
	}
	t.ID = r.PathValue("id") // "" on create
	t.Name = strings.ToLower(strings.TrimSpace(t.Name))
	if !safeID.MatchString(t.Name) {
		http.Error(w, "type name: letters, digits, - or _ only", http.StatusBadRequest)
		return
	}
	if !colorRe.MatchString(t.Color) {
		http.Error(w, "color must look like #3d6fe0", http.StatusBadRequest)
		return
	}
	if _, ok := runtimes[t.Runtime]; !ok {
		http.Error(w, "unknown runtime "+t.Runtime, http.StatusBadRequest)
		return
	}
	if err := saveType(&t); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, t)
}

var colorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func checkpointsH(w http.ResponseWriter, r *http.Request) {
	_, _, g, dir, ok := agentWork(w, r)
	if !ok {
		return
	}
	if dir == "" {
		writeJSON(w, []Checkpoint{})
		return
	}
	cs, err := checkpoints(dir, g.Base)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, cs)
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{4,40}$`)

// editable loads an agent whose worktree the user may change: it has run, and isn't running now.
func editable(w http.ResponseWriter, r *http.Request) (a Agent, p Project, g Goal, dir string, ok bool) {
	if a, p, g, dir, ok = agentWork(w, r); !ok {
		return
	}
	if dir == "" {
		http.Error(w, a.Name+" has no work yet", http.StatusBadRequest)
		return a, p, g, dir, false
	}
	if isRunning(a.ID) {
		http.Error(w, "stop "+a.Name+" before changing its work", http.StatusConflict)
		return a, p, g, dir, false
	}
	return a, p, g, dir, true
}

// changed recomputes an agent's diff stat and tells the UI.
func changed(p Project, a Agent, dir, base string) {
	refreshDiff(a.ID, dir, base)
	g, _ := getGoal(a.ID)
	hub.publish(p.ID, map[string]any{"type": "status", "agent": a.ID, "status": g.Status})
}

func revertH(w http.ResponseWriter, r *http.Request) {
	a, p, g, dir, ok := editable(w, r)
	if !ok {
		return
	}
	var req struct{ SHA string }
	if !readJSON(w, r, &req) {
		return
	}
	if !shaRe.MatchString(req.SHA) {
		http.Error(w, "bad commit id", http.StatusBadRequest)
		return
	}
	if err := revertCommit(dir, req.SHA); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	changed(p, a, dir, g.Base)
	w.WriteHeader(http.StatusNoContent)
}

// hunksH removes the selected hunks from the agent's work, or promotes them into its manager's work.
func hunksH(w http.ResponseWriter, r *http.Request) {
	a, p, g, dir, ok := editable(w, r)
	if !ok {
		return
	}
	var req struct {
		Action string
		IDs    []string
	}
	if !readJSON(w, r, &req) {
		return
	}
	ids := map[string]bool{}
	for _, id := range req.IDs {
		ids[id] = true
	}
	fs, err := worktreeDiff(dir, g.Base)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	patch, n := hunkPatch(fs, ids)
	if n == 0 || n != len(ids) {
		http.Error(w, "the diff changed since you loaded it; refresh and select again", http.StatusConflict)
		return
	}
	switch req.Action {
	case "remove":
		if _, err := gitIn(dir, patch, "apply", "-R", "-"); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		checkpoint(dir, fmt.Sprintf("arranger: user removed %d change(s)", n))
		notes := g.Notes + patch
		if len(notes) > 8000 { // keep the newest removals; the prompt shouldn't grow without bound
			notes = notes[len(notes)-8000:]
		}
		setGoal(a.ID, map[string]any{"notes": notes})
		changed(p, a, dir, g.Base)
	case "promote":
		mgr, _, err := getAgent(a.Parent)
		if a.Parent == "" || err != nil {
			http.Error(w, a.Name+" has no manager to promote to", http.StatusBadRequest)
			return
		}
		if isRunning(mgr.ID) {
			http.Error(w, "stop "+mgr.Name+" before changing its work", http.StatusConflict)
			return
		}
		mdir, mb, err := workspace(p, mgr)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := gitIn(mdir, patch, "apply", "--check", "-"); err != nil {
			http.Error(w, "doesn't apply cleanly to "+mgr.Name+"'s work: "+err.Error(), http.StatusConflict)
			return
		}
		gitIn(mdir, patch, "apply", "-")
		checkpoint(mdir, fmt.Sprintf("arranger: promote %d change(s) from %s", n, a.Name))
		changed(p, mgr, mdir, mb)
	default:
		http.Error(w, "action must be remove or promote", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func summaryH(w http.ResponseWriter, r *http.Request) {
	s, err := projectSummary(r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, s)
}

// sseH streams a project's live updates as Server-Sent Events.
func sseH(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	c := hub.subscribe(r.URL.Query().Get("project"))
	defer hub.unsubscribe(c)
	fmt.Fprintf(w, "data: {\"type\":\"hello\",\"build\":%q}\n\n", buildID) // lets an open page notice a restart onto new code
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-c:
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
	}
}

// guard blocks cross-site writes (CSRF) and, on loopback, foreign Host headers
// (DNS rebinding). This server starts processes, so any page on the web must not drive it.
func guard(loopback bool, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if loopback && host != "localhost" && host != "127.0.0.1" && host != "::1" {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); r.Method != http.MethodGet && o != "" && o != "http://"+r.Host {
			http.Error(w, "cross-origin request blocked", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func main() {
	home, _ := os.UserHomeDir()
	addr := flag.String("addr", "127.0.0.1:7777", "listen address")
	data := flag.String("data", filepath.Join(home, ".arranger"), "data directory")
	parallel := flag.Int("parallel", 4, "max agent processes running at once")
	flag.Parse()
	slots = make(chan struct{}, max(1, *parallel))
	worktreeRoot = filepath.Join(*data, "worktrees")
	if err := openDB(filepath.Join(*data, "arranger.db")); err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("GET /{$}", landing)
	http.HandleFunc("GET /arrange", arrange)
	http.Handle("GET /app.js", http.FileServerFS(files))
	http.HandleFunc("POST /api/projects", createProjectH)
	http.HandleFunc("PUT /api/projects/{id}", updateProjectH)
	http.HandleFunc("POST /api/projects/{id}/arrangement", saveArrangementH)
	http.HandleFunc("POST /api/projects/{id}/run", runAllH)
	http.HandleFunc("GET /api/projects/{id}/summary", summaryH)
	http.HandleFunc("GET /api/agents/{id}", getAgentH)
	http.HandleFunc("PUT /api/agents/{id}", updateAgentH)
	http.HandleFunc("PUT /api/agents/{id}/goal", saveGoalH)
	http.HandleFunc("POST /api/agents/{id}/run", runH)
	http.HandleFunc("POST /api/agents/{id}/stop", stopH)
	http.HandleFunc("GET /api/agents/{id}/events", eventsH)
	http.HandleFunc("GET /api/agents/{id}/diff", diffH)
	http.HandleFunc("GET /api/agents/{id}/checkpoints", checkpointsH)
	http.HandleFunc("POST /api/agents/{id}/revert", revertH)
	http.HandleFunc("POST /api/agents/{id}/hunks", hunksH)
	http.HandleFunc("GET /api/agents/{id}/merge", mergeH)
	http.HandleFunc("POST /api/agents/{id}/merge", mergeH)
	http.HandleFunc("POST /api/types", typesH)
	http.HandleFunc("PUT /api/types/{id}", typesH)
	http.HandleFunc("DELETE /api/types/{id}", typesH)
	http.HandleFunc("GET /api/events", sseH)

	host, _, _ := net.SplitHostPort(*addr)
	ip := net.ParseIP(host)
	log.Printf("arranger on http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, guard(host == "localhost" || ip != nil && ip.IsLoopback(), http.DefaultServeMux)))
}
