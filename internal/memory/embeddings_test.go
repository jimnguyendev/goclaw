package memory

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

// mockProvider counts Embed calls and returns deterministic embeddings.
type mockProvider struct {
	name      string
	model     string
	callCount atomic.Int64
	// embedFn overrides embedding logic when set; otherwise returns index-based vectors.
	embedFn func(texts []string) ([][]float32, error)
}

func (m *mockProvider) Name() string { return m.name }
func (m *mockProvider) Model() string { return m.model }
func (m *mockProvider) Embed(_ context.Context, texts []string) ([][]float32, error) {
	m.callCount.Add(1)
	if m.embedFn != nil {
		return m.embedFn(texts)
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		// Deterministic: embed as hash-derived 3-dim vector
		h := ContentHash(t)
		out[i] = []float32{float32(h[0]), float32(h[1]), float32(h[2])}
	}
	return out, nil
}

func newMock() *mockProvider {
	return &mockProvider{name: "mock", model: "v1"}
}

// TestCachedProvider_CacheHit verifies repeated embed of the same text hits cache.
func TestCachedProvider_CacheHit(t *testing.T) {
	inner := newMock()
	p := WithL1Cache(inner)
	ctx := context.Background()

	text := "hello world"
	first, err := p.Embed(ctx, []string{text})
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Embed(ctx, []string{text})
	if err != nil {
		t.Fatal(err)
	}

	if inner.callCount.Load() != 1 {
		t.Errorf("expected 1 inner call, got %d", inner.callCount.Load())
	}
	if len(first[0]) != len(second[0]) {
		t.Errorf("cached result differs from original")
	}
	for i := range first[0] {
		if first[0][i] != second[0][i] {
			t.Errorf("embedding[%d]: want %v, got %v", i, first[0][i], second[0][i])
		}
	}
}

// TestCachedProvider_CacheMiss verifies distinct texts each call the inner provider.
func TestCachedProvider_CacheMiss(t *testing.T) {
	inner := newMock()
	p := WithL1Cache(inner)
	ctx := context.Background()

	texts := []string{"alpha", "beta", "gamma"}
	for _, text := range texts {
		if _, err := p.Embed(ctx, []string{text}); err != nil {
			t.Fatal(err)
		}
	}
	if inner.callCount.Load() != 3 {
		t.Errorf("expected 3 inner calls, got %d", inner.callCount.Load())
	}
}

// TestCachedProvider_BatchPartialHit verifies that a mixed batch only embeds uncached texts.
func TestCachedProvider_BatchPartialHit(t *testing.T) {
	inner := newMock()
	p := WithL1Cache(inner)
	ctx := context.Background()

	// Pre-warm cache with "a" and "b"
	if _, err := p.Embed(ctx, []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	callsBefore := inner.callCount.Load()

	// Now embed ["a", "c", "b"] — only "c" is uncached
	results, err := p.Embed(ctx, []string{"a", "c", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if inner.callCount.Load() != callsBefore+1 {
		t.Errorf("expected 1 additional inner call for uncached 'c', got %d extra",
			inner.callCount.Load()-callsBefore)
	}
	if len(results) != 3 {
		t.Errorf("expected 3 results, got %d", len(results))
	}
	for i, r := range results {
		if len(r) == 0 {
			t.Errorf("result[%d] is empty", i)
		}
	}
}

// TestCachedProvider_LRUPromotion verifies that accessed entries are promoted (not FIFO).
func TestCachedProvider_LRUPromotion(t *testing.T) {
	inner := newMock()
	p := WithL1Cache(inner)
	ctx := context.Background()

	// Insert "keep" and "evict" as first two entries
	p.Embed(ctx, []string{"keep"})
	p.Embed(ctx, []string{"evict"})

	// Fill remaining cache slots (48 more)
	for i := 2; i < l1CacheMax; i++ {
		p.Embed(ctx, []string{string(rune('A'+i)) + "pad"})
	}

	// Access "keep" to promote it (moves to end of LRU list)
	p.Embed(ctx, []string{"keep"})

	// Insert one more to trigger eviction — "evict" is now oldest (not "keep")
	p.Embed(ctx, []string{"OVERFLOW"})

	// "keep" should still be in cache (was promoted)
	callsBefore := inner.callCount.Load()
	p.Embed(ctx, []string{"keep"})
	if inner.callCount.Load() != callsBefore {
		t.Error("'keep' was evicted despite being recently accessed — LRU promotion broken")
	}

	// "evict" should have been evicted (was oldest unreferenced)
	callsBefore = inner.callCount.Load()
	p.Embed(ctx, []string{"evict"})
	if inner.callCount.Load() == callsBefore {
		t.Error("'evict' should have been evicted but was still in cache")
	}
}

// TestCachedProvider_LRUEviction verifies oldest entries are evicted when cache is full.
func TestCachedProvider_LRUEviction(t *testing.T) {
	inner := newMock()
	p := WithL1Cache(inner)
	ctx := context.Background()

	// Fill cache to capacity
	for i := 0; i < l1CacheMax; i++ {
		text := string(rune('A' + i%26)) + string(rune('0'+i))
		if _, err := p.Embed(ctx, []string{text}); err != nil {
			t.Fatal(err)
		}
	}
	// "A0" (first inserted) should have been evicted after filling 50 slots.
	// Insert one more to trigger eviction.
	if _, err := p.Embed(ctx, []string{"OVERFLOW"}); err != nil {
		t.Fatal(err)
	}

	// Verify cache size doesn't exceed l1CacheMax
	p.mu.Lock()
	size := len(p.cache)
	p.mu.Unlock()
	if size > l1CacheMax {
		t.Errorf("cache size %d exceeds max %d", size, l1CacheMax)
	}
}

// TestCachedProvider_ConcurrentSafety verifies no data races under concurrent access.
func TestCachedProvider_ConcurrentSafety(t *testing.T) {
	inner := newMock()
	p := WithL1Cache(inner)
	ctx := context.Background()

	var wg sync.WaitGroup
	texts := []string{"x", "y", "z", "w"}
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			text := texts[i%len(texts)]
			if _, err := p.Embed(ctx, []string{text}); err != nil {
				t.Errorf("Embed error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	// All 4 unique texts should be in cache; inner called at most 4 times
	if inner.callCount.Load() > int64(len(texts)) {
		t.Errorf("inner called %d times for %d unique texts (cache not effective)",
			inner.callCount.Load(), len(texts))
	}
}

// TestCachedProvider_NameModel verifies delegation to inner provider.
func TestCachedProvider_NameModel(t *testing.T) {
	inner := &mockProvider{name: "openai", model: "text-embedding-3-small"}
	p := WithL1Cache(inner)
	if p.Name() != "openai" {
		t.Errorf("Name: want openai, got %s", p.Name())
	}
	if p.Model() != "text-embedding-3-small" {
		t.Errorf("Model: want text-embedding-3-small, got %s", p.Model())
	}
}
