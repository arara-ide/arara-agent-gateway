package agent

import (
	"strings"
	"testing"
)

func TestParseClaudeUsage(t *testing.T) {
	stdout := `{"type":"system","subtype":"init"}
{"type":"control_response","response":{"subtype":"success","request_id":"arara-init","response":{"models":[]}}}
{"type":"control_response","response":{"subtype":"success","request_id":"arara-usage","response":{"rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":42,"resets_at":"2026-11-20T03:00:00Z"}}}}}`
	got := string(ParseClaudeUsage(stdout))
	if !strings.Contains(got, `"rate_limits_available":true`) || strings.Contains(got, "models") {
		t.Fatalf("got %s", got)
	}
	if ParseClaudeUsage(`{"type":"control_response","response":{"subtype":"error","request_id":"arara-usage","error":"nope"}}`) != nil {
		t.Fatal("error response must be ignored")
	}
}

func TestParseCodexUsage(t *testing.T) {
	stdout := "{\"id\":1,\"result\":{}}\n{\"method\":\"account/updated\"}\n{\"id\":2,\"result\":{\"rateLimits\":{\"primary\":{\"usedPercent\":12,\"windowDurationMins\":300}}}}\n"
	got := string(ParseCodexUsage(stdout))
	if !strings.Contains(got, `"usedPercent":12`) {
		t.Fatalf("got %s", got)
	}
	if ParseCodexUsage("{\"id\":2,\"error\":{\"message\":\"x\"}}") != nil {
		t.Fatal("missing result must be nil")
	}
}

func TestParseAntigravityUsage(t *testing.T) {
	stdout := "Loading...\n{\"command\":{\"name\":\"usage\",\"data\":{\"groups\":[{\"name\":\"Gemini Models\",\"buckets\":[{\"window\":\"5h\",\"remaining_fraction\":0.75}]}]}}}\ntrailing"
	got := string(ParseAntigravityUsage(stdout))
	if !strings.Contains(got, `"remaining_fraction":0.75`) || strings.Contains(got, "trailing") {
		t.Fatalf("got %s", got)
	}
	if ParseAntigravityUsage("no json") != nil || ParseAntigravityUsage("{broken") != nil {
		t.Fatal("invalid output must be nil")
	}
}

func TestUsageCommandsAreBoundedAndNeverSendPrompts(t *testing.T) {
	for _, a := range []Adapter{Claude{}, Codex{}, Antigravity{}} {
		spec := a.UsageCommand()
		if spec == nil || len(spec.Args) < 3 || spec.Args[0] != "timeout" || spec.Args[1] != usageTimeoutSeconds {
			t.Fatalf("%s usage probe must be bounded by timeout: %v", a.Name(), spec)
		}
		if strings.Contains(string(spec.Stdin), `"type":"user"`) {
			t.Fatalf("%s usage probe sends a prompt", a.Name())
		}
	}
}
