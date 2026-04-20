package proxy

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const cacheTTL = 5 * time.Minute

// CacheStore 缓存存储接口
type CacheStore interface {
	// Get 获取缓存值，不存在返回 0, false
	Get(key string) (int, bool)
	// Set 设置缓存值，带 TTL
	Set(key string, value int)
}

// ==================== Redis 实现 ====================

type RedisCacheStore struct {
	client *redis.Client
}

func NewRedisCacheStore(redisURL string) (*RedisCacheStore, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, err
	}
	opts.ReadTimeout = 2 * time.Second
	opts.WriteTimeout = 2 * time.Second
	client := redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, err
	}

	log.Printf("[Cache] Redis 连接成功: %s", redisURL)
	return &RedisCacheStore{client: client}, nil
}

func (r *RedisCacheStore) Get(key string) (int, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	val, err := r.client.Get(ctx, "kiro:cache:"+key).Int()
	if err != nil {
		return 0, false
	}
	return val, true
}

func (r *RedisCacheStore) Set(key string, value int) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r.client.Set(ctx, "kiro:cache:"+key, value, cacheTTL)
}

// ==================== 内存实现 ====================

type MemoryCacheStore struct {
	mu     sync.RWMutex
	items  map[string]*cacheItem
	stopCh chan struct{}
}

type cacheItem struct {
	value     int
	expiresAt time.Time
}

func NewMemoryCacheStore() *MemoryCacheStore {
	m := &MemoryCacheStore{
		items:  make(map[string]*cacheItem),
		stopCh: make(chan struct{}),
	}
	// 定期清理过期条目
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-m.stopCh:
				return
			case <-ticker.C:
				m.cleanup()
			}
		}
	}()
	return m
}

// Stop 停止清理 goroutine
func (m *MemoryCacheStore) Stop() {
	close(m.stopCh)
}

func (m *MemoryCacheStore) Get(key string) (int, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	item, ok := m.items[key]
	if !ok || time.Now().After(item.expiresAt) {
		return 0, false
	}
	return item.value, true
}

func (m *MemoryCacheStore) Set(key string, value int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[key] = &cacheItem{
		value:     value,
		expiresAt: time.Now().Add(cacheTTL),
	}
}

func (m *MemoryCacheStore) cleanup() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for k, v := range m.items {
		if now.After(v.expiresAt) {
			delete(m.items, k)
		}
	}
}
