package config

import (
	"strings"
	"testing"
	"time"
)

const goodKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func env(m map[string]string) Getenv {
	return func(k string) string { return m[k] }
}

func base() map[string]string {
	return map[string]string{"API_KEY": strings.Repeat("k", 40), "CREDENTIALS_ENCRYPTION_KEY": goodKey}
}

func TestDefaults(t *testing.T) {
	cfg, err := Load(env(base()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 3000 || cfg.IdleTimeout != 300*time.Second || cfg.IdleCheckInterval != 15*time.Second || cfg.AutoStart || cfg.MaxActiveInstances != 10 || cfg.MaxAgentsPerInstance != 3 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.ContainerMemory != 4<<30 || cfg.ContainerCPUs != 2 {
		t.Fatalf("unexpected container limits: %d %v", cfg.ContainerMemory, cfg.ContainerCPUs)
	}
	if cfg.GitHubEnabled() {
		t.Fatal("github must be disabled without credentials")
	}
}

func TestRejectsMissingSecrets(t *testing.T) {
	_, err := Load(env(map[string]string{"API_KEY": "change_me"}))
	if err == nil || !strings.Contains(err.Error(), "API_KEY") || !strings.Contains(err.Error(), "CREDENTIALS_ENCRYPTION_KEY") {
		t.Fatalf("expected errors for both secrets, got %v", err)
	}
}

func TestRejectsSameIntegrationKey(t *testing.T) {
	m := base()
	m["INTEGRATION_API_KEY"] = m["API_KEY"]
	if _, err := Load(env(m)); err == nil {
		t.Fatal("expected error")
	}
}

func TestGitHubAllOrNothing(t *testing.T) {
	m := base()
	m["GITHUB_CLIENT_ID"] = "abc"
	if _, err := Load(env(m)); err == nil {
		t.Fatal("expected error for partial github config")
	}
	m["GITHUB_CLIENT_SECRET"] = "s"
	m["GITHUB_REDIRECT_URL"] = "http://evil.example/cb"
	if _, err := Load(env(m)); err == nil {
		t.Fatal("expected error for non-https redirect")
	}
	m["GITHUB_REDIRECT_URL"] = "https://acweb.example/github/callback"
	cfg, err := Load(env(m))
	if err != nil || !cfg.GitHubEnabled() {
		t.Fatalf("expected github enabled, err=%v", err)
	}
}

func TestCustomIdle(t *testing.T) {
	m := base()
	m["IDLE_TIMEOUT_SECONDS"] = "60"
	m["IDLE_CHECK_INTERVAL_SECONDS"] = "5"
	m["AUTO_START"] = "true"
	cfg, err := Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IdleTimeout != time.Minute || cfg.IdleCheckInterval != 5*time.Second || !cfg.AutoStart {
		t.Fatalf("unexpected: %+v", cfg)
	}
	m["IDLE_TIMEOUT_SECONDS"] = "1"
	if _, err := Load(env(m)); err == nil {
		t.Fatal("expected minimum idle timeout")
	}
}

func TestParseBytesAndKey(t *testing.T) {
	cases := map[string]int64{"512m": 512 << 20, "4g": 4 << 30, "1.5g": 3 << 29}
	for in, want := range cases {
		got, err := ParseBytes(in)
		if err != nil || got != want {
			t.Fatalf("%s: got %d err %v", in, got, err)
		}
	}
	if _, err := ParseBytes("10k"); err == nil {
		t.Fatal("expected minimum size error")
	}
	if _, err := ParseKey("c2hvcnQ="); err == nil {
		t.Fatal("expected short key error")
	}
	if k, err := ParseKey("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="); err != nil || len(k) != 32 {
		t.Fatalf("base64 key: %v", err)
	}
}
