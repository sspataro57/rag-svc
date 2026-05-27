// Package cache owns Redis/Valkey connectivity (both standalone and
// sentinel modes) plus higher-level caches like the query-embedding cache.
//
// Every caller should take a redis.UniversalClient interface rather than a
// concrete *redis.Client so tests can swap in miniredis and production can
// use the sentinel failover client transparently.
package cache

import (
	"context"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/treetop/rag-svc/internal/config"
)

// NewClient returns a Redis/Valkey client wired according to cfg. It pings
// the server before returning so misconfiguration fails at startup.
func NewClient(ctx context.Context, cfg config.Core) (redis.UniversalClient, error) {
	var client redis.UniversalClient
	switch strings.ToLower(cfg.RedisMode) {
	case "", "standalone":
		opts, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			return nil, fmt.Errorf("cache: parse REDIS_URL: %w", err)
		}
		if cfg.RedisPassword != "" && opts.Password == "" {
			opts.Password = cfg.RedisPassword
		}
		client = redis.NewClient(opts)
	case "sentinel":
		client = redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:       cfg.RedisMasterName,
			SentinelAddrs:    cfg.RedisSentinels,
			Password:         cfg.RedisPassword,
			SentinelPassword: cfg.RedisSentinelPassword,
			DB:               cfg.RedisDB,
		})
	default:
		return nil, fmt.Errorf("cache: unknown REDIS_MODE %q", cfg.RedisMode)
	}
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("cache: ping: %w", err)
	}
	return client, nil
}
