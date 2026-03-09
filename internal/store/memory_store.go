package store

import (
	"context"
	"time"
)

// DocumentInfo describes a memory document.
type DocumentInfo struct {
	Path      string `json:"path"`
	Hash      string `json:"hash"`
	AgentID   string `json:"agent_id,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	UpdatedAt int64  `json:"updated_at"`
}

// DocumentDetail provides full document info including chunk/embedding stats.
type DocumentDetail struct {
	Path          string `json:"path"`
	Content       string `json:"content"`
	Hash          string `json:"hash"`
	UserID        string `json:"user_id,omitempty"`
	ChunkCount    int    `json:"chunk_count"`
	EmbeddedCount int    `json:"embedded_count"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

// ChunkInfo describes a single memory chunk.
type ChunkInfo struct {
	ID           string `json:"id"`
	StartLine    int    `json:"start_line"`
	EndLine      int    `json:"end_line"`
	TextPreview  string `json:"text_preview"`
	HasEmbedding bool   `json:"has_embedding"`
}

// MemorySearchResult is a single result from memory search.
type MemorySearchResult struct {
	Path      string   `json:"path"`
	StartLine int      `json:"start_line"`
	EndLine   int      `json:"end_line"`
	Score     float64  `json:"score"`
	Snippet   string   `json:"snippet"`
	Source    string   `json:"source"`
	Scope     string   `json:"scope,omitempty"`   // "global" or "personal"
	Sources   []string `json:"sources,omitempty"` // backends that contributed: "fts", "vector", "graph"
}

// MemorySearchOptions configures a memory search query.
type MemorySearchOptions struct {
	MaxResults int
	MinScore   float64
	Source     string // "memory", "sessions", ""
	PathPrefix string
}

// EmbeddingProvider generates vector embeddings for text.
type EmbeddingProvider interface {
	Name() string
	Model() string
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// ─── Knowledge Graph types ────────────────────────────────────────────────────

// KGEntity is a named entity to persist in the knowledge graph.
type KGEntity struct {
	Name     string   `json:"name"`
	NodeType string   `json:"node_type,omitempty"` // defaults to "entity"
	Aliases  []string `json:"aliases,omitempty"`
}

// KGRelation is a directed relationship between two named entities.
type KGRelation struct {
	From       string     `json:"from"`
	To         string     `json:"to"`
	Relation   string     `json:"relation"`
	Weight     float64    `json:"weight,omitempty"`
	ValidFrom  *time.Time `json:"valid_from,omitempty"`
	ValidUntil *time.Time `json:"valid_until,omitempty"`
}

// KGNodeInfo is a lightweight node summary used in stats.
type KGNodeInfo struct {
	Name   string `json:"name"`
	Degree int    `json:"degree"`
}

// KGStatsResult holds knowledge graph statistics.
type KGStatsResult struct {
	NodeCount int64        `json:"node_count"`
	EdgeCount int64        `json:"edge_count"`
	TopNodes  []KGNodeInfo `json:"top_nodes"`
}

// ─── Search config ────────────────────────────────────────────────────────────

// MemorySearchConfig holds runtime-tunable parameters for the tri-hybrid
// search pipeline (RRF + temporal decay + MMR).
type MemorySearchConfig struct {
	// RRF constant k (default 60). Higher = less rank-sensitivity.
	RRFk int `json:"rrf_k"`
	// Temporal decay half-life in days (default 30). Evergreen chunks skip decay.
	DecayHalfLifeDays float64 `json:"decay_half_life"`
	// Each access event slows decay by this fraction of the half-life (default 0.1).
	DecayAccessFactor float64 `json:"decay_access_factor"`
	// MMR lambda: 1.0 = pure relevance, 0.0 = pure diversity (default 0.7).
	MMRLambda float64 `json:"mmr_lambda"`
}

// DefaultMemorySearchConfig returns sensible defaults.
func DefaultMemorySearchConfig() MemorySearchConfig {
	return MemorySearchConfig{
		RRFk:              60,
		DecayHalfLifeDays: 30.0,
		DecayAccessFactor: 0.1,
		MMRLambda:         0.7,
	}
}

// ─── Store interface ──────────────────────────────────────────────────────────

// MemoryStore manages memory documents, search, and the knowledge graph.
type MemoryStore interface {
	// Document CRUD
	GetDocument(ctx context.Context, agentID, userID, path string) (string, error)
	PutDocument(ctx context.Context, agentID, userID, path, content string) error
	DeleteDocument(ctx context.Context, agentID, userID, path string) error
	ListDocuments(ctx context.Context, agentID, userID string) ([]DocumentInfo, error)

	// Admin queries
	ListAllDocumentsGlobal(ctx context.Context) ([]DocumentInfo, error)
	ListAllDocuments(ctx context.Context, agentID string) ([]DocumentInfo, error)
	GetDocumentDetail(ctx context.Context, agentID, userID, path string) (*DocumentDetail, error)
	ListChunks(ctx context.Context, agentID, userID, path string) ([]ChunkInfo, error)

	// Search (tri-hybrid: FTS + vector + knowledge-graph → RRF → decay → MMR)
	Search(ctx context.Context, query string, agentID, userID string, opts MemorySearchOptions) ([]MemorySearchResult, error)

	// Indexing
	IndexDocument(ctx context.Context, agentID, userID, path string) error
	IndexAll(ctx context.Context, agentID, userID string) error

	// Knowledge Graph (agent-explicit; no hidden LLM extraction)
	KGIndexEntities(ctx context.Context, agentID, userID string, entities []KGEntity, relations []KGRelation) error
	KGStats(ctx context.Context, agentID string) (*KGStatsResult, error)

	// Runtime search config
	GetSearchConfig(ctx context.Context, agentID string) (MemorySearchConfig, error)
	SetSearchConfig(ctx context.Context, agentID string, updates map[string]float64) error

	SetEmbeddingProvider(provider EmbeddingProvider)
	Close() error
}
