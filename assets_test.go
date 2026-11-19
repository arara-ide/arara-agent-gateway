package gateway

import (
	"encoding/json"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func catalog(t *testing.T, lang string) map[string]string {
	t.Helper()
	raw, err := fs.ReadFile(Web(), "locales/"+lang+".json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

var placeholder = regexp.MustCompile(`\{\d+\}`)

func TestLocaleParity(t *testing.T) {
	en := catalog(t, "en")
	for _, lang := range []string{"pt", "es"} {
		other := catalog(t, lang)
		if !slices.Equal(keys(en), keys(other)) {
			t.Fatalf("%s keys differ from en", lang)
		}
		for k, v := range en {
			if !slices.Equal(placeholder.FindAllString(v, -1), placeholder.FindAllString(other[k], -1)) {
				t.Fatalf("%s: placeholders of %q differ", lang, k)
			}
			if strings.TrimSpace(other[k]) == "" {
				t.Fatalf("%s: empty %q", lang, k)
			}
		}
	}
}

func TestDashboardKeysExist(t *testing.T) {
	en := catalog(t, "en")
	js, _ := fs.ReadFile(Web(), "app.js")
	html, _ := fs.ReadFile(Web(), "index.html")
	src := string(js) + string(html)
	used := regexp.MustCompile(`(?:\bt\('|tn\('|data-i18n=")([a-zA-Z_.]+)`).FindAllStringSubmatch(src, -1)
	for _, m := range used {
		key := m[1]
		if _, ok := en[key]; ok {
			continue
		}
		if _, ok := en[key+".other"]; ok {
			continue
		}
		t.Fatalf("missing locale key %q", key)
	}
	for _, prefix := range []string{"state.", "run.", "conn.", "login."} {
		found := false
		for k := range en {
			if strings.HasPrefix(k, prefix) {
				found = true
			}
		}
		if !found {
			t.Fatalf("no keys for %s", prefix)
		}
	}
}

func TestDashboardHasNoInlineScriptsOrStorage(t *testing.T) {
	html, _ := fs.ReadFile(Web(), "index.html")
	js, _ := fs.ReadFile(Web(), "app.js")
	if regexp.MustCompile(`<script(?:\s[^>]*)?>\s*[^<\s]`).Match(html) || strings.Contains(string(html), "onclick=") {
		t.Fatal("inline script would be blocked by CSP")
	}
	for _, banned := range []string{"localStorage", "sessionStorage", "document.cookie", "innerHTML"} {
		if strings.Contains(string(js), banned) {
			t.Fatalf("dashboard uses %s", banned)
		}
	}
}

func TestEveryAPIErrorIsTranslated(t *testing.T) {
	en := catalog(t, "en")
	src := ""
	for _, f := range []string{"errors.go", "server.go", "handlers.go", "sse.go"} {
		b, err := fs.ReadFile(apiSources, f)
		if err != nil {
			t.Fatal(err)
		}
		src += string(b)
	}
	for _, m := range regexp.MustCompile(`newError\(http\.\w+, "([a-z_]+)"`).FindAllStringSubmatch(src, -1) {
		if _, ok := en["error."+m[1]]; !ok {
			t.Fatalf("error code %q has no dashboard translation", m[1])
		}
	}
}

func TestOpenAPICoversEveryRoute(t *testing.T) {
	src, err := fs.ReadFile(apiSources, "server.go")
	if err != nil {
		t.Fatal(err)
	}
	spec := string(OpenAPI)
	for _, m := range regexp.MustCompile(`s\.handle\("([A-Z]+) ([^"]+)"`).FindAllStringSubmatch(string(src), -1) {
		path, method := m[2], strings.ToLower(m[1])
		idx := strings.Index(spec, "\n  "+path+":\n")
		if idx < 0 {
			t.Fatalf("route %s missing in docs/openapi.yaml", path)
		}
		block := spec[idx+1:]
		if next := regexp.MustCompile(`\n  /api/`).FindStringIndex(block[1:]); next != nil {
			block = block[:next[0]+1]
		}
		if !strings.Contains(block, "\n    "+method+":") {
			t.Fatalf("method %s %s missing in docs/openapi.yaml", method, path)
		}
	}
}
