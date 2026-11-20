package agent

import "testing"

func TestValidEffort(t *testing.T) {
	if !ValidEffort("high") || !ValidEffort("xhigh") {
		t.Fatal("expected valid effort")
	}
	if ValidEffort("") || ValidEffort("high\"\nbad=true") || ValidEffort("HIGH") {
		t.Fatal("expected invalid effort")
	}
}

func TestParseClaudeModels(t *testing.T) {
	stdout := `{"type":"control_response","response":{"response":{"models":[{"value":"default","displayName":"Claude Sonnet 4.6","supportsEffort":true,"supportedEffortLevels":["high","low","medium"]},{"value":"claude-haiku-4-5","displayName":"Claude Haiku 4.5","supportsEffort":false}]}}}`
	models := ParseClaudeModels(stdout)
	if len(models) != 2 {
		t.Fatalf("got %d models", len(models))
	}
	if models[0].ID != "default" || !models[0].IsDefault || len(models[0].Efforts) != 3 || models[0].Efforts[0] != "low" {
		t.Fatalf("unexpected first model: %+v", models[0])
	}
	if models[1].ID != "claude-haiku-4-5" || len(models[1].Efforts) != 0 {
		t.Fatalf("unexpected second model: %+v", models[1])
	}
}

func TestParseCodexModels(t *testing.T) {
	stdout := "{\"id\":1,\"result\":{}}\n{\"id\":2,\"result\":{\"data\":[{\"model\":\"gpt-5.4\",\"displayName\":\"GPT-5.4\",\"isDefault\":true,\"supportedReasoningEfforts\":[{\"reasoningEffort\":\"high\"},{\"reasoningEffort\":\"low\"}]},{\"model\":\"hidden\",\"hidden\":true}]}}\n"
	models := ParseCodexModels(stdout)
	if len(models) != 1 {
		t.Fatalf("got %d models", len(models))
	}
	if models[0].ID != "gpt-5.4" || !models[0].IsDefault || len(models[0].Efforts) != 2 || models[0].Efforts[0] != "low" {
		t.Fatalf("unexpected codex model: %+v", models[0])
	}
}

func TestParseAntigravityModels(t *testing.T) {
	jsonOut := `{"command":{"name":"models","data":{"models":[{"id":"gemini-4-neon-high","label":"Gemini 4 Neon (High)"},{"id":"gemini-4-neon-low","label":"Gemini 4 Neon (Low)"},{"id":"claude-sonnet-4-6-thinking","label":"Claude Sonnet 4.6 (Thinking)"}]}}}`
	models := ParseAntigravityModels(jsonOut)
	if len(models) != 2 {
		t.Fatalf("got %d models: %+v", len(models), models)
	}
	if models[0].Label != "Gemini 4 Neon" || len(models[0].Efforts) != 2 || models[0].Efforts[0] != "low" || models[0].Variants["high"] != "gemini-4-neon-high" {
		t.Fatalf("unexpected grouped model: %+v", models[0])
	}
	if models[1].Label != "Claude Sonnet 4.6" || models[1].Badge != "Thinking" {
		t.Fatalf("unexpected thinking model: %+v", models[1])
	}

	tsvOut := `{"conversation_id":"","status":"SUCCESS","response":"gemini-4-neon-low\tGemini 4 Neon (Low)\ngemini-4-neon-high\tGemini 4 Neon (High)"}`
	tsvModels := ParseAntigravityModels(tsvOut)
	if len(tsvModels) != 1 || len(tsvModels[0].Efforts) != 2 {
		t.Fatalf("unexpected tsv models: %+v", tsvModels)
	}
}
