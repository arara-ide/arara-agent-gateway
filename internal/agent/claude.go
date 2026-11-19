package agent

import (
	"encoding/json"
	"strings"
)

type Claude struct{}

func (Claude) Name() string      { return "claude" }
func (Claude) Title() string     { return "Claude Code" }
func (Claude) Methods() []string { return []string{MethodAccount, MethodAPIKey} }

func (Claude) Run(in RunInput) Command {
	args := []string{"claude", "-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--dangerously-skip-permissions"}
	if in.Model != "" {
		args = append(args, "--model", in.Model)
	}
	if in.SessionID != "" {
		args = append(args, "--resume", in.SessionID)
	}
	return Command{Args: args, Stdin: []byte(in.Prompt)}
}

func (Claude) APIKeyEnv(key string) []string { return []string{"ANTHROPIC_API_KEY=" + key} }

func (Claude) StatusArgs() []string { return []string{"claude", "auth", "status", "--json"} }

func (Claude) ParseStatus(_ int, stdout string) (bool, string) {
	var st struct {
		LoggedIn   bool   `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
		Email      string `json:"email"`
	}
	start := strings.Index(stdout, "{")
	if start < 0 || json.Unmarshal([]byte(stdout[start:]), &st) != nil {
		return false, ""
	}
	account := st.Email
	if account == "" && st.LoggedIn {
		account = st.AuthMethod
	}
	return st.LoggedIn, account
}

func (Claude) Login() *Login {
	return &Login{Args: []string{"claude", "auth", "login", "--claudeai"}, Tty: true, NeedsInput: true}
}

func (Claude) LogoutArgs() []string { return []string{"claude", "auth", "logout"} }

func (Claude) IsAuthFailure(text string) bool {
	return containsAny(text, "not logged in", "please run /login", "invalid api key", "authentication_error", "oauth token has expired", "oauth token revoked")
}

func (c Claude) NewParser() Parser { return &claudeParser{adapter: c} }

type claudeParser struct {
	adapter Claude
	out     Outcome
	text    strings.Builder
}

func (p *claudeParser) Line(line []byte) []Event {
	var msg struct {
		Type      string          `json:"type"`
		Subtype   string          `json:"subtype"`
		SessionID string          `json:"session_id"`
		IsError   bool            `json:"is_error"`
		Result    string          `json:"result"`
		Event     json.RawMessage `json:"event"`
		Message   struct {
			Content []struct {
				Type  string          `json:"type"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &msg) != nil {
		return nil
	}
	var events []Event
	switch msg.Type {
	case "system":
		if msg.Subtype == "init" && msg.SessionID != "" && p.out.SessionID == "" {
			p.out.SessionID = msg.SessionID
			events = append(events, Event{Type: "session", SessionID: msg.SessionID})
		}
	case "stream_event":
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal(msg.Event, &ev) == nil && ev.Type == "content_block_delta" && ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
			p.text.WriteString(ev.Delta.Text)
			events = append(events, Event{Type: "delta", Text: ev.Delta.Text})
		}
	case "assistant":
		for _, c := range msg.Message.Content {
			if c.Type == "tool_use" {
				events = append(events, Event{Type: "tool", Tool: c.Name, Detail: truncate(string(c.Input), 300)})
			}
		}
	case "result":
		p.out.Completed = true
		if msg.SessionID != "" {
			p.out.SessionID = msg.SessionID
		}
		p.out.Response = msg.Result
		if p.out.Response == "" {
			p.out.Response = p.text.String()
		}
		if msg.IsError {
			p.out.Failed = true
			p.out.Message = truncate(msg.Result, 500)
			p.out.AuthError = p.adapter.IsAuthFailure(msg.Result)
		}
	}
	return events
}

func (p *claudeParser) Outcome() Outcome {
	out := p.out
	if out.Response == "" {
		out.Response = p.text.String()
	}
	return out
}
