package agent

import (
	"encoding/json"
	"strings"
)

type Codex struct{}

func (Codex) Name() string      { return "codex" }
func (Codex) Title() string     { return "Codex" }
func (Codex) Methods() []string { return []string{MethodAccount, MethodAPIKey} }

func (Codex) Run(in RunInput) Command {
	args := []string{"codex", "exec"}
	if in.SessionID != "" {
		args = append(args, "resume")
	}
	args = append(args, "--json", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox")
	if in.Model != "" {
		args = append(args, "--model", in.Model)
	}
	if in.SessionID != "" {
		args = append(args, in.SessionID)
	}
	args = append(args, "-")
	return Command{Args: args, Stdin: []byte(in.Prompt)}
}

func (Codex) APIKeyEnv(key string) []string { return []string{"CODEX_API_KEY=" + key} }

func (Codex) StatusArgs() []string { return []string{"codex", "login", "status"} }

func (Codex) ParseStatus(exitCode int, stdout string) (bool, string) {
	text := strings.TrimSpace(StripANSI(stdout))
	if exitCode != 0 || !strings.HasPrefix(strings.ToLower(text), "logged in") {
		return false, ""
	}
	if _, method, ok := strings.Cut(text, "using "); ok {
		return true, strings.TrimSpace(method)
	}
	return true, ""
}

func (Codex) Login() *Login {
	return &Login{Args: []string{"codex", "login", "--device-auth"}}
}

func (Codex) Logout() *Logout { return &Logout{Args: []string{"codex", "logout"}} }

func (Codex) IsAuthFailure(text string) bool {
	return containsAny(text, "401 unauthorized", "not logged in", "missing bearer", "refresh token", "invalid api key", "incorrect api key")
}

func (c Codex) NewParser() Parser { return &codexParser{adapter: c} }

type codexParser struct {
	adapter  Codex
	out      Outcome
	messages []string
}

func (p *codexParser) Line(line []byte) []Event {
	var msg struct {
		Type     string `json:"type"`
		ThreadID string `json:"thread_id"`
		Message  string `json:"message"`
		Error    struct {
			Message string `json:"message"`
		} `json:"error"`
		Item struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Command string `json:"command"`
			Server  string `json:"server"`
			Tool    string `json:"tool"`
			Changes []struct {
				Path string `json:"path"`
			} `json:"changes"`
		} `json:"item"`
	}
	if json.Unmarshal(line, &msg) != nil {
		return nil
	}
	switch msg.Type {
	case "thread.started":
		if msg.ThreadID != "" {
			p.out.SessionID = msg.ThreadID
			return []Event{{Type: "session", SessionID: msg.ThreadID}}
		}
	case "item.started":
		switch msg.Item.Type {
		case "command_execution":
			return []Event{{Type: "tool", Tool: "command", Detail: truncate(msg.Item.Command, 300)}}
		case "mcp_tool_call":
			return []Event{{Type: "tool", Tool: msg.Item.Server + "." + msg.Item.Tool}}
		}
	case "item.completed":
		switch msg.Item.Type {
		case "agent_message":
			text := msg.Item.Text
			if len(p.messages) > 0 {
				text = "\n\n" + text
			}
			p.messages = append(p.messages, msg.Item.Text)
			return []Event{{Type: "delta", Text: text}}
		case "file_change":
			paths := make([]string, 0, len(msg.Item.Changes))
			for _, c := range msg.Item.Changes {
				paths = append(paths, c.Path)
			}
			return []Event{{Type: "tool", Tool: "file_change", Detail: truncate(strings.Join(paths, ", "), 300)}}
		}
	case "turn.completed":
		p.out.Completed = true
	case "turn.failed":
		p.out.Completed = true
		p.fail(msg.Error.Message)
	case "error":
		if !strings.HasPrefix(msg.Message, "Reconnecting") {
			p.out.Message = truncate(msg.Message, 500)
		}
	}
	return nil
}

func (p *codexParser) fail(message string) {
	p.out.Failed = true
	p.out.Message = truncate(message, 500)
	p.out.AuthError = p.adapter.IsAuthFailure(message)
}

func (p *codexParser) Outcome() Outcome {
	out := p.out
	out.Response = strings.Join(p.messages, "\n\n")
	return out
}
