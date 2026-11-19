package agent

import (
	"regexp"
	"sort"
	"strings"
)

const (
	MethodAccount = "account"
	MethodAPIKey  = "api_key"
)

type Event struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Tool      string `json:"tool,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

type Outcome struct {
	Response  string
	SessionID string
	Failed    bool
	AuthError bool
	Message   string
	Completed bool
}

type Parser interface {
	Line(line []byte) []Event
	Outcome() Outcome
}

type RunInput struct {
	SessionID string
	Model     string
	UseAPIKey bool
	Prompt    string
}

type Command struct {
	Args  []string
	Stdin []byte
}

type AutoSend struct {
	Match string
	Input string
}

type Login struct {
	Args       []string
	Env        []string
	Tty        bool
	NeedsInput bool
	AutoSend   []AutoSend
	Success    []string
	Failure    []string
}

type Logout struct {
	Args []string
	Env  []string
	Tty  bool
	Done []string
}

type Adapter interface {
	Name() string
	Title() string
	Methods() []string
	Run(in RunInput) Command
	NewParser() Parser
	APIKeyEnv(key string) []string
	StatusArgs() []string
	ParseStatus(exitCode int, stdout string) (bool, string)
	Login() *Login
	Logout() *Logout
	IsAuthFailure(text string) bool
}

var registry = map[string]Adapter{}

func register(a Adapter) { registry[a.Name()] = a }

func init() {
	register(Claude{})
	register(Codex{})
	register(Antigravity{})
}

func Get(name string) (Adapter, bool) {
	a, ok := registry[name]
	return a, ok
}

func Names() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var (
	sessionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	modelPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/\[\]-]{0,79}$`)
	ansiPattern    = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[@-Z\\-_]`)
	urlPattern     = regexp.MustCompile(`https://[^\s"'<>\x1b]+`)
	codePattern    = regexp.MustCompile(`\b[A-Z0-9]{4,5}-[A-Z0-9]{4,5}\b`)
)

func ValidSessionID(s string) bool { return sessionPattern.MatchString(s) }
func ValidModel(s string) bool     { return modelPattern.MatchString(s) }

func StripANSI(s string) string { return ansiPattern.ReplaceAllString(s, "") }

func FindURL(text string, hints ...string) string {
	clean := StripANSI(text)
	for _, u := range urlPattern.FindAllString(clean, -1) {
		u = strings.TrimRight(u, ".,)")
		if len(hints) == 0 {
			return u
		}
		for _, h := range hints {
			if strings.Contains(u, h) {
				return u
			}
		}
	}
	return ""
}

func FindDeviceCode(text string) string {
	return codePattern.FindString(StripANSI(text))
}

func ContainsAny(text string, needles ...string) bool {
	lower := strings.ToLower(text)
	for _, n := range needles {
		if strings.Contains(lower, n) {
			return true
		}
	}
	return false
}

func containsAny(text string, needles ...string) bool {
	return ContainsAny(text, needles...)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
