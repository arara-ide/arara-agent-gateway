package agent

import (
	"encoding/json"
	"net/url"
	"strings"
)

type Antigravity struct{}

func (Antigravity) Name() string      { return "antigravity" }
func (Antigravity) Title() string     { return "Google Antigravity" }
func (Antigravity) Methods() []string { return []string{MethodAccount, MethodAPIKey} }

func (Antigravity) Run(in RunInput) Command {
	args := []string{"agy", "--print=" + in.Prompt, "--output-format", "stream-json", "--dangerously-skip-permissions"}
	if in.UseAPIKey {
		args = append(args, "--settings", `{"modelProvider":"gemini"}`)
	}
	if in.Model != "" {
		args = append(args, "--model", in.Model)
	}
	if in.Effort != "" {
		args = append(args, "--effort", in.Effort)
	}
	if in.SessionID != "" {
		args = append(args, "--conversation", in.SessionID)
	}
	return Command{Args: args}
}

func (Antigravity) APIKeyEnv(key string) []string { return []string{"GEMINI_API_KEY=" + key} }

func (Antigravity) StatusArgs() []string {
	return []string{"agy", "--output-format", "json", "models"}
}

func (Antigravity) ModelsCommand() *ModelsCommand {
	return &ModelsCommand{Args: []string{"agy", "--output-format", "json", "models"}}
}

func (Antigravity) ParseModels(stdout string) []Model { return ParseAntigravityModels(stdout) }

func (Antigravity) UsageCommand() *ModelsCommand {
	return usageCommand([]string{"agy", "-p", "/usage", "--output-format", "json"}, nil)
}

func (Antigravity) ParseUsage(stdout string) json.RawMessage { return ParseAntigravityUsage(stdout) }

func (Antigravity) ParseStatus(exitCode int, stdout string) (bool, string) {
	if exitCode != 0 {
		return false, ""
	}
	var out struct {
		Status   string `json:"status"`
		Response string `json:"response"`
	}
	start := strings.Index(stdout, "{")
	if start < 0 || json.Unmarshal([]byte(stdout[start:]), &out) != nil {
		return false, ""
	}
	if out.Status == "SUCCESS" && strings.TrimSpace(out.Response) != "" {
		return true, "Google"
	}
	return false, ""
}

func (Antigravity) Login() *Login {
	return &Login{
		Args:       []string{"agy"},
		Env:        []string{"SSH_CONNECTION=1 2 3 4", "TERM=xterm-256color"},
		Tty:        true,
		NeedsInput: true,
		AutoSend:   []AutoSend{{Match: "select login method", Input: "\r"}},
		Success:    []string{"successfully signed in", "signed in as", "login successful", "authentication successful"},
		Failure:    []string{"got an error:", "token exchange failed", "invalid_grant"},
		AuthorizeURL: func(u *url.URL) bool {
			q, err := url.ParseQuery(u.RawQuery)
			return err == nil && u.Hostname() == "accounts.google.com" &&
				(u.Path == "/o/oauth2/auth" || u.Path == "/o/oauth2/v2/auth") &&
				q.Get("response_type") == "code" && q.Get("client_id") != "" &&
				q.Get("state") != "" && q.Get("redirect_uri") != ""
		},
	}
}

func (Antigravity) Logout() *Logout {
	return &Logout{
		Args: []string{"agy", "-i", "/logout"},
		Env:  []string{"TERM=xterm-256color"},
		Tty:  true,
		Done: []string{"logged out", "signed out", "not signed in"},
	}
}

func (Antigravity) IsAuthFailure(text string) bool {
	return containsAny(text, "authentication required", "please sign in", "not authenticated", "api key not valid", "api_key_invalid", "permission_denied")
}

func (a Antigravity) NewParser() Parser { return &antigravityParser{adapter: a} }

type antigravityParser struct {
	adapter Antigravity
	out     Outcome
	text    strings.Builder
	tools   map[int]bool
}

func (p *antigravityParser) Line(line []byte) []Event {
	var msg struct {
		Event          string `json:"event"`
		ConversationID string `json:"conversation_id"`
		Step           struct {
			Index     int    `json:"step_index"`
			State     string `json:"state"`
			StepType  string `json:"step_type"`
			TextDelta string `json:"text_delta"`
		} `json:"step_update"`
		Result struct {
			ConversationID string `json:"conversation_id"`
			Status         string `json:"status"`
			Response       string `json:"response"`
			Error          string `json:"error"`
		} `json:"result"`
	}
	if json.Unmarshal(line, &msg) != nil {
		return nil
	}
	switch msg.Event {
	case "init":
		if msg.ConversationID != "" {
			p.out.SessionID = msg.ConversationID
			return []Event{{Type: "session", SessionID: msg.ConversationID}}
		}
	case "step_update":
		switch msg.Step.StepType {
		case "agent_response":
			if msg.Step.TextDelta != "" {
				p.text.WriteString(msg.Step.TextDelta)
				return []Event{{Type: "delta", Text: msg.Step.TextDelta}}
			}
		case "user_input", "system_message", "error_message", "":
		default:
			if p.tools == nil {
				p.tools = map[int]bool{}
			}
			if !p.tools[msg.Step.Index] {
				p.tools[msg.Step.Index] = true
				return []Event{{Type: "tool", Tool: msg.Step.StepType}}
			}
		}
	case "result":
		p.out.Completed = true
		if msg.Result.ConversationID != "" {
			p.out.SessionID = msg.Result.ConversationID
		}
		p.out.Response = msg.Result.Response
		if msg.Result.Status != "SUCCESS" {
			p.out.Failed = true
			reason := msg.Result.Error
			if reason == "" {
				reason = msg.Result.Response
			}
			if reason == "" {
				reason = "antigravity finished with status " + msg.Result.Status
			}
			p.out.Message = truncate(reason, 500)
			p.out.AuthError = p.adapter.IsAuthFailure(reason)
		}
	}
	return nil
}

func (p *antigravityParser) Outcome() Outcome {
	out := p.out
	if out.Response == "" {
		out.Response = p.text.String()
	}
	return out
}
