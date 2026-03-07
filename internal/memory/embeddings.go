package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
)

// ContentHash returns a short SHA256 hex digest of the content (first 16 bytes).
func ContentHash(text string) string {
	h := sha256.Sum256([]byte(text))
	return fmt.Sprintf("%x", h[:16])
}

// TextChunk is a chunk of text with line number metadata.
type TextChunk struct {
	Text      string
	StartLine int
	EndLine   int
}

// ChunkText splits text into chunks at paragraph boundaries.
// Each chunk includes its starting line number in the source file.
func ChunkText(text string, maxChunkLen int) []TextChunk {
	if maxChunkLen <= 0 {
		maxChunkLen = 1000
	}

	lines := strings.Split(text, "\n")
	var chunks []TextChunk
	var current strings.Builder
	startLine := 1

	flush := func(endLine int) {
		content := strings.TrimSpace(current.String())
		if content != "" {
			chunks = append(chunks, TextChunk{
				Text:      content,
				StartLine: startLine,
				EndLine:   endLine,
			})
		}
		current.Reset()
		startLine = endLine + 1
	}

	for i, line := range lines {
		lineNum := i + 1

		// Paragraph boundary: empty line
		if strings.TrimSpace(line) == "" && current.Len() > 0 {
			if current.Len() >= maxChunkLen/2 {
				flush(lineNum - 1)
				continue
			}
		}

		if current.Len() > 0 {
			current.WriteString("\n")
		}
		current.WriteString(line)

		// Force flush if too large
		if current.Len() >= maxChunkLen {
			flush(lineNum)
		}
	}

	if current.Len() > 0 {
		flush(len(lines))
	}

	return chunks
}

// EmbeddingProvider generates vector embeddings for text.
type EmbeddingProvider interface {
	// Name returns the provider identifier (e.g., "openai", "voyage").
	Name() string

	// Model returns the model used for embeddings.
	Model() string

	// Embed generates embeddings for a batch of texts.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// OpenAIEmbeddingProvider uses the OpenAI-compatible embedding API.
// Works with OpenAI, OpenRouter, and any compatible endpoint.
type OpenAIEmbeddingProvider struct {
	name       string
	model      string
	apiKey     string
	apiURL     string
	dimensions int // optional: truncate output to this many dimensions (0 = use model default)
}

// NewOpenAIEmbeddingProvider creates a provider for OpenAI-compatible embedding APIs.
func NewOpenAIEmbeddingProvider(name, apiKey, apiURL, model string) *OpenAIEmbeddingProvider {
	if apiURL == "" {
		apiURL = "https://api.openai.com/v1"
	}
	if model == "" {
		model = "text-embedding-3-small"
	}

	return &OpenAIEmbeddingProvider{
		name:   name,
		model:  model,
		apiKey: apiKey,
		apiURL: apiURL,
	}
}

// WithDimensions sets the output dimensions for models that support dimension truncation.
func (p *OpenAIEmbeddingProvider) WithDimensions(d int) *OpenAIEmbeddingProvider {
	p.dimensions = d
	return p
}

func (p *OpenAIEmbeddingProvider) Name() string  { return p.name }
func (p *OpenAIEmbeddingProvider) Model() string { return p.model }

func (p *OpenAIEmbeddingProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	reqBody := map[string]interface{}{
		"input": texts,
		"model": p.model,
	}
	if p.dimensions > 0 {
		reqBody["dimensions"] = p.dimensions
	}

	bodyJSON, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiURL+"/embeddings", bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embedding API error %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	embeddings := make([][]float32, len(result.Data))
	for i, d := range result.Data {
		embeddings[i] = d.Embedding
	}

	return embeddings, nil
}

// CachedEmbeddingProvider wraps any EmbeddingProvider with an in-memory LRU cache.
// Keyed by SHA256 content hash. Evicts the oldest entry when the cache is full.
// This reduces redundant API calls when the same text is embedded multiple times
// (e.g., re-indexing unchanged chunks or repeating the same search query).
const l1CacheMax = 50

type CachedEmbeddingProvider struct {
	inner  EmbeddingProvider
	mu     sync.Mutex
	cache  map[string][]float32
	keys   []string // insertion-order for LRU eviction
}

// WithL1Cache wraps provider with an in-memory LRU embedding cache.
func WithL1Cache(provider EmbeddingProvider) *CachedEmbeddingProvider {
	return &CachedEmbeddingProvider{
		inner: provider,
		cache: make(map[string][]float32),
	}
}

func (c *CachedEmbeddingProvider) Name() string  { return c.inner.Name() }
func (c *CachedEmbeddingProvider) Model() string { return c.inner.Model() }

func (c *CachedEmbeddingProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	results := make([][]float32, len(texts))

	// Separate cached from uncached
	type miss struct {
		idx  int
		text string
	}
	var misses []miss
	for i, t := range texts {
		h := ContentHash(t)
		c.mu.Lock()
		if emb, ok := c.cache[h]; ok {
			results[i] = emb
			c.mu.Unlock()
		} else {
			c.mu.Unlock()
			misses = append(misses, miss{i, t})
		}
	}

	if len(misses) == 0 {
		return results, nil
	}

	// Batch embed uncached texts
	batchTexts := make([]string, len(misses))
	for i, m := range misses {
		batchTexts[i] = m.text
	}
	embeddings, err := c.inner.Embed(ctx, batchTexts)
	if err != nil {
		return nil, err
	}
	for i, m := range misses {
		if i < len(embeddings) {
			results[m.idx] = embeddings[i]
			c.set(ContentHash(m.text), embeddings[i])
		}
	}
	return results, nil
}

func (c *CachedEmbeddingProvider) set(hash string, emb []float32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.cache[hash]; !exists {
		if len(c.keys) >= l1CacheMax {
			oldest := c.keys[0]
			c.keys = c.keys[1:]
			delete(c.cache, oldest)
		}
		c.keys = append(c.keys, hash)
	}
	c.cache[hash] = emb
}

// CosineSimilarity computes the cosine similarity between two vectors.
// Returns a value between -1 and 1 (1 = identical).
func CosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}

	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}

	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}

	return dot / denom
}
