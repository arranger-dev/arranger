// Package server is arranger's HTTP interface: the arrange page, the JSON API it drives, and
// a Server-Sent Events stream of live updates.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"arranger/internal/agents"
	"arranger/internal/orch"
	"arranger/internal/store"
	"arranger/internal/version"
	"arranger/web"
)

type Server struct {
	st    *store.Store
	o     *orch.Orchestrator
	pages *template.Template
	build string // changes on every start, so pages served by an older process can tell they're stale
}

// New returns the HTTP handler. loopback restricts Host headers to localhost names, which
// blocks DNS-rebinding attacks when the server only listens locally.
func New(st *store.Store, o *orch.Orchestrator, loopback bool) http.Handler {
	s := &Server{st: st, o: o, pages: template.Must(template.ParseFS(web.FS, "*.html")), build: fmt.Sprint(time.Now().UnixNano())}
	mux := http.NewServeMux()
	mux.Handle("GET /{$}", http.RedirectHandler("/arrange", http.StatusFound))
	mux.HandleFunc("GET /arrange", s.arrange)
	mux.Handle("GET /app.js", http.FileServerFS(web.FS))
	mux.Handle("GET /arranger.png", http.FileServerFS(web.FS))
	mux.Handle("GET /fonts.css", http.FileServerFS(web.FS))
	mux.Handle("GET /fonts/", http.FileServerFS(web.FS))
	mux.HandleFunc("POST /api/projects", s.createProject)
	mux.HandleFunc("PUT /api/projects/{id}", s.updateProject)
	mux.HandleFunc("POST /api/projects/{id}/arrangement", s.saveArrangement)
	mux.HandleFunc("POST /api/projects/{id}/run", s.runAll)
	mux.HandleFunc("GET /api/projects/{id}/summary", s.summary)
	mux.HandleFunc("GET /api/projects/{id}/stats", s.stats)
	mux.HandleFunc("GET /api/agents/{id}", s.getAgent)
	mux.HandleFunc("PUT /api/agents/{id}", s.updateAgent)
	mux.HandleFunc("PUT /api/agents/{id}/goal", s.saveGoal)
	mux.HandleFunc("POST /api/agents/{id}/run", s.run)
	mux.HandleFunc("POST /api/agents/{id}/stop", s.stop)
	mux.HandleFunc("GET /api/agents/{id}/events", s.events)
	mux.HandleFunc("GET /api/agents/{id}/diff", s.diff)
	mux.HandleFunc("GET /api/agents/{id}/checkpoints", s.checkpoints)
	mux.HandleFunc("POST /api/agents/{id}/revert", s.revert)
	mux.HandleFunc("POST /api/agents/{id}/sync", s.sync)
	mux.HandleFunc("POST /api/agents/{id}/hunks", s.hunks)
	mux.HandleFunc("GET /api/agents/{id}/merge", s.merge)
	mux.HandleFunc("POST /api/agents/{id}/merge", s.merge)
	mux.HandleFunc("POST /api/types", s.saveType)
	mux.HandleFunc("PUT /api/types/{id}", s.saveType)
	mux.HandleFunc("DELETE /api/types/{id}", s.deleteType)
	mux.HandleFunc("GET /api/events", s.sse)
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, version.Get()) })
	return logRequests(guard(loopback, mux))
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

func (s *Server) arrange(w http.ResponseWriter, r *http.Request) {
	ps, err := s.st.Projects()
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
	as, err := s.st.Agents(cur.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	types, err := s.st.Types()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	installed := agents.Installed()
	names := make([]string, 0, len(installed))
	for n := range installed {
		names = append(names, n)
	}
	sort.Strings(names)
	if as == nil {
		as = []store.Agent{}
	}
	err = s.pages.ExecuteTemplate(w, "arrange.html", map[string]any{
		"Projects": ps, "Project": cur, "Agents": as, "Runtimes": names, "Installed": installed,
		"Types": types, "Build": s.build, "Version": version.Get(), "Repo": version.Repo,
	})
	if err != nil {
		zap.L().Error("render page", zap.Error(err))
	}
}

// safeID keeps agent ids usable as directory and git branch names.
var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

var colorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// validate rejects bad/duplicate ids, missing names, bad colors, unknown parents and cycles.
func validate(as []store.Agent) error {
	parent := map[string]string{}
	for _, a := range as {
		if !safeID.MatchString(a.ID) || strings.TrimSpace(a.Name) == "" {
			return fmt.Errorf("agent %q needs a name and an id of letters, digits, - or _", a.ID)
		}
		if a.Color != "" && !colorRe.MatchString(a.Color) {
			return fmt.Errorf("agent %s: color must look like #3d6fe0", a.Name)
		}
		if a.Soft < 0 || a.Hard < 0 || (a.Hard > 0 && a.Soft > a.Hard) {
			return fmt.Errorf("agent %s: token limits must be 0 (none) or positive, soft below hard", a.Name)
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

// fail writes err as a plain-text error with status code.
func fail(w http.ResponseWriter, code int, err error) { http.Error(w, err.Error(), code) }
