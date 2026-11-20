package agent

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

type Model struct {
	ID        string            `json:"id"`
	Label     string            `json:"label"`
	Efforts   []string          `json:"efforts,omitempty"`
	Badge     string            `json:"badge,omitempty"`
	IsDefault bool              `json:"is_default,omitempty"`
	Variants  map[string]string `json:"variants,omitempty"`
}

type ModelsCommand struct {
	Args  []string
	Stdin []byte
}

var (
	effortPattern         = regexp.MustCompile(`^[a-z]{1,16}$`)
	antigravityEffortRe   = regexp.MustCompile(`^(.*) \((Low|Medium|High|Xhigh|Max)\)$`)
	antigravityThinkingRe = regexp.MustCompile(`^(.*?)\s*\(Thinking\)$`)
	effortOrder           = map[string]int{"none": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6, "ultra": 7}
)

func ValidEffort(s string) bool { return effortPattern.MatchString(s) }

func cleanEfforts(raw []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		e = strings.ToLower(strings.TrimSpace(e))
		if !ValidEffort(e) || seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		oi, oki := effortOrder[out[i]]
		oj, okj := effortOrder[out[j]]
		if oki && okj {
			return oi < oj
		}
		if oki != okj {
			return oki
		}
		return out[i] < out[j]
	})
	return out
}

func ParseClaudeModels(stdout string) []Model {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var msg struct {
			Type     string `json:"type"`
			Response struct {
				Response struct {
					Models []struct {
						Value                 string   `json:"value"`
						DisplayName           string   `json:"displayName"`
						SupportsEffort        bool     `json:"supportsEffort"`
						SupportedEffortLevels []string `json:"supportedEffortLevels"`
					} `json:"models"`
				} `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(line), &msg) != nil || msg.Type != "control_response" {
			continue
		}
		out := make([]Model, 0, len(msg.Response.Response.Models))
		for _, m := range msg.Response.Response.Models {
			if m.Value == "" {
				continue
			}
			label := m.DisplayName
			if label == "" {
				label = m.Value
			}
			var efforts []string
			if m.SupportsEffort {
				efforts = cleanEfforts(m.SupportedEffortLevels)
			}
			out = append(out, Model{
				ID:        m.Value,
				Label:     label,
				Efforts:   efforts,
				IsDefault: m.Value == "default",
			})
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func ParseCodexModels(stdout string) []Model {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var msg struct {
			ID     int `json:"id"`
			Result struct {
				Data []struct {
					Model                     string `json:"model"`
					DisplayName               string `json:"displayName"`
					Hidden                    bool   `json:"hidden"`
					IsDefault                 bool   `json:"isDefault"`
					SupportedReasoningEfforts []struct {
						ReasoningEffort string `json:"reasoningEffort"`
					} `json:"supportedReasoningEfforts"`
				} `json:"data"`
			} `json:"result"`
		}
		if json.Unmarshal([]byte(line), &msg) != nil || msg.ID != 2 || len(msg.Result.Data) == 0 {
			continue
		}
		out := make([]Model, 0, len(msg.Result.Data))
		for _, m := range msg.Result.Data {
			if m.Model == "" || m.Hidden {
				continue
			}
			label := m.DisplayName
			if label == "" {
				label = m.Model
			}
			rawEfforts := make([]string, 0, len(m.SupportedReasoningEfforts))
			for _, e := range m.SupportedReasoningEfforts {
				rawEfforts = append(rawEfforts, e.ReasoningEffort)
			}
			out = append(out, Model{
				ID:        m.Model,
				Label:     label,
				Efforts:   cleanEfforts(rawEfforts),
				IsDefault: m.IsDefault,
			})
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

type rawAntigravityModel struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

func ParseAntigravityModels(stdout string) []Model {
	var raw []rawAntigravityModel
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var cmdMsg struct {
			Command struct {
				Name string `json:"name"`
				Data struct {
					Models []rawAntigravityModel `json:"models"`
				} `json:"data"`
			} `json:"command"`
			Status   string `json:"status"`
			Response string `json:"response"`
		}
		if json.Unmarshal([]byte(line), &cmdMsg) != nil {
			continue
		}
		if cmdMsg.Command.Name == "models" && len(cmdMsg.Command.Data.Models) > 0 {
			raw = cmdMsg.Command.Data.Models
			break
		}
		if cmdMsg.Status == "SUCCESS" && strings.TrimSpace(cmdMsg.Response) != "" {
			for _, row := range strings.Split(cmdMsg.Response, "\n") {
				row = strings.TrimSpace(row)
				if row == "" {
					continue
				}
				id, label, ok := strings.Cut(row, "\t")
				if !ok {
					id, label = row, row
				}
				raw = append(raw, rawAntigravityModel{ID: strings.TrimSpace(id), Label: strings.TrimSpace(label)})
			}
			if len(raw) > 0 {
				break
			}
		}
	}
	if len(raw) == 0 {
		return nil
	}
	order := make([]string, 0, len(raw))
	grouped := map[string]*Model{}
	for _, item := range raw {
		if item.ID == "" || item.Label == "" {
			continue
		}
		if match := antigravityEffortRe.FindStringSubmatch(item.Label); match != nil {
			effort := strings.ToLower(match[2])
			if strings.HasSuffix(item.ID, "-"+effort) {
				key := strings.TrimSuffix(item.ID, "-"+effort)
				m, ok := grouped[key]
				if !ok {
					m = &Model{ID: item.ID, Label: match[1], Variants: map[string]string{}}
					grouped[key] = m
					order = append(order, key)
				}
				m.Variants[effort] = item.ID
				m.Efforts = append(m.Efforts, effort)
				continue
			}
		}
		m := &Model{ID: item.ID, Label: item.Label}
		if match := antigravityThinkingRe.FindStringSubmatch(item.Label); match != nil {
			m.Label = match[1]
			m.Badge = "Thinking"
		}
		if _, exists := grouped[item.ID]; !exists {
			order = append(order, item.ID)
		}
		grouped[item.ID] = m
	}
	out := make([]Model, 0, len(order))
	for _, key := range order {
		m := grouped[key]
		if len(m.Efforts) > 0 {
			m.Efforts = cleanEfforts(m.Efforts)
		}
		out = append(out, *m)
	}
	return out
}
