package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoad_RequiredDatabaseURL(t *testing.T) {
	clearAllEnv(t)
	t.Setenv("REDIS_URL", "redis://localhost:6379")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when DATABASE_URL is unset, got nil")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("expected error to mention DATABASE_URL, got: %v", err)
	}
}

func TestLoad_RequiredRedisURL_StandaloneMode(t *testing.T) {
	clearAllEnv(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/db")
	// REDIS_MODE defaults to standalone; REDIS_URL missing should fail.

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when REDIS_URL is unset, got nil")
	}
	if !strings.Contains(err.Error(), "REDIS_URL") {
		t.Errorf("expected error to mention REDIS_URL, got: %v", err)
	}
}

func TestLoad_SentinelMode_RequiresSentinels(t *testing.T) {
	clearAllEnv(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/db")
	t.Setenv("REDIS_MODE", "sentinel")
	// REDIS_SENTINELS missing.

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when REDIS_SENTINELS is unset in sentinel mode")
	}
	if !strings.Contains(err.Error(), "REDIS_SENTINELS") {
		t.Errorf("expected error to mention REDIS_SENTINELS, got: %v", err)
	}
}

func TestLoad_SentinelMode_Valid(t *testing.T) {
	clearAllEnv(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/db")
	t.Setenv("REDIS_MODE", "sentinel")
	t.Setenv("REDIS_SENTINELS", "valkey-sentinel.cnpg.svc.cluster.local:26379")
	t.Setenv("REDIS_MASTER_NAME", "mymaster")
	t.Setenv("REDIS_PASSWORD", "pw")
	t.Setenv("REDIS_SENTINEL_PASSWORD", "pw")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Core.RedisMode != "sentinel" {
		t.Errorf("mode: got %q", cfg.Core.RedisMode)
	}
	if len(cfg.Core.RedisSentinels) != 1 || cfg.Core.RedisSentinels[0] != "valkey-sentinel.cnpg.svc.cluster.local:26379" {
		t.Errorf("sentinels: %v", cfg.Core.RedisSentinels)
	}
	if cfg.Core.RedisMasterName != "mymaster" {
		t.Errorf("master: %q", cfg.Core.RedisMasterName)
	}
}

func TestLoad_UnknownRedisMode(t *testing.T) {
	clearAllEnv(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/db")
	t.Setenv("REDIS_MODE", "clustered")
	t.Setenv("REDIS_URL", "redis://x:6379")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "REDIS_MODE") {
		t.Fatalf("expected REDIS_MODE validation error, got %v", err)
	}
}

func TestLoad_Defaults(t *testing.T) {
	clearAllEnv(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/db")
	t.Setenv("REDIS_URL", "redis://localhost:6379")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	cases := []struct {
		name string
		got  any
		want any
	}{
		{"BlobBucket", cfg.Core.BlobBucket, "rag-svc"},
		{"HTTPAddr", cfg.Core.HTTPAddr, ":8080"},
		{"LogLevel", cfg.Core.LogLevel, "info"},
		{"EmbedBaseURL", cfg.LLM.EmbedBaseURL, "https://api.openai.com/v1"},
		{"EmbedModel", cfg.LLM.EmbedModel, "text-embedding-3-small"},
		{"EmbedBatchSize", cfg.LLM.EmbedBatchSize, 96},
		{"AnswerModel", cfg.LLM.AnswerModel, "gpt-4.1-mini"},
		{"JiraPollInterval", cfg.Jira.PollInterval, 5 * time.Minute},
		{"JiraRequestTimeout", cfg.Jira.RequestTimeout, 30 * time.Second},
		{"JiraWorkers", cfg.Jira.Workers, 4},
		{"ConfluencePollInterval", cfg.Confluence.PollInterval, 10 * time.Minute},
		{"ConfluenceWorkers", cfg.Confluence.Workers, 4},
		{"OIDCSessionCookie", cfg.Auth.OIDCSessionCookie, "rag_svc_session"},
		{"VectorWeight", cfg.Search.VectorWeight, 0.6},
		{"FTSWeight", cfg.Search.FTSWeight, 0.4},
		{"RecencyDecayDays", cfg.Search.RecencyDecayDays, 180},
		{"QueryCacheTTL", cfg.Search.QueryCacheTTL, 300 * time.Second},
		{"RateLimitPerUser", cfg.Search.RateLimitPerUser, 30},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestLoad_OverridesFromEnv(t *testing.T) {
	clearAllEnv(t)
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/db")
	t.Setenv("REDIS_URL", "redis://localhost:6379")
	t.Setenv("HTTP_ADDR", ":9000")
	t.Setenv("EMBED_MODEL", "custom-model")
	t.Setenv("JIRA_PROJECTS", "ENG,OPS,PLAT")
	t.Setenv("SEARCH_VECTOR_WEIGHT", "0.75")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if cfg.Core.HTTPAddr != ":9000" {
		t.Errorf("HTTPAddr: got %q, want %q", cfg.Core.HTTPAddr, ":9000")
	}
	if cfg.LLM.EmbedModel != "custom-model" {
		t.Errorf("EmbedModel: got %q, want %q", cfg.LLM.EmbedModel, "custom-model")
	}
	wantProjects := []string{"ENG", "OPS", "PLAT"}
	if len(cfg.Jira.Projects) != len(wantProjects) {
		t.Fatalf("JiraProjects: got %v, want %v", cfg.Jira.Projects, wantProjects)
	}
	for i, p := range wantProjects {
		if cfg.Jira.Projects[i] != p {
			t.Errorf("JiraProjects[%d]: got %q, want %q", i, cfg.Jira.Projects[i], p)
		}
	}
	if cfg.Search.VectorWeight != 0.75 {
		t.Errorf("VectorWeight: got %v, want %v", cfg.Search.VectorWeight, 0.75)
	}
}

// clearAllEnv unsets every env var the config reads so tests start from a
// clean slate regardless of the developer's shell environment.
func clearAllEnv(t *testing.T) {
	t.Helper()
	vars := []string{
		"DATABASE_URL",
		"REDIS_URL", "REDIS_MODE", "REDIS_SENTINELS", "REDIS_MASTER_NAME",
		"REDIS_PASSWORD", "REDIS_SENTINEL_PASSWORD",
		"BLOB_ENDPOINT", "BLOB_BUCKET",
		"BLOB_ACCESS_KEY", "BLOB_SECRET_KEY", "HTTP_ADDR", "LOG_LEVEL",
		"EMBED_BASE_URL", "EMBED_API_KEY", "EMBED_MODEL", "EMBED_BATCH_SIZE",
		"ANSWER_BASE_URL", "ANSWER_API_KEY", "ANSWER_MODEL",
		"JIRA_BASE_URL", "JIRA_EMAIL", "JIRA_TOKEN", "JIRA_PROJECTS",
		"JIRA_POLL_INTERVAL", "JIRA_REQUEST_TIMEOUT", "JIRA_WORKERS",
		"CONFLUENCE_BASE_URL", "CONFLUENCE_EMAIL", "CONFLUENCE_TOKEN",
		"CONFLUENCE_SPACES", "CONFLUENCE_POLL_INTERVAL", "CONFLUENCE_WORKERS",
		"OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET",
		"OIDC_REDIRECT_URL", "OIDC_SESSION_COOKIE",
		"SLACK_SIGNING_SECRET", "SLACK_BOT_TOKEN", "EXTENSION_ID",
		"WEB_UI_ORIGIN",
		"SEARCH_VECTOR_WEIGHT", "SEARCH_FTS_WEIGHT",
		"SEARCH_RECENCY_DECAY_DAYS", "SEARCH_QUERY_CACHE_TTL",
		"SEARCH_RATE_LIMIT_PER_USER",
	}
	for _, v := range vars {
		// t.Setenv registers a cleanup that restores the original value;
		// the immediate Unsetenv clears it for the test body. envconfig
		// treats empty-string env vars as set (and fails to parse int
		// fields), so we must actually unset rather than set to "".
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
}
