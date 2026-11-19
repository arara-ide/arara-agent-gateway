package config

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const minAPIKeyLength = 32

type Config struct {
	Port                 int
	DataDir              string
	LogLevel             string
	APIKey               string
	IntegrationAPIKey    string
	EncryptionKey        []byte
	GitHubClientID       string
	GitHubClientSecret   string
	GitHubRedirectURL    string
	GitHubScopes         string
	IdleTimeout          time.Duration
	IdleCheckInterval    time.Duration
	AutoStart            bool
	MaxActiveInstances   int
	MaxAgentsPerInstance int
	RunTimeout           time.Duration
	ContainerMemory      int64
	ContainerCPUs        float64
	ContainerPids        int64
	ContainerRuntime     string
	ContainerNetwork     string
	RuntimeImage         string
	DockerSocket         string
}

func (c Config) GitHubEnabled() bool {
	return c.GitHubClientID != ""
}

type Getenv func(string) string

func Load(getenv Getenv) (Config, error) {
	var errs []error
	str := func(key, fallback string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return fallback
	}
	integer := func(key string, fallback, min int) int {
		raw := str(key, "")
		if raw == "" {
			return fallback
		}
		v, err := strconv.Atoi(raw)
		if err != nil || v < min {
			errs = append(errs, fmt.Errorf("%s must be an integer >= %d", key, min))
			return fallback
		}
		return v
	}
	boolean := func(key string, fallback bool) bool {
		raw := str(key, "")
		if raw == "" {
			return fallback
		}
		v, err := strconv.ParseBool(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s must be true or false", key))
			return fallback
		}
		return v
	}

	cfg := Config{
		Port:                 integer("PORT", 3000, 1),
		DataDir:              str("DATA_DIR", "./data"),
		LogLevel:             strings.ToLower(str("LOG_LEVEL", "info")),
		APIKey:               str("API_KEY", ""),
		IntegrationAPIKey:    str("INTEGRATION_API_KEY", ""),
		GitHubClientID:       str("GITHUB_CLIENT_ID", ""),
		GitHubClientSecret:   str("GITHUB_CLIENT_SECRET", ""),
		GitHubRedirectURL:    str("GITHUB_REDIRECT_URL", ""),
		GitHubScopes:         str("GITHUB_SCOPES", "repo"),
		IdleTimeout:          time.Duration(integer("IDLE_TIMEOUT_SECONDS", 300, 30)) * time.Second,
		IdleCheckInterval:    time.Duration(integer("IDLE_CHECK_INTERVAL_SECONDS", 15, 1)) * time.Second,
		AutoStart:            boolean("AUTO_START", false),
		MaxActiveInstances:   integer("MAX_ACTIVE_INSTANCES", 10, 1),
		MaxAgentsPerInstance: integer("MAX_AGENTS_PER_INSTANCE", 3, 1),
		RunTimeout:           time.Duration(integer("RUN_TIMEOUT_SECONDS", 3600, 30)) * time.Second,
		ContainerPids:        int64(integer("CONTAINER_PIDS", 512, 64)),
		ContainerRuntime:     str("CONTAINER_RUNTIME", ""),
		ContainerNetwork:     str("CONTAINER_NETWORK", "aag-instances"),
		RuntimeImage:         str("RUNTIME_IMAGE", "ghcr.io/arara-ide/arara-agent-runtime:latest"),
		DockerSocket:         str("DOCKER_SOCKET", "/var/run/docker.sock"),
	}

	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, errors.New("LOG_LEVEL must be debug, info, warn or error"))
	}

	if len(cfg.APIKey) < minAPIKeyLength || cfg.APIKey == "change_me" {
		errs = append(errs, fmt.Errorf("API_KEY is required and must have at least %d characters", minAPIKeyLength))
	}
	if cfg.IntegrationAPIKey != "" {
		if len(cfg.IntegrationAPIKey) < minAPIKeyLength {
			errs = append(errs, fmt.Errorf("INTEGRATION_API_KEY must have at least %d characters", minAPIKeyLength))
		}
		if cfg.IntegrationAPIKey == cfg.APIKey {
			errs = append(errs, errors.New("INTEGRATION_API_KEY must differ from API_KEY"))
		}
	}

	key, err := ParseKey(str("CREDENTIALS_ENCRYPTION_KEY", ""))
	if err != nil {
		errs = append(errs, err)
	}
	cfg.EncryptionKey = key

	if mem, err := ParseBytes(str("CONTAINER_MEMORY", "4g")); err != nil {
		errs = append(errs, fmt.Errorf("CONTAINER_MEMORY: %w", err))
	} else {
		cfg.ContainerMemory = mem
	}
	if cpus, err := strconv.ParseFloat(str("CONTAINER_CPUS", "2"), 64); err != nil || cpus <= 0 {
		errs = append(errs, errors.New("CONTAINER_CPUS must be a positive number"))
	} else {
		cfg.ContainerCPUs = cpus
	}

	github := []string{cfg.GitHubClientID, cfg.GitHubClientSecret, cfg.GitHubRedirectURL}
	set := 0
	for _, v := range github {
		if v != "" {
			set++
		}
	}
	if set != 0 && set != len(github) {
		errs = append(errs, errors.New("GITHUB_CLIENT_ID, GITHUB_CLIENT_SECRET and GITHUB_REDIRECT_URL must be set together"))
	}
	if cfg.GitHubRedirectURL != "" {
		u, err := url.Parse(cfg.GitHubRedirectURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && !isLoopback(u.Hostname())) {
			errs = append(errs, errors.New("GITHUB_REDIRECT_URL must be an https URL (http only on localhost)"))
		}
	}

	return cfg, errors.Join(errs...)
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func ParseKey(raw string) ([]byte, error) {
	if raw == "" {
		return nil, errors.New("CREDENTIALS_ENCRYPTION_KEY is required (32 bytes, base64 or hex)")
	}
	if b, err := hex.DecodeString(raw); err == nil && len(b) == 32 {
		return b, nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(raw); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, errors.New("CREDENTIALS_ENCRYPTION_KEY must decode to exactly 32 bytes (base64 or hex)")
}

func ParseBytes(raw string) (int64, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "g"):
		mult, s = 1<<30, strings.TrimSuffix(s, "g")
	case strings.HasSuffix(s, "m"):
		mult, s = 1<<20, strings.TrimSuffix(s, "m")
	case strings.HasSuffix(s, "k"):
		mult, s = 1<<10, strings.TrimSuffix(s, "k")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return 0, errors.New("must be a positive size such as 512m or 4g")
	}
	n := int64(v * float64(mult))
	if n < 64<<20 {
		return 0, errors.New("must be at least 64m")
	}
	return n, nil
}
