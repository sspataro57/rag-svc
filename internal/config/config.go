package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/kelseyhightower/envconfig"
)

type Config struct {
	Core       Core
	LLM        LLM
	Jira       Jira
	Confluence Confluence
	Auth       Auth
	Search     Search
}

type Core struct {
	DatabaseURL string `envconfig:"DATABASE_URL" required:"true"`

	// Redis/Valkey config. Two modes are supported:
	//   standalone — single `REDIS_URL` (compose, local dev).
	//   sentinel   — `REDIS_SENTINELS` list + `REDIS_MASTER_NAME` for HA
	//                deployments (K8s Valkey-Sentinel). Both
	//                `REDIS_PASSWORD` (auth to master) and
	//                `REDIS_SENTINEL_PASSWORD` (auth to sentinel) are
	//                supported independently.
	RedisMode             string   `envconfig:"REDIS_MODE" default:"standalone"`
	RedisURL              string   `envconfig:"REDIS_URL"`
	RedisSentinels        []string `envconfig:"REDIS_SENTINELS"`
	RedisMasterName       string   `envconfig:"REDIS_MASTER_NAME" default:"mymaster"`
	RedisPassword         string   `envconfig:"REDIS_PASSWORD"`
	RedisSentinelPassword string   `envconfig:"REDIS_SENTINEL_PASSWORD"`

	BlobEndpoint  string `envconfig:"BLOB_ENDPOINT"`
	BlobBucket    string `envconfig:"BLOB_BUCKET" default:"rag-svc"`
	BlobAccessKey string `envconfig:"BLOB_ACCESS_KEY"`
	BlobSecretKey string `envconfig:"BLOB_SECRET_KEY"`
	HTTPAddr      string `envconfig:"HTTP_ADDR" default:":8080"`
	LogLevel      string `envconfig:"LOG_LEVEL" default:"info"`
}

type LLM struct {
	EmbedBaseURL   string `envconfig:"EMBED_BASE_URL" default:"https://api.openai.com/v1"`
	EmbedAPIKey    string `envconfig:"EMBED_API_KEY"`
	EmbedModel     string `envconfig:"EMBED_MODEL" default:"text-embedding-3-small"`
	EmbedBatchSize int    `envconfig:"EMBED_BATCH_SIZE" default:"96"`
	AnswerBaseURL  string `envconfig:"ANSWER_BASE_URL" default:"https://api.openai.com/v1"`
	AnswerAPIKey   string `envconfig:"ANSWER_API_KEY"`
	AnswerModel    string `envconfig:"ANSWER_MODEL" default:"gpt-4.1-mini"`
}

type Jira struct {
	BaseURL        string        `envconfig:"JIRA_BASE_URL"`
	Email          string        `envconfig:"JIRA_EMAIL"`
	Token          string        `envconfig:"JIRA_TOKEN"`
	Projects       []string      `envconfig:"JIRA_PROJECTS"`
	PollInterval   time.Duration `envconfig:"JIRA_POLL_INTERVAL" default:"5m"`
	RequestTimeout time.Duration `envconfig:"JIRA_REQUEST_TIMEOUT" default:"30s"`
	Workers        int           `envconfig:"JIRA_WORKERS" default:"4"`
}

type Confluence struct {
	BaseURL      string        `envconfig:"CONFLUENCE_BASE_URL"`
	Email        string        `envconfig:"CONFLUENCE_EMAIL"`
	Token        string        `envconfig:"CONFLUENCE_TOKEN"`
	Spaces       []string      `envconfig:"CONFLUENCE_SPACES"`
	PollInterval time.Duration `envconfig:"CONFLUENCE_POLL_INTERVAL" default:"10m"`
	Workers      int           `envconfig:"CONFLUENCE_WORKERS" default:"4"`
}

type Auth struct {
	OIDCIssuer         string `envconfig:"OIDC_ISSUER"`
	OIDCClientID       string `envconfig:"OIDC_CLIENT_ID"`
	OIDCClientSecret   string `envconfig:"OIDC_CLIENT_SECRET"`
	OIDCRedirectURL    string `envconfig:"OIDC_REDIRECT_URL"`
	OIDCSessionCookie  string `envconfig:"OIDC_SESSION_COOKIE" default:"rag_svc_session"`
	SlackSigningSecret string `envconfig:"SLACK_SIGNING_SECRET"`
	SlackBotToken      string `envconfig:"SLACK_BOT_TOKEN"`
	ExtensionID        string `envconfig:"EXTENSION_ID"`
	// WebUIOrigin is the origin that the web chat UI runs under
	// (e.g., https://rag.treetopllc.com). Added to the CORS allowlist so
	// the same-domain extension cookie flow works. Empty ⇒ not allowlisted.
	WebUIOrigin string `envconfig:"WEB_UI_ORIGIN"`
}

type Search struct {
	VectorWeight     float64       `envconfig:"SEARCH_VECTOR_WEIGHT" default:"0.6"`
	FTSWeight        float64       `envconfig:"SEARCH_FTS_WEIGHT" default:"0.4"`
	RecencyDecayDays int           `envconfig:"SEARCH_RECENCY_DECAY_DAYS" default:"180"`
	QueryCacheTTL    time.Duration `envconfig:"SEARCH_QUERY_CACHE_TTL" default:"300s"`
	RateLimitPerUser int           `envconfig:"SEARCH_RATE_LIMIT_PER_USER" default:"30"`
}

// Load reads configuration from the process environment. Fields tagged
// required:"true" (DATABASE_URL, REDIS_URL) must be set or Load returns an
// error. Subcommand-specific credentials (Jira, OIDC, LLM) are loaded but not
// validated here; each subcommand that uses them enforces its own
// requirements.
//
// Each section is processed separately with an empty prefix so env var names
// stay flat (DATABASE_URL, EMBED_BATCH_SIZE, JIRA_TOKEN, etc.) rather than
// being prefixed with the containing field name as envconfig defaults to.
// Core is processed first so a missing required field (DATABASE_URL,
// REDIS_URL) surfaces deterministically ahead of other parse errors.
func Load() (*Config, error) {
	var cfg Config
	sections := []struct {
		name string
		v    any
	}{
		{"core", &cfg.Core},
		{"llm", &cfg.LLM},
		{"jira", &cfg.Jira},
		{"confluence", &cfg.Confluence},
		{"auth", &cfg.Auth},
		{"search", &cfg.Search},
	}
	for _, s := range sections {
		if err := envconfig.Process("", s.v); err != nil {
			return nil, fmt.Errorf("config %s: %w", s.name, err)
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate enforces mode-dependent invariants envconfig tags can't express:
// exactly one of (REDIS_URL) or (REDIS_SENTINELS) must be present, based on
// REDIS_MODE. We pick this up at Load time so startup fails fast with a
// readable error instead of deferring to the first Redis call.
func (c *Config) Validate() error {
	switch strings.ToLower(c.Core.RedisMode) {
	case "", "standalone":
		if c.Core.RedisURL == "" {
			return fmt.Errorf("config: REDIS_URL is required when REDIS_MODE=standalone")
		}
	case "sentinel":
		if len(c.Core.RedisSentinels) == 0 {
			return fmt.Errorf("config: REDIS_SENTINELS is required when REDIS_MODE=sentinel")
		}
		if c.Core.RedisMasterName == "" {
			return fmt.Errorf("config: REDIS_MASTER_NAME is required when REDIS_MODE=sentinel")
		}
	default:
		return fmt.Errorf("config: unknown REDIS_MODE %q (expected standalone|sentinel)", c.Core.RedisMode)
	}
	return nil
}
