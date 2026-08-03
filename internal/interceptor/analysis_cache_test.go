package interceptor

import (
	"strings"
	"testing"
	"time"
)

func TestAnalysisCacheKeyUsesContentAndPromptWithoutRetainingReference(t *testing.T) {
	reference := "data:image/png;base64,AAAA"
	dataTTL := 7 * time.Minute
	urlTTL := 30 * time.Second
	first, firstTTL := analysisCacheKey(reference, "vision-model", "zh-CN", "focus", dataTTL, urlTTL)
	second, secondTTL := analysisCacheKey(reference, "vision-model", "zh", "focus", dataTTL, urlTTL)
	if first != second || firstTTL != dataTTL || secondTTL != dataTTL {
		t.Fatalf("equivalent keys differ: %q/%s %q/%s", first, firstTTL, second, secondTTL)
	}
	if strings.Contains(first, reference) || len(first) != 64 {
		t.Fatalf("cache key retained reference or is not SHA-256: %q", first)
	}
	changedFocus, _ := analysisCacheKey(reference, "vision-model", "zh", "other", dataTTL, urlTTL)
	changedModel, _ := analysisCacheKey(reference, "other-model", "zh", "focus", dataTTL, urlTTL)
	if changedFocus == first || changedModel == first {
		t.Fatal("cache key ignored prompt or model")
	}
	_, gotURLTTL := analysisCacheKey("https://example.com/image.png", "vision-model", "zh", "focus", dataTTL, urlTTL)
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
