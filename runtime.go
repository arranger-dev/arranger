package main

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
)

// Event is one normalized thing an agent did.
// Kinds: msg | delta (streamed text, joined into a msg) | tool | file | usage | done | error.
type Event struct {
	Kind string
	Text string
	Cost float64
	In   int
	Out  int
}

// Runtime describes how to drive one agent CLI headlessly.
// The command is Bin + Args(model) + the agent's extra args + Tail, and the prompt
// goes in on stdin unless PromptArg puts it last on the command line.
type Runtime struct {
	Bin       string
	Args      func(model string) []string
	Tail      []string
	PromptArg bool
	Parse     func(line []byte) []Event
}

// withModel returns an Args func: base flags plus flag+model when a model is set.
func withModel(flag string, base ...string) func(string) []string {
	return func(model string) []string {
		args := append([]string{}, base...)
		if model != "" && flag != "" {
			args = append(args, flag, model)
		}
		return args
	}
}

var runtimes = map[string]Runtime{
	"claude":   {Bin: "claude", Args: withModel("--model", "-p", "--output-format", "stream-json", "--verbose", "--permission-mode", "acceptEdits"), Parse: parseClaude},
	"codex":    {Bin: "codex", Args: withModel("-m", "exec", "--json", "--full-auto"), Tail: []string{"-"}, Parse: parseCodex},
	"gemini":   {Bin: "gemini", Args: withModel("-m", "--output-format", "stream-json", "--approval-mode", "auto_edit"), Parse: parseGemini},
	"pi":       {Bin: "pi", Args: withModel("--model", "-p", "--mode", "json"), Parse: parsePi},
	"opencode": {Bin: "opencode", Args: withModel("-m", "run", "--format", "json"), Parse: parseOpencode},
	"cursor":   {Bin: "cursor-agent", Args: withModel("--model", "-p", "--output-format", "stream-json"), PromptArg: true, Parse: parseCursor},
	"amp":      {Bin: "amp", Args: withModel("", "-x", "--stream-json"), Parse: parseClaude}, // amp speaks Claude Code's stream JSON
	// generic runs the agent's extra args as the command (e.g. "aider --yes-always --message-file -");
	// every output line becomes a message.
	"generic": {Args: withModel(""), Parse: func(l []byte) []Event { return []Event{{Kind: "msg", Text: string(l)}} }},
}

// installedRuntimes reports which runtime CLIs are on PATH. generic is always available.
func installedRuntimes() map[string]bool {
	m := map[string]bool{}
	for name, rt := range runtimes {
		_, err := exec.LookPath(rt.Bin)
		m[name] = rt.Bin == "" || err == nil
	}
	return m
}

// command builds the process for a run. extra is the agent's free-form args.
func (rt Runtime) command(ctx context.Context, model, extra, prompt string) (*exec.Cmd, error) {
	bin, args := rt.Bin, append(rt.Args(model), strings.Fields(extra)...)
	if bin == "" {
		if len(args) == 0 {
			return nil, errors.New("generic runtime: put the command in the agent's extra args")
		}
		bin, args = args[0], args[1:]
	}
	if _, err := exec.LookPath(bin); err != nil {
		return nil, errors.New(bin + " is not installed")
	}
	args = append(args, rt.Tail...)
	if rt.PromptArg {
		args = append(args, prompt)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	if !rt.PromptArg {
		cmd.Stdin = strings.NewReader(prompt)
	}
	return cmd, nil
}

func fileTool(name string) bool {
	switch strings.ToLower(name) {
	case "edit", "write", "multiedit", "notebookedit", "create_file", "edit_file", "write_file", "replace", "patch", "delete", "apply_patch":
		return true
	}
	return false
}

func toolEvent(name string, input json.RawMessage) Event {
	kind := "tool"
	if fileTool(name) {
		kind = "file"
	}
	return Event{Kind: kind, Text: strings.TrimSpace(name + " " + toolArg(input))}
}

func parseClaude(line []byte) []Event {
	var m struct {
		Type    string          `json:"type"`
		Subtype string          `json:"subtype"`
		IsError bool            `json:"is_error"`
		Result  string          `json:"result"`
		Error   json.RawMessage `json:"error"`   // amp puts its failure reason here
		Message json.RawMessage `json:"message"` // an object on assistant/user lines, a string on some system lines
		Cost    float64         `json:"total_cost_usd"`
		Usage   struct {
			In     int `json:"input_tokens"`
			CacheW int `json:"cache_creation_input_tokens"`
			CacheR int `json:"cache_read_input_tokens"`
			Out    int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(line, &m) != nil {
		return []Event{{Kind: "msg", Text: string(line)}}
	}
	var msg struct {
		Content []struct {
			Type    string          `json:"type"`
			Text    string          `json:"text"`
			Name    string          `json:"name"`
			Input   json.RawMessage `json:"input"`
			IsError bool            `json:"is_error"`
			Content json.RawMessage `json:"content"`
		} `json:"content"`
	}
	if m.Type == "assistant" || m.Type == "user" {
		json.Unmarshal(m.Message, &msg)
	}
	var es []Event
	switch m.Type {
	case "assistant":
		for _, c := range msg.Content {
			switch c.Type {
			case "text":
				if t := strings.TrimSpace(c.Text); t != "" {
					es = append(es, Event{Kind: "msg", Text: t})
				}
			case "tool_use":
				es = append(es, toolEvent(c.Name, c.Input))
			}
		}
	case "user":
		for _, c := range msg.Content {
			if c.Type == "tool_result" && c.IsError {
				es = append(es, Event{Kind: "error", Text: clip(rawText(c.Content), 300)})
			}
		}
	case "result":
		u := m.Usage
		e := Event{Kind: "done", Text: strings.TrimSpace(m.Result), Cost: m.Cost, In: u.In + u.CacheW + u.CacheR, Out: u.Out}
		if m.IsError || m.Subtype != "success" {
			e.Kind, e.Text = "error", m.Subtype+": "+clip(m.Result+rawText(m.Error), 300)
		}
		es = append(es, e)
	}
	return es
}

// parseCursor handles cursor-agent's stream JSON: Claude-style messages plus tool_call events.
func parseCursor(line []byte) []Event {
	var m struct {
		Type     string                                    `json:"type"`
		Subtype  string                                    `json:"subtype"`
		ToolCall map[string]struct{ Args json.RawMessage } `json:"tool_call"`
	}
	if json.Unmarshal(line, &m) == nil && m.Type == "tool_call" {
		if m.Subtype != "started" {
			return nil
		}
		for k, v := range m.ToolCall { // one key, e.g. "writeToolCall"
			return []Event{toolEvent(strings.TrimSuffix(k, "ToolCall"), v.Args)}
		}
		return nil
	}
	return parseClaude(line)
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
		return []Event{{Kind: "usage", In: m.Usage.In, Out: m.Usage.Out}}
	case m.Type == "turn.failed":
		return []Event{{Kind: "error", Text: m.Error.Message}}
	case m.Type == "error":
		return []Event{{Kind: "error", Text: m.Message}}
	}
	return nil
}

// parseGemini handles gemini-cli's stream-json: streamed assistant text, tool calls, a final result.
func parseGemini(line []byte) []Event {
	var m struct {
		Type     string          `json:"type"`
		Role     string          `json:"role"`
		Content  string          `json:"content"`
		Tool     string          `json:"tool_name"`
		Params   json.RawMessage `json:"parameters"`
		Status   string          `json:"status"`
		Message  string          `json:"message"`
		ErrorObj struct {
			Message string `json:"message"`
		} `json:"error"`
		Stats struct {
			In  int `json:"input_tokens"`
			Out int `json:"output_tokens"`
		} `json:"stats"`
	}
	if json.Unmarshal(line, &m) != nil {
		return []Event{{Kind: "msg", Text: string(line)}}
	}
	switch m.Type {
	case "message":
		if m.Role == "assistant" {
			return []Event{{Kind: "delta", Text: m.Content}}
		}
	case "tool_use":
		return []Event{toolEvent(m.Tool, m.Params)}
	case "tool_result":
		if m.Status == "error" {
			return []Event{{Kind: "error", Text: clip(m.ErrorObj.Message, 300)}}
		}
	case "error":
		return []Event{{Kind: "error", Text: m.Message}}
	case "result":
		if m.Status != "success" {
			return []Event{{Kind: "error", Text: "result: " + m.Status + " " + m.ErrorObj.Message}}
		}
		return []Event{{Kind: "usage", In: m.Stats.In, Out: m.Stats.Out}}
	}
	return nil
}

// parsePi handles pi's --mode json session events.
func parsePi(line []byte) []Event {
	var m struct {
		Type     string          `json:"type"`
		ToolName string          `json:"toolName"`
		Args     json.RawMessage `json:"args"`
		IsError  bool            `json:"isError"`
		Result   struct {
			Content json.RawMessage `json:"content"`
		} `json:"result"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			StopReason   string `json:"stopReason"`
			ErrorMessage string `json:"errorMessage"`
			Usage        struct {
				In   int `json:"input"`
				Out  int `json:"output"`
				Cost struct {
					Total float64 `json:"total"`
				} `json:"cost"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &m) != nil {
		return []Event{{Kind: "msg", Text: string(line)}}
	}
	switch m.Type {
	case "tool_execution_start":
		return []Event{toolEvent(m.ToolName, m.Args)}
	case "tool_execution_end":
		if m.IsError {
			return []Event{{Kind: "error", Text: clip(rawText(m.Result.Content), 300)}}
		}
	case "message_end":
		msg := m.Message
		if msg.Role != "assistant" {
			return nil
		}
		var text []string
		for _, c := range msg.Content {
			if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
				text = append(text, strings.TrimSpace(c.Text))
			}
		}
		es := []Event{{Kind: "usage", In: msg.Usage.In, Out: msg.Usage.Out, Cost: msg.Usage.Cost.Total}}
		if len(text) > 0 {
			es = append(es, Event{Kind: "msg", Text: strings.Join(text, "\n")})
		}
		if msg.StopReason == "error" || msg.StopReason == "aborted" {
			es = append(es, Event{Kind: "error", Text: msg.StopReason + ": " + msg.ErrorMessage})
		}
		return es
	}
	return nil
}

// parseOpencode handles `opencode run --format json` parts.
func parseOpencode(line []byte) []Event {
	var m struct {
		Type  string `json:"type"`
		Error struct {
			Name string `json:"name"`
			Data struct {
				Message string `json:"message"`
			} `json:"data"`
		} `json:"error"`
		Part struct {
			Text  string `json:"text"`
			Tool  string `json:"tool"`
			State struct {
				Status string          `json:"status"`
				Input  json.RawMessage `json:"input"`
				Error  string          `json:"error"`
			} `json:"state"`
			Cost   float64 `json:"cost"`
			Tokens struct {
				In  int `json:"input"`
				Out int `json:"output"`
			} `json:"tokens"`
		} `json:"part"`
	}
	if json.Unmarshal(line, &m) != nil {
		return []Event{{Kind: "msg", Text: string(line)}}
	}
	p := m.Part
	switch m.Type {
	case "text":
		if t := strings.TrimSpace(p.Text); t != "" {
			return []Event{{Kind: "msg", Text: t}}
		}
	case "tool_use":
		if p.State.Status == "error" {
			return []Event{toolEvent(p.Tool, p.State.Input), {Kind: "error", Text: clip(p.State.Error, 300)}}
		}
		return []Event{toolEvent(p.Tool, p.State.Input)}
	case "step_finish":
		return []Event{{Kind: "usage", In: p.Tokens.In, Out: p.Tokens.Out, Cost: p.Cost}}
	case "error":
		return []Event{{Kind: "error", Text: strings.TrimSpace(m.Error.Name + ": " + m.Error.Data.Message)}}
	}
	return nil
}

// toolArg picks the most telling field of a tool input for a one-line summary.
func toolArg(raw json.RawMessage) string {
	var in map[string]any
	json.Unmarshal(raw, &in)
	for _, k := range []string{"file_path", "filePath", "path", "command", "pattern", "url", "query", "description"} {
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
