package memory

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nextlevelbuilder/goclaw/internal/bootstrap"
)

// ManagerConfig configures the memory manager.
type ManagerConfig struct {
	DBPath       string // path to SQLite database file
	MemoryDir    string // directory containing memory files (MEMORY.md, memory/*.md)
	MaxChunkLen  int    // max characters per chunk (default 1000)
	MaxResults   int    // default max search results (default 6)
	VectorWeight float64
	TextWeight   float64
}

// DefaultManagerConfig returns sensible defaults.
func DefaultManagerConfig(workspace string) ManagerConfig {
	return ManagerConfig{
		DBPath:       filepath.Join(workspace, "memory.db"),
		MemoryDir:    workspace,
		MaxChunkLen:  1000,
		MaxResults:   6,
		VectorWeight: 0.7,
		TextWeight:   0.3,
	}
}

const l1CacheMax = 50

// Manager coordinates memory indexing and search.
type Manager struct {
	store    *SQLiteStore
	provider EmbeddingProvider
	config   ManagerConfig
	mu       sync.RWMutex
	watcher  *Watcher
	// l1Cache is an in-memory LRU cache for recently used embeddings (query + chunk).
	// Keyed by content hash. Evicts oldest entry when full.
	l1Cache    map[string][]float32
	l1Keys     []string // insertion-order keys for LRU eviction
}

// NewManager creates a memory manager.
func NewManager(cfg ManagerConfig) (*Manager, error) {
	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	store, err := NewSQLiteStore(cfg.DBPath)
	if err != nil {
		return nil, err
	}

	return &Manager{
		store:   store,
		config:  cfg,
		l1Cache: make(map[string][]float32),
	}, nil
}

// SetEmbeddingProvider sets the embedding provider for vector search.
// If not set, only FTS search is available.
func (m *Manager) SetEmbeddingProvider(provider EmbeddingProvider) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.provider = provider
}

// IndexFile reads a file, chunks it, and stores the chunks in the database.
// Skips unchanged files (by content hash, matching TS manager-sync-ops.ts)
// and optionally generates embeddings.
func (m *Manager) IndexFile(ctx context.Context, path string) error {
	absPath := path
	if !filepath.IsAbs(path) {
		absPath = filepath.Join(m.config.MemoryDir, path)
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	text := string(data)
	relPath := path
	if filepath.IsAbs(relPath) {
		if rel, err := filepath.Rel(m.config.MemoryDir, relPath); err == nil {
			relPath = rel
		}
	}

	// Hash-based change detection (matching TS: compare SHA256 hash, skip if unchanged)
	fileHash := ContentHash(text)
	if existingHash, ok := m.store.GetFileHash(relPath); ok && existingHash == fileHash {
		slog.Debug("skipping unchanged file", "path", relPath)
		return nil
	}

	chunks := ChunkText(text, m.config.MaxChunkLen)

	// Delete old chunks for this path
	if err := m.store.DeleteByPath(relPath); err != nil {
		return fmt.Errorf("delete old chunks for %s: %w", relPath, err)
	}

	// Collect texts for batch embedding
	var textsToEmbed []string
	var chunkEntries []Chunk

	for i, tc := range chunks {
		hash := ContentHash(tc.Text)
		id := fmt.Sprintf("%s#%d", relPath, i)

		entry := Chunk{
			ID:        id,
			Path:      relPath,
			Source:    "memory",
			StartLine: tc.StartLine,
			EndLine:   tc.EndLine,
			Hash:      hash,
			Text:      tc.Text,
		}

		chunkEntries = append(chunkEntries, entry)
		textsToEmbed = append(textsToEmbed, tc.Text)
	}

	// Generate embeddings if provider is available.
	// Check L1 (in-memory) then L2 (SQLite cache) before calling the API,
	// so unchanged chunks don't trigger an API round-trip on every re-index.
	m.mu.RLock()
	provider := m.provider
	m.mu.RUnlock()

	if provider != nil && len(textsToEmbed) > 0 {
		// Separate cached chunks from those that need embedding.
		type uncachedEntry struct {
			chunkIdx int
			text     string
			hash     string
		}
		var uncached []uncachedEntry

		for i, entry := range chunkEntries {
			// L1 check
			if emb := m.l1Get(entry.Hash); emb != nil {
				chunkEntries[i].Embedding = emb
				chunkEntries[i].Model = provider.Model()
				continue
			}
			// L2 check
			if emb, ok := m.store.GetCachedEmbedding(entry.Hash, provider.Name(), provider.Model()); ok {
				chunkEntries[i].Embedding = emb
				chunkEntries[i].Model = provider.Model()
				m.l1Set(entry.Hash, emb)
				continue
			}
			uncached = append(uncached, uncachedEntry{chunkIdx: i, text: textsToEmbed[i], hash: entry.Hash})
		}

		if len(uncached) > 0 {
			texts := make([]string, len(uncached))
			for i, u := range uncached {
				texts[i] = u.text
			}
			embeddings, err := provider.Embed(ctx, texts)
			if err != nil {
				slog.Warn("embedding generation failed, indexing without vectors", "path", relPath, "error", err)
			} else {
				for i, u := range uncached {
					if i < len(embeddings) {
						chunkEntries[u.chunkIdx].Embedding = embeddings[i]
						chunkEntries[u.chunkIdx].Model = provider.Model()
						m.l1Set(u.hash, embeddings[i])
						m.store.CacheEmbedding(u.hash, provider.Name(), provider.Model(), embeddings[i])
					}
				}
			}
		}
	}

	// Store chunks
	for _, entry := range chunkEntries {
		if err := m.store.UpsertChunk(entry); err != nil {
			slog.Warn("failed to store chunk", "id", entry.ID, "error", err)
		}
	}

	// Update file metadata for future change detection
	var mtime int64
	var size int64
	if info, err := os.Stat(absPath); err == nil {
		mtime = info.ModTime().UnixMilli()
		size = info.Size()
	}
	m.store.UpsertFile(relPath, "memory", fileHash, mtime, size)

	slog.Debug("indexed file", "path", relPath, "chunks", len(chunkEntries))
	return nil
}

// IndexAll scans the memory directory for .md files and indexes them all.
func (m *Manager) IndexAll(ctx context.Context) error {
	memoryDir := m.config.MemoryDir

	// Index MEMORY.md at root
	memoryFile := filepath.Join(memoryDir, bootstrap.MemoryFile)
	if _, err := os.Stat(memoryFile); err == nil {
		if err := m.IndexFile(ctx, memoryFile); err != nil {
			slog.Warn("failed to index MEMORY.md", "error", err)
		}
	}

	// Index memory/*.md
	memSubDir := filepath.Join(memoryDir, "memory")
	if _, err := os.Stat(memSubDir); err == nil {
		filepath.Walk(memSubDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if strings.HasSuffix(path, ".md") {
				if err := m.IndexFile(ctx, path); err != nil {
					slog.Warn("failed to index file", "path", path, "error", err)
				}
			}
			return nil
		})
	}

	slog.Info("memory indexing complete", "chunks", m.store.ChunkCount())
	return nil
}

// l1Get returns a cached embedding from the in-memory L1 cache (thread-safe).
func (m *Manager) l1Get(hash string) []float32 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.l1Cache[hash]
}

// l1Set stores an embedding in the in-memory L1 cache, evicting the oldest entry if full.
func (m *Manager) l1Set(hash string, emb []float32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.l1Cache[hash]; !exists {
		if len(m.l1Keys) >= l1CacheMax {
			oldest := m.l1Keys[0]
			m.l1Keys = m.l1Keys[1:]
			delete(m.l1Cache, oldest)
		}
		m.l1Keys = append(m.l1Keys, hash)
	}
	m.l1Cache[hash] = emb
}

// Search performs a hybrid search over indexed memory.
func (m *Manager) Search(ctx context.Context, query string, opts SearchOptions) ([]SearchResult, error) {
	if opts.MaxResults <= 0 {
		opts.MaxResults = m.config.MaxResults
	}

	m.mu.RLock()
	provider := m.provider
	m.mu.RUnlock()

	hybridCfg := HybridSearchConfig{
		VectorWeight: m.config.VectorWeight,
		TextWeight:   m.config.TextWeight,
	}
	if hybridCfg.VectorWeight == 0 && hybridCfg.TextWeight == 0 {
		hybridCfg = DefaultHybridConfig()
	}

	return HybridSearch(ctx, m.store, provider, query, opts, hybridCfg)
}

// GetFile reads a memory file (or a section of it) and returns the content.
func (m *Manager) GetFile(path string, fromLine, numLines int) (string, error) {
	absPath := path
	if !filepath.IsAbs(path) {
		absPath = filepath.Join(m.config.MemoryDir, path)
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}

	text := string(data)

	if fromLine <= 0 && numLines <= 0 {
		return text, nil
	}

	lines := strings.Split(text, "\n")

	start := fromLine - 1 // 1-indexed to 0-indexed
	if start < 0 {
		start = 0
	}
	if start >= len(lines) {
		return "", nil
	}

	end := len(lines)
	if numLines > 0 {
		end = start + numLines
		if end > len(lines) {
			end = len(lines)
		}
	}

	return strings.Join(lines[start:end], "\n"), nil
}

// ChunkCount returns the number of indexed chunks.
func (m *Manager) ChunkCount() int {
	return m.store.ChunkCount()
}

// StartWatcher starts a file watcher that monitors memory files for changes
// and re-indexes them automatically with debounce (matching TS chokidar pattern).
func (m *Manager) StartWatcher(ctx context.Context) error {
	w, err := newWatcher(m)
	if err != nil {
		return fmt.Errorf("create watcher: %w", err)
	}
	if err := w.start(ctx); err != nil {
		return fmt.Errorf("start watcher: %w", err)
	}
	m.watcher = w
	return nil
}

// StopWatcher stops the file watcher if running.
func (m *Manager) StopWatcher() {
	if m.watcher != nil {
		m.watcher.stop()
		m.watcher = nil
	}
}

// Close shuts down the memory manager (including watcher).
func (m *Manager) Close() error {
	m.StopWatcher()
	return m.store.Close()
}
