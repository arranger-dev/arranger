package agents

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
)

func parseFile(t *testing.T, path string, parse func([]byte) []Event) []Event {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var es []Event
	for _, l := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		es = append(es, parse(l)...)
	}
	return es
}

func TestParseClaude(t *testing.T) {
	es := parseFile(t, "testdata/claude.jsonl", parseClaude)
	want := []Event{
		{Kind: "file", Text: "Write a.txt"},
		{Kind: "msg", Text: "Done."},
		{Kind: "done", Text: "Done.", Cost: 0.0257562, In: 17 + 10470 + 33842, Out: 283, ID: "total"},
		{Kind: "error", Text: "Permission denied: Bash"},
	}
	if !reflect.DeepEqual(es, want) {
		t.Fatalf("got  %+v\nwant %+v", es, want)
	}
}

func TestParseCodex(t *testing.T) {
	es := parseFile(t, "testdata/codex.jsonl", parseCodex)
	want := []Event{
		{Kind: "tool", Text: "$ bash -lc ls"},
		{Kind: "file", Text: "add a.txt"},
		{Kind: "msg", Text: "Created a.txt."},
		{Kind: "usage", In: 1200, Out: 40},
		{Kind: "error", Text: "stream disconnected"},
	}
	if !reflect.DeepEqual(es, want) {
		t.Fatalf("got  %+v\nwant %+v", es, want)
	}
}

func TestParseGarbage(t *testing.T) {
	if es := parseClaude([]byte("not json")); len(es) != 1 || es[0].Kind != "msg" {
		t.Fatalf("got %+v", es)
	}
}

func TestParseOthers(t *testing.T) {
	cases := map[string]struct {
		parse func([]byte) []Event
		want  []Event
	}{
		"opencode": {parseOpencode, []Event{
			{Kind: "file", Text: "write a.txt"},
			{Kind: "usage", In: 9811, Out: 83},
			{Kind: "msg", Text: "done"},
			{Kind: "usage", In: 8256, Out: 3},
		}},
		"pi": {parsePi, []Event{
			{Kind: "file", Text: "write a.txt"},
			{Kind: "error", Text: "command not found"},
			{Kind: "usage", In: 900, Out: 12, Cost: 0.004},
			{Kind: "msg", Text: "done"},
		}},
		"gemini": {parseGemini, []Event{
			{Kind: "file", Text: "write_file a.txt"},
			{Kind: "error", Text: "denied"},
			{Kind: "delta", Text: "Created "},
			{Kind: "delta", Text: "a.txt."},
			{Kind: "usage", In: 1200, Out: 100},
		}},
		"cursor": {parseCursor, []Event{
			{Kind: "msg", Text: "Creating the file."},
			{Kind: "file", Text: "write a.txt"},
			{Kind: "tool", Text: "shell ls"},
			{Kind: "done", Text: "Created a.txt.", ID: "total"},
		}},
	}
	for name, c := range cases {
		if es := parseFile(t, "testdata/"+name+".jsonl", c.parse); !reflect.DeepEqual(es, c.want) {
			t.Errorf("%s:\ngot  %+v\nwant %+v", name, es, c.want)
		}
	}
	// amp speaks Claude's format; its failures carry the reason in "error"
	es := parseFile(t, "testdata/amp.jsonl", parseClaude)
	if len(es) != 1 || es[0].Kind != "error" || !strings.Contains(es[0].Text, "no longer supported") {
		t.Errorf("amp: %+v", es)
	}
}

func TestCommand(t *testing.T) {
	if _, err := Runtimes["generic"].Command(context.Background(), "", "", "x", nil); err == nil {
		t.Error("generic without a command should fail")
	}
	cmd, err := Runtimes["generic"].Command(context.Background(), "", "sh -c cat", "hello", nil)
	if err != nil || cmd.Args[0] != "sh" {
		t.Fatalf("generic: %v %v", cmd, err)
	}
	if out, _ := cmd.Output(); string(out) != "hello" {
		t.Errorf("generic stdin: %q", out)
	}
	if cmd, err := Runtimes["codex"].Command(context.Background(), "o3", "--foo", "do it", nil); err == nil {
		if got := strings.Join(cmd.Args[1:], " "); got != "exec --json --full-auto -m o3 --foo -" {
			t.Errorf("codex args: %s", got)
		}
	}
}

func TestParseClaudeUsage(t *testing.T) {
	line := `{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":5,"cache_read_input_tokens":100,"output_tokens":7}}}`
	es := parseClaude([]byte(line))
	want := []Event{{Kind: "msg", Text: "hi"}, {Kind: "usage", ID: "m1", In: 105, Out: 7}}
	if !reflect.DeepEqual(es, want) {
		t.Fatalf("got %+v", es)
	}
}

// Claude Code refuses most shell commands in -p mode unless they're allowed; a worker's checks are.
func TestClaudeAllowsChecks(t *testing.T) {
	got := strings.Join(claudeAllow([]string{"go test ./...", "CI=1 npm run lint && ./scripts/verify.sh", " "}), " ")
	want := "--allowedTools Bash(go test ./...) Bash(go:*) Bash(CI=1 npm run lint && ./scripts/verify.sh) Bash(npm:*)"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if claudeAllow(nil) != nil {
		t.Fatal("no checks, no flag")
	}
	rt := Runtime{Bin: "sh", Args: withModel(""), Allow: claudeAllow}
	cmd, err := rt.Command(context.Background(), "", "--extra", "p", []string{"make test"})
	if err != nil {
		t.Fatal(err)
	}
	if a := strings.Join(cmd.Args[1:], " "); a != "--allowedTools Bash(make test) Bash(make:*) --extra" {
		t.Fatalf("args: %s", a)
	}
}
