package interceptor

import (
	"container/list"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/zesuy/Plugin-Deepseek-Vision/internal/vision"
)

type analysisCacheEntry struct {
	key       string
	value     string
	expiresAt time.Time
}

// analysisCache is intentionally small and generation-local. It stores only a
// hash key and derived text; image references and bytes are never retained.
type analysisCache struct {
	mu       sync.Mutex
	capacity int
	items    map[string]*list.Element
	lru      *list.List
}

func newAnalysisCache(capacity int) *analysisCache {
	if capacity < 0 {
		capacity = 0
	}
	return &analysisCache{capacity: capacity, items: make(map[string]*list.Element), lru: list.New()}
}

func (c *analysisCache) Get(key string) (string, bool) {
	if c == nil || c.capacity == 0 || key == "" {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.items[key]
	if !ok {
		return "", false
	}
	entry := element.Value.(*analysisCacheEntry)
	if !entry.expiresAt.IsZero() && time.Now().After(entry.expiresAt) {
		delete(c.items, key)
		c.lru.Remove(element)
		return "", false
	}
	c.lru.MoveToFront(element)
	return entry.value, true
}

func (c *analysisCache) Set(key, value string, ttl time.Duration) {
	if c == nil || c.capacity == 0 || key == "" || ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	expiresAt := time.Now().Add(ttl)
	if element, ok := c.items[key]; ok {
		entry := element.Value.(*analysisCacheEntry)
		entry.value = value
		entry.expiresAt = expiresAt
		c.lru.MoveToFront(element)
		return
	}
	element := c.lru.PushFront(&analysisCacheEntry{key: key, value: value, expiresAt: expiresAt})
	c.items[key] = element
	for len(c.items) > c.capacity {
		oldest := c.lru.Back()
		if oldest == nil {
			break
		}
		delete(c.items, oldest.Value.(*analysisCacheEntry).key)
		c.lru.Remove(oldest)
	}
}

func (c *analysisCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

func analysisGroupCacheKey(images []vision.ImageInput, model, language, prompt string, dataTTL, urlTTL time.Duration) (string, time.Duration) {
	ttl := dataTTL
	parts := []string{"prompt-group-v1", strings.TrimSpace(model), vision.NormalizeLanguage(language), vision.BuildPrompt(prompt, language)}
	for i := range images {
		parts = append(parts, images[i].Reference)
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(images[i].Reference)), "data:") {
			ttl = urlTTL
		}
	}
	hash := sha256.New()
	for _, part := range parts {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(part))
	}
	return hex.EncodeToString(hash.Sum(nil)), ttl
}
