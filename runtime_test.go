package main

import (
	"bytes"
	"os"
	"reflect"
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
		{Kind: "done", Text: "Done.", Cost: 0.0257562, In: 17 + 10470 + 33842, Out: 283},
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
		{Kind: "done", In: 1200, Out: 40},
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
