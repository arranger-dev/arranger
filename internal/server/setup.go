package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"arranger/internal/agents"
	"arranger/internal/store"
)

// Getting started: which agent CLIs are ready, checks that fit the project's repo, and starter teams.

// tool tells a new user how to get one agent CLI ready.
type tool struct {
	Runtime   string `json:"runtime"`
	Bin       string `json:"bin"`
	Installed bool   `json:"installed"`
	Version   string `json:"version"`
	Install   string `json:"install"`
	Login     string `json:"login"`
	Docs      string `json:"docs"`
}

// toolHelp is how to install each agent CLI and sign in; check the tool's docs if a command changes.
var toolHelp = map[string]struct{ install, login, docs string }{
	"claude":   {"npm install -g @anthropic-ai/claude-code", "claude  (then /login)", "https://docs.anthropic.com/en/docs/claude-code"},
	"codex":    {"npm install -g @openai/codex", "codex login", "https://github.com/openai/codex"},
	"gemini":   {"npm install -g @google/gemini-cli", "gemini  (then sign in)", "https://github.com/google-gemini/gemini-cli"},
	"opencode": {"npm install -g opencode-ai", "opencode auth login", "https://opencode.ai/docs"},
	"cursor":   {"curl https://cursor.com/install -fsS | bash", "cursor-agent login", "https://cursor.com/cli"},
	"amp":      {"npm install -g @sourcegraph/amp", "amp login", "https://ampcode.com"},
	"pi":       {"npm install -g @mariozechner/pi-coding-agent", "pi  (then /login)", "https://github.com/badlogic/pi-mono"},
}

func tools(ctx context.Context) []tool {
	ts := []tool{}
	for name, rt := range agents.Runtimes {
		h, ok := toolHelp[name]
		if !ok || rt.Bin == "" {
			continue
		}
		t := tool{Runtime: name, Bin: rt.Bin, Install: h.install, Login: h.login, Docs: h.docs}
		if _, err := exec.LookPath(rt.Bin); err == nil {
			t.Installed = true
			c, cancel := context.WithTimeout(ctx, 3*time.Second)
			if out, err := exec.CommandContext(c, rt.Bin, "--version").Output(); err == nil {
				t.Version = strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
			}
			cancel()
		}
		ts = append(ts, t)
	}
	sort.Slice(ts, func(i, j int) bool {
		if ts[i].Installed != ts[j].Installed {
			return ts[i].Installed
		}
		return ts[i].Runtime < ts[j].Runtime
	})
	return ts
}

// check is a shell check that fits the repo, and what it proves.
type check struct {
	Cmd string `json:"cmd"`
	Why string `json:"why"`
}

// suggestChecks reads the repo's build files and suggests checks that prove work is done.
func suggestChecks(repo string) []check {
	cs := []check{}
	has := func(f string) bool { _, err := os.Stat(filepath.Join(repo, f)); return err == nil }
	if repo == "" {
		return cs
	}
	if has("go.mod") {
		cs = append(cs, check{"go build ./...", "it compiles"}, check{"go vet ./...", "no suspicious code"}, check{"go test ./...", "the tests pass"})
	}
	if has("package.json") {
		pm := "npm"
		switch {
		case has("pnpm-lock.yaml"):
			pm = "pnpm"
		case has("yarn.lock"):
			pm = "yarn"
		case has("bun.lockb"), has("bun.lock"):
			pm = "bun"
		}
		var pkg struct{ Scripts map[string]string }
		if b, err := os.ReadFile(filepath.Join(repo, "package.json")); err == nil {
			json.Unmarshal(b, &pkg)
		}
		for _, s := range []struct{ name, why string }{{"build", "it builds"}, {"typecheck", "types check"}, {"lint", "lint passes"}, {"test", "the tests pass"}} {
			if sc, ok := pkg.Scripts[s.name]; ok && !strings.Contains(sc, "no test specified") {
				cs = append(cs, check{pm + " run " + s.name, s.why})
			}
		}
	}
	if has("Cargo.toml") {
		cs = append(cs, check{"cargo build", "it compiles"}, check{"cargo test", "the tests pass"})
	}
	if has("pyproject.toml") || has("setup.py") || has("requirements.txt") {
		cs = append(cs, check{"python3 -m pytest -q", "the tests pass"})
	}
	if b, err := os.ReadFile(filepath.Join(repo, "Makefile")); err == nil {
		for _, target := range []string{"test", "check", "lint"} {
			if strings.Contains("\n"+string(b), "\n"+target+":") {
				cs = append(cs, check{"make " + target, "make " + target + " passes"})
			}
		}
	}
	return cs
}

// setup returns what a new user needs: agent CLIs, checks for the project's repo, starter teams.
func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	p, _ := s.st.Project(r.URL.Query().Get("project"))
	ts, err := s.st.Templates()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]any{"tools": tools(r.Context()), "checks": suggestChecks(p.Repo), "templates": ts})
}

func (s *Server) saveTemplate(w http.ResponseWriter, r *http.Request) {
	var t store.Template
	if !readJSON(w, r, &t) {
		return
	}
	if err := s.st.SaveTemplate(&t); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, t)
}

func (s *Server) deleteTemplate(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteTemplate(r.PathValue("id")); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
