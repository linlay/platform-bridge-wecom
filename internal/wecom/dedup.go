// Package dedup 对企业微信 Bot 推送来的重复消息做幂等抑制。
//
// 键：normalizedAppKey + ":" + reqId；TTL 默认 10 分钟（对齐 Java processedMessages）。
package wecom

import (
	"strings"
	"sync"
	"time"
)

type Cache struct {
	ttl time.Duration
	mu  sync.Mutex
	m   map[string]time.Time
}

func NewDedup(ttl time.Duration) *Cache { return &Cache{ttl: ttl, m: make(map[string]time.Time)} }

// Seen 若 key 已存在且未过期，返回 true（视为重复并跳过）；否则登记并返回 false。
// reqId 为空时不做抑制（Java 的 trimToEmpty 语义）。
func (c *Cache) Seen(appKey, reqID string) bool {
	if strings.TrimSpace(reqID) == "" {
		return false
	}
	key := strings.ToLower(strings.TrimSpace(appKey)) + ":" + reqID
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.m[key]; ok && now.Sub(t) < c.ttl {
		c.m[key] = now
		return true
	}
	c.m[key] = now
	// 顺手清理过期项（每次调用 scan 一次，规模小时开销可忽略）
	for k, t := range c.m {
		if now.Sub(t) >= c.ttl {
			delete(c.m, k)
		}
	}
	return false
}
