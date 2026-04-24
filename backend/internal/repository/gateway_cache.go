package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const stickySessionPrefix = "sticky_session:"
const compatibilityExclusionPrefix = "compatibility_exclusion:"

type gatewayCache struct {
	rdb *redis.Client
}

func NewGatewayCache(rdb *redis.Client) service.GatewayCache {
	return &gatewayCache{rdb: rdb}
}

// buildSessionKey 构建 session key，包含 groupID 实现分组隔离
// 格式: sticky_session:{groupID}:{sessionHash}
func buildSessionKey(groupID int64, sessionHash string) string {
	return fmt.Sprintf("%s%d:%s", stickySessionPrefix, groupID, sessionHash)
}

func buildCompatibilityExclusionKey(groupID int64, scopeKey string) string {
	return fmt.Sprintf("%s%d:%s", compatibilityExclusionPrefix, groupID, scopeKey)
}

func (c *gatewayCache) GetSessionAccountID(ctx context.Context, groupID int64, sessionHash string) (int64, error) {
	key := buildSessionKey(groupID, sessionHash)
	return c.rdb.Get(ctx, key).Int64()
}

func (c *gatewayCache) SetSessionAccountID(ctx context.Context, groupID int64, sessionHash string, accountID int64, ttl time.Duration) error {
	key := buildSessionKey(groupID, sessionHash)
	return c.rdb.Set(ctx, key, accountID, ttl).Err()
}

func (c *gatewayCache) RefreshSessionTTL(ctx context.Context, groupID int64, sessionHash string, ttl time.Duration) error {
	key := buildSessionKey(groupID, sessionHash)
	return c.rdb.Expire(ctx, key, ttl).Err()
}

// DeleteSessionAccountID 删除粘性会话与账号的绑定关系。
// 当检测到绑定的账号不可用（如状态错误、禁用、不可调度等）时调用，
// 以便下次请求能够重新选择可用账号。
//
// DeleteSessionAccountID removes the sticky session binding for the given session.
// Called when the bound account becomes unavailable (e.g., error status, disabled,
// or unschedulable), allowing subsequent requests to select a new available account.
func (c *gatewayCache) DeleteSessionAccountID(ctx context.Context, groupID int64, sessionHash string) error {
	key := buildSessionKey(groupID, sessionHash)
	return c.rdb.Del(ctx, key).Err()
}

func (c *gatewayCache) GetCompatibilityExcludedPlatforms(ctx context.Context, groupID int64, scopeKey string) (map[string]time.Time, error) {
	key := buildCompatibilityExclusionKey(groupID, scopeKey)
	values, err := c.rdb.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return map[string]time.Time{}, nil
	}

	result := make(map[string]time.Time, len(values))
	for platform, raw := range values {
		unixSeconds, parseErr := time.Parse(time.RFC3339Nano, raw)
		if parseErr != nil {
			continue
		}
		result[platform] = unixSeconds
	}
	return result, nil
}

func (c *gatewayCache) SetCompatibilityExcludedPlatform(ctx context.Context, groupID int64, scopeKey string, platform string, observedAt time.Time, ttl time.Duration) error {
	key := buildCompatibilityExclusionKey(groupID, scopeKey)
	pipe := c.rdb.TxPipeline()
	pipe.HSet(ctx, key, platform, observedAt.UTC().Format(time.RFC3339Nano))
	pipe.Expire(ctx, key, ttl)
	_, err := pipe.Exec(ctx)
	return err
}
