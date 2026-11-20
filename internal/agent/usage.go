package agent

import (
	"encoding/json"
	"strings"
)

const usageTimeoutSeconds = "20"

func usageCommand(args []string, stdin []byte) *ModelsCommand {
	return &ModelsCommand{Args: append([]string{"timeout", usageTimeoutSeconds}, args...), Stdin: stdin}
}

func jsonLines(stdout string, visit func(line []byte) json.RawMessage) json.RawMessage {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		if found := visit([]byte(line)); found != nil {
			return found
		}
	}
	return nil
}

func objectOrNil(raw json.RawMessage) json.RawMessage {
	var probe map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &probe) != nil {
		return nil
	}
	return raw
}

func ParseClaudeUsage(stdout string) json.RawMessage {
	return jsonLines(stdout, func(line []byte) json.RawMessage {
		var msg struct {
			Type     string `json:"type"`
			Response struct {
				Subtype   string          `json:"subtype"`
				RequestID string          `json:"request_id"`
				Response  json.RawMessage `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(line, &msg) != nil || msg.Type != "control_response" || msg.Response.RequestID != "arara-usage" || msg.Response.Subtype != "success" {
			return nil
		}
		return objectOrNil(msg.Response.Response)
	})
}

func ParseCodexUsage(stdout string) json.RawMessage {
	return jsonLines(stdout, func(line []byte) json.RawMessage {
		var msg struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(line, &msg) != nil || msg.ID != 2 {
			return nil
		}
		return objectOrNil(msg.Result)
	})
}

func ParseAntigravityUsage(stdout string) json.RawMessage {
	start := strings.Index(stdout, "{")
	if start < 0 {
		return nil
	}
	var raw json.RawMessage
	if json.NewDecoder(strings.NewReader(stdout[start:])).Decode(&raw) != nil {
		return nil
	}
	return objectOrNil(raw)
}
