package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"arranger/internal/agents"
	"arranger/internal/git"
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

// check is a shell check that fits the repo, and what it proves. Specific checks prove the goal's
// own work: they fail before it's done. The rest (a build, the whole test suite) already pass on the
// code as it is, so they only prove nothing broke. Edit is a placeholder in Cmd the user replaces
// with something from their goal, e.g. the name of the test that proves it.
type check struct {
	Cmd      string `json:"cmd"`
	Why      string `json:"why"`
	Specific bool   `json:"specific,omitempty"`
	Edit     string `json:"edit,omitempty"`
}

// changedCheck suggests a check that fails when the goal's work changed nothing at all, so an agent
// that only claims success can't pass on checks that already passed before it started.
func changedCheck(repo string) check {
	return check{Cmd: fmt.Sprintf("! git diff --quiet %s...", git.DefaultTarget(repo)),
		Why: "the work changed something; fails if the agent changed no file", Specific: true}
}

// newTestCheck suggests a heuristic check that fails unless the diff against the repo's default
// branch touches a file matching pattern: it catches "tests pass" checks that would pass even if
// the goal added no test at all. It's a filename heuristic, not real coverage: a test added to an
// existing test file it doesn't rename, or an inline test in a non-matching file, slips through.
func newTestCheck(repo, pattern, label string) check {
	target := git.DefaultTarget(repo)
	return check{
		Cmd:      fmt.Sprintf("git diff --stat %s... | grep -q '%s'", target, pattern),
		Why:      fmt.Sprintf("heuristic: fails unless the diff touches a %s file; doesn't check the test actually covers the change", label),
		Specific: true,
	}
}

// suggestChecks reads the repo's build files and suggests checks that prove work is done.
func suggestChecks(repo string) []check {
	cs := []check{}
	has := func(f string) bool { _, err := os.Stat(filepath.Join(repo, f)); return err == nil }
	if repo == "" {
		return cs
	}
	cs = append(cs, changedCheck(repo))
	if has("go.mod") {
		target := git.DefaultTarget(repo)
		cs = append(cs,
			// checks that prove this goal's work, not just that nothing broke
			check{Cmd: `t=TestName; go test -run "^$t\$" -v ./... | grep -q -- "--- PASS: $t "`,
				Why: "a test named for this goal exists and passes; can't pass by matching no test. Replace TestName", Specific: true, Edit: "TestName"},
			check{Cmd: fmt.Sprintf(`p=$(git diff --name-only %s... -- '*.go' | sed -e 's|[^/]*$||' -e 's|^|./|' | sort -u); test -n "$p" && go test $p`, target),
				Why: "the tests of the packages the goal changed pass; fails if it changed no Go code", Specific: true},
			newTestCheck(repo, `_test\.go`, "_test.go"),
			check{Cmd: "go build ./...", Why: "it compiles"},
			check{Cmd: "go vet ./...", Why: "no suspicious code"},
			check{Cmd: "go test ./...", Why: "the existing tests still pass; won't fail if the goal added no test for its own change"},
		)
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
		for _, s := range []struct{ name, why string }{
			{"build", "it builds"},
			{"typecheck", "types check"},
			{"lint", "lint passes"},
			{"test", "the existing tests still pass; won't fail if the goal added no test for its own change"},
		} {
			if sc, ok := pkg.Scripts[s.name]; ok && !strings.Contains(sc, "no test specified") {
				cs = append(cs, check{Cmd: pm + " run " + s.name, Why: s.why})
				if s.name == "test" {
					cs = append(cs, newTestCheck(repo, `\.(test|spec)\.[jt]sx\?`, ".test./.spec."))
				}
			}
		}
	}
	if has("Cargo.toml") {
		cs = append(cs, check{Cmd: "t=test_name; cargo test $t -- --exact 2>&1 | grep -q \"test .*$t ... ok\"", Why: "a test named for this goal exists and passes. Replace test_name", Specific: true, Edit: "test_name"},
			check{Cmd: "cargo build", Why: "it compiles"}, check{Cmd: "cargo test", Why: "the existing tests still pass; won't fail if the goal added no test for its own change"})
	}
	if has("pyproject.toml") || has("setup.py") || has("requirements.txt") {
		cs = append(cs, check{Cmd: "python3 -m pytest -q -k test_name | grep -q ' passed'", Why: "a test named for this goal exists and passes; pytest exits 5 when nothing matches. Replace test_name", Specific: true, Edit: "test_name"},
			check{Cmd: "python3 -m pytest -q", Why: "the existing tests still pass; won't fail if the goal added no test for its own change"}, newTestCheck(repo, `test_.*\.py\|_test\.py`, "test_*.py / *_test.py"))
	}
	if b, err := os.ReadFile(filepath.Join(repo, "Makefile")); err == nil {
		for _, target := range []string{"test", "check", "lint"} {
			if strings.Contains("\n"+string(b), "\n"+target+":") {
				why := "make " + target + " passes"
				if target == "test" {
					why += "; won't fail if the goal added no test for its own change"
				}
				cs = append(cs, check{Cmd: "make " + target, Why: why})
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
