package interceptor

import (
	"strings"
	"testing"
	"time"

	"github.com/zesuy/Plugin-Deepseek-Vision/internal/vision"
)

func TestAnalysisCacheKeyUsesContentAndPromptWithoutRetainingReference(t *testing.T) {
	reference := "data:image/png;base64,AAAA"
	dataTTL := 7 * time.Minute
	urlTTL := 30 * time.Second
	images := []vision.ImageInput{{Number: 1, Reference: reference}}
	first, firstTTL := analysisGroupCacheKey(images, "vision-model", "zh-CN", "focus", dataTTL, urlTTL)
	second, secondTTL := analysisGroupCacheKey(images, "vision-model", "zh", "focus", dataTTL, urlTTL)
	if first != second || firstTTL != dataTTL || secondTTL != dataTTL {
		t.Fatalf("equivalent keys differ: %q/%s %q/%s", first, firstTTL, second, secondTTL)
	}
	if strings.Contains(first, reference) || len(first) != 64 {
		t.Fatalf("cache key retained reference or is not SHA-256: %q", first)
	}
	changedFocus, _ := analysisGroupCacheKey(images, "vision-model", "zh", "other", dataTTL, urlTTL)
	changedModel, _ := analysisGroupCacheKey(images, "other-model", "zh", "focus", dataTTL, urlTTL)
	if changedFocus == first || changedModel == first {
		t.Fatal("cache key ignored prompt or model")
	}
	ordered := []vision.ImageInput{{Number: 1, Reference: reference}, {Number: 2, Reference: "https://example.com/second.png"}}
	reversed := []vision.ImageInput{{Number: 2, Reference: "https://example.com/second.png"}, {Number: 1, Reference: reference}}
	orderedKey, _ := analysisGroupCacheKey(ordered, "vision-model", "zh", "focus", dataTTL, urlTTL)
	reversedKey, _ := analysisGroupCacheKey(reversed, "vision-model", "zh", "focus", dataTTL, urlTTL)
	if orderedKey == reversedKey {
		t.Fatal("cache key ignored image order")
	}
	shifted := []vision.ImageInput{{Number: 11, Reference: reference}, {Number: 12, Reference: "https://example.com/second.png"}}
	shiftedKey, _ := analysisGroupCacheKey(shifted, "vision-model", "zh", "focus", dataTTL, urlTTL)
	if shiftedKey != orderedKey {
		t.Fatal("cache key incorrectly depends on traversal-global image numbers")
	}
	_, gotURLTTL := analysisGroupCacheKey([]vision.ImageInput{{Number: 1, Reference: "https://example.com/image.png"}}, "vision-model", "zh", "focus", dataTTL, urlTTL)
	if gotURLTTL != urlTTL || gotURLTTL >= dataTTL {
		t.Fatalf("URL TTL = %s, data TTL = %s", gotURLTTL, dataTTL)
	}
}

func TestAnalysisCacheExpiresAndEvicts(t *testing.T) {
	cache := newAnalysisCache(2)
	cache.Set("one", "1", time.Minute)
	cache.Set("two", "2", time.Minute)
	if value, ok := cache.Get("one"); !ok || value != "1" {
		t.Fatalf("cache get = %q, %v", value, ok)
	}
	cache.Set("three", "3", time.Minute)
	if _, ok := cache.Get("two"); ok {
		t.Fatal("least-recently-used entry was not evicted")
	}
	if cache.Len() != 2 {
		t.Fatalf("cache length = %d", cache.Len())
	}
	cache.Set("short", "value", 5*time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	if _, ok := cache.Get("short"); ok {
		t.Fatal("expired entry was returned")
	}
}
