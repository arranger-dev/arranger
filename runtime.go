package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
)

// Event is one normalized thing an agent did.
// Kinds: msg | tool | file | usage | done | error.
type Event struct {
	Kind string
	Text string
	Cost float64
	In   int
	Out  int
}

// Runtime describes how to drive one agent CLI headlessly. The prompt goes in on stdin.
type Runtime struct {
	Bin   string
	Args  func(model string) []string
	Parse func(line []byte) []Event
}

var runtimes = map[string]Runtime{
	"claude": {
		Bin: "claude",
		Args: func(model string) []string {
			args := []string{"-p", "--output-format", "stream-json", "--verbose", "--permission-mode", "acceptEdits"}
			if model != "" {
				args = append(args, "--model", model)
			}
			return args
		},
		Parse: parseClaude,
	},
	"codex": {
		Bin: "codex",
		Args: func(model string) []string {
			args := []string{"exec", "--json", "--full-auto"}
			if model != "" {
				args = append(args, "-m", model)
			}
			return args
		},
		Parse: parseCodex,
	},
}

// installedRuntimes reports which runtime CLIs are on PATH.
func installedRuntimes() map[string]bool {
	m := map[string]bool{}
	for name, rt := range runtimes {
		_, err := exec.LookPath(rt.Bin)
		m[name] = err == nil
	}
	return m
}

// command builds the process for a run. extra is the agent's free-form args;
// codex needs "-" last to read the prompt from stdin.
func (rt Runtime) command(ctx context.Context, model, extra string) *exec.Cmd {
	args := append(rt.Args(model), strings.Fields(extra)...)
	if rt.Bin == "codex" {
		args = append(args, "-")
	}
	return exec.CommandContext(ctx, rt.Bin, args...)
}

func parseClaude(line []byte) []Event {
	var m struct {
		Type    string  `json:"type"`
		Subtype string  `json:"subtype"`
		IsError bool    `json:"is_error"`
		Result  string  `json:"result"`
		Cost    float64 `json:"total_cost_usd"`
		Usage   struct {
			In     int `json:"input_tokens"`
			CacheW int `json:"cache_creation_input_tokens"`
			CacheR int `json:"cache_read_input_tokens"`
			Out    int `json:"output_tokens"`
		} `json:"usage"`
		Message struct {
			Content []struct {
				Type    string          `json:"type"`
				Text    string          `json:"text"`
				Name    string          `json:"name"`
				Input   json.RawMessage `json:"input"`
				IsError bool            `json:"is_error"`
				Content json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &m) != nil {
		return []Event{{Kind: "msg", Text: string(line)}}
	}
	var es []Event
	switch m.Type {
	case "assistant":
		for _, c := range m.Message.Content {
			switch c.Type {
			case "text":
				if t := strings.TrimSpace(c.Text); t != "" {
					es = append(es, Event{Kind: "msg", Text: t})
				}
			case "tool_use":
				kind := "tool"
				switch c.Name {
				case "Edit", "Write", "MultiEdit", "NotebookEdit":
					kind = "file"
				}
				es = append(es, Event{Kind: kind, Text: c.Name + " " + toolArg(c.Input)})
			}
		}
	case "user":
		for _, c := range m.Message.Content {
			if c.Type == "tool_result" && c.IsError {
				es = append(es, Event{Kind: "error", Text: clip(rawText(c.Content), 300)})
			}
		}
	case "result":
		u := m.Usage
		e := Event{Kind: "done", Text: clip(m.Result, 300), Cost: m.Cost, In: u.In + u.CacheW + u.CacheR, Out: u.Out}
		if m.IsError || m.Subtype != "success" {
			e.Kind, e.Text = "error", m.Subtype+": "+clip(m.Result, 300)
		}
		es = append(es, e)
	}
	return es
}

func parseCodex(line []byte) []Event {
	var m struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
		Usage struct {
			In  int `json:"input_tokens"`
			Out int `json:"output_tokens"`
		} `json:"usage"`
		Item struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Command string `json:"command"`
			Query   string `json:"query"`
			Server  string `json:"server"`
			Tool    string `json:"tool"`
			Changes []struct {
				Path string `json:"path"`
				Kind string `json:"kind"`
			} `json:"changes"`
		} `json:"item"`
	}
	if json.Unmarshal(line, &m) != nil {
		return []Event{{Kind: "msg", Text: string(line)}}
	}
	it := m.Item
	switch {
	case m.Type == "item.started" && it.Type == "command_execution":
		return []Event{{Kind: "tool", Text: "$ " + it.Command}}
	case m.Type == "item.completed" && it.Type == "agent_message":
		return []Event{{Kind: "msg", Text: it.Text}}
	case m.Type == "item.completed" && it.Type == "file_change":
		var ps []string
		for _, c := range it.Changes {
			ps = append(ps, c.Kind+" "+c.Path)
		}
		return []Event{{Kind: "file", Text: strings.Join(ps, ", ")}}
	case m.Type == "item.started" && it.Type == "mcp_tool_call":
		return []Event{{Kind: "tool", Text: it.Server + "." + it.Tool}}
	case m.Type == "item.started" && it.Type == "web_search":
		return []Event{{Kind: "tool", Text: "search " + it.Query}}
	case m.Type == "turn.completed":
		return []Event{{Kind: "done", In: m.Usage.In, Out: m.Usage.Out}}
	case m.Type == "turn.failed":
		return []Event{{Kind: "error", Text: m.Error.Message}}
	case m.Type == "error":
		return []Event{{Kind: "error", Text: m.Message}}
	}
	return nil
}

// toolArg picks the most telling field of a tool input for a one-line summary.
func toolArg(raw json.RawMessage) string {
	var in map[string]any
	json.Unmarshal(raw, &in)
	for _, k := range []string{"file_path", "command", "pattern", "url", "query", "description", "path"} {
		if v, ok := in[k].(string); ok {
			return clip(v, 120)
		}
	}
	return ""
}

// rawText flattens a tool_result content, which is either a string or a list of text blocks.
func rawText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	json.Unmarshal(raw, &parts)
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
