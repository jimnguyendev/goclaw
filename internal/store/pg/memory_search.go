package pg

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// Search performs tri-hybrid search: FTS + vector + knowledge-graph BFS.
// Pipeline: parallel retrieval → RRF fusion → temporal decay → MMR re-ranking → trackAccess.
func (s *PGMemoryStore) Search(ctx context.Context, query string, agentID, userID string, opts store.MemorySearchOptions) ([]store.MemorySearchResult, error) {
	maxResults := opts.MaxResults
	if maxResults <= 0 {
		maxResults = s.cfg.MaxResults
	}

	aid := mustParseUUID(agentID)

	cfg, cfgErr := s.GetSearchConfig(ctx, agentID)
	if cfgErr != nil {
		slog.Warn("memory.search.config_failed", "error", cfgErr, "agent_id", agentID)
	}

	fetchN := maxResults * 3

	// Parallel retrieval: FTS + vector + graph
	var (
		ftsResults, vecResults, graphResults []scoredChunk
		wg                                  sync.WaitGroup
	)

	wg.Add(3)

	go func() {
		defer wg.Done()
		var err error
		ftsResults, err = s.ftsSearch(ctx, query, aid, userID, fetchN)
		if err != nil {
			slog.Warn("memory.search.fts_failed", "error", err, "agent_id", agentID)
		}
		if len(ftsResults) == 0 {
			ftsResults, err = s.likeSearch(ctx, query, aid, userID, fetchN)
			if err != nil {
				slog.Warn("memory.search.like_failed", "error", err, "agent_id", agentID)
			}
		}
	}()

	go func() {
		defer wg.Done()
		if s.provider != nil {
			embeddings, err := s.provider.Embed(ctx, []string{query})
			if err != nil {
				slog.Warn("memory.search.embed_failed", "error", err, "agent_id", agentID)
				return
			}
			if len(embeddings) > 0 {
				var vErr error
				vecResults, vErr = s.vectorSearch(ctx, embeddings[0], aid, userID, fetchN)
				if vErr != nil {
					slog.Warn("memory.search.vector_failed", "error", vErr, "agent_id", agentID)
				}
			}
		}
	}()

	go func() {
		defer wg.Done()
		var err error
		graphResults, err = s.graphSearch(ctx, query, aid, userID, fetchN)
		if err != nil {
			slog.Warn("memory.search.graph_failed", "error", err, "agent_id", agentID)
		}
	}()

	wg.Wait()

	// RRF fusion
	merged := rrfMerge(ftsResults, vecResults, graphResults, cfg.RRFk)

	// Temporal decay + access boost
	applyTemporalDecay(merged, cfg)

	// Sort by score descending
	for i := 0; i < len(merged); i++ {
		for j := i + 1; j < len(merged); j++ {
			if merged[j].Score > merged[i].Score {
				merged[i], merged[j] = merged[j], merged[i]
			}
		}
	}

	// MMR re-ranking for diversity
	reranked := mmrRerank(merged, cfg.MMRLambda, maxResults*2)

	// Async access tracking
	go s.trackAccess(context.Background(), reranked)

	// Build output, apply filters
	var out []store.MemorySearchResult
	for _, c := range reranked {
		if opts.MinScore > 0 && c.Score < opts.MinScore {
			continue
		}
		if opts.PathPrefix != "" && !strings.HasPrefix(c.Path, opts.PathPrefix) {
			continue
		}
		scope := "global"
		if c.UserID != nil && *c.UserID != "" {
			scope = "personal"
		}
		out = append(out, store.MemorySearchResult{
			Path:      c.Path,
			StartLine: c.StartLine,
			EndLine:   c.EndLine,
			Score:     c.Score,
			Snippet:   c.Text,
			Source:    "memory",
			Scope:     scope,
			Sources:   c.Sources,
		})
		if len(out) >= maxResults {
			break
		}
	}

	return out, nil
}

// scoredChunk is an internal result with temporal metadata for decay scoring.
type scoredChunk struct {
	ID          string
	Path        string
	StartLine   int
	EndLine     int
	Text        string
	Score       float64
	UserID      *string
	AccessedAt  *time.Time
	AccessCount int
	IsEvergreen bool
	// Sources tracks which backends contributed this chunk: "fts", "vector", "graph".
	Sources []string
}

func (s *PGMemoryStore) ftsSearch(ctx context.Context, query string, agentID uuid.UUID, userID string, limit int) ([]scoredChunk, error) {
	var q string
	var args []interface{}

	cols := `id::text, path, start_line, end_line, text, user_id, accessed_at, access_count, is_evergreen,
			ts_rank(tsv, plainto_tsquery('simple', $1)) AS score`

	if userID != "" {
		q = fmt.Sprintf(`SELECT %s FROM memory_chunks
			WHERE agent_id = $2 AND tsv @@ plainto_tsquery('simple', $3)
			AND (user_id IS NULL OR user_id = $4)
			ORDER BY score DESC LIMIT $5`, cols)
		args = []interface{}{query, agentID, query, userID, limit}
	} else {
		q = fmt.Sprintf(`SELECT %s FROM memory_chunks
			WHERE agent_id = $2 AND tsv @@ plainto_tsquery('simple', $3)
			AND user_id IS NULL
			ORDER BY score DESC LIMIT $4`, cols)
		args = []interface{}{query, agentID, query, limit}
	}

	return s.scanChunks(ctx, q, args)
}

func (s *PGMemoryStore) vectorSearch(ctx context.Context, embedding []float32, agentID uuid.UUID, userID string, limit int) ([]scoredChunk, error) {
	vecStr := vectorToString(embedding)

	cols := `id::text, path, start_line, end_line, text, user_id, accessed_at, access_count, is_evergreen,
			1 - (embedding <=> $1::vector) AS score`

	var q string
	var args []interface{}

	if userID != "" {
		q = fmt.Sprintf(`SELECT %s FROM memory_chunks
			WHERE agent_id = $2 AND embedding IS NOT NULL
			AND (user_id IS NULL OR user_id = $3)
			ORDER BY embedding <=> $4::vector LIMIT $5`, cols)
		args = []interface{}{vecStr, agentID, userID, vecStr, limit}
	} else {
		q = fmt.Sprintf(`SELECT %s FROM memory_chunks
			WHERE agent_id = $2 AND embedding IS NOT NULL
			AND user_id IS NULL
			ORDER BY embedding <=> $3::vector LIMIT $4`, cols)
		args = []interface{}{vecStr, agentID, vecStr, limit}
	}

	return s.scanChunks(ctx, q, args)
}

// likeSearch is a fallback when FTS returns nothing (e.g., cross-language query).
func (s *PGMemoryStore) likeSearch(ctx context.Context, query string, agentID uuid.UUID, userID string, limit int) ([]scoredChunk, error) {
	words := strings.Fields(query)
	if len(words) == 0 {
		return nil, nil
	}

	const maxKeywords = 5
	const minKeywordLen = 3
	var filtered []string
	for _, w := range words {
		if len(w) >= minKeywordLen {
			filtered = append(filtered, w)
		}
	}
	if len(filtered) == 0 {
		return nil, nil
	}
	for i := 0; i < len(filtered); i++ {
		for j := i + 1; j < len(filtered); j++ {
			if len(filtered[j]) > len(filtered[i]) {
				filtered[i], filtered[j] = filtered[j], filtered[i]
			}
		}
	}
	if len(filtered) > maxKeywords {
		filtered = filtered[:maxKeywords]
	}

	args := []interface{}{agentID}
	var conditions []string
	for _, w := range filtered {
		args = append(args, "%"+w+"%")
		conditions = append(conditions, fmt.Sprintf("text ILIKE $%d", len(args)))
	}

	q := fmt.Sprintf(`SELECT id::text, path, start_line, end_line, text, user_id,
		accessed_at, access_count, is_evergreen, 0.5 AS score
		FROM memory_chunks
		WHERE agent_id = $1 AND (%s)`, strings.Join(conditions, " OR "))

	if userID != "" {
		args = append(args, userID)
		q += fmt.Sprintf(" AND (user_id IS NULL OR user_id = $%d)", len(args))
	} else {
		q += " AND user_id IS NULL"
	}
	args = append(args, limit)
	q += fmt.Sprintf(" LIMIT $%d", len(args))

	return s.scanChunks(ctx, q, args)
}

// scanChunks executes a query and scans into []scoredChunk.
func (s *PGMemoryStore) scanChunks(ctx context.Context, q string, args []interface{}) ([]scoredChunk, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []scoredChunk
	for rows.Next() {
		var r scoredChunk
		if err := rows.Scan(&r.ID, &r.Path, &r.StartLine, &r.EndLine, &r.Text,
			&r.UserID, &r.AccessedAt, &r.AccessCount, &r.IsEvergreen, &r.Score); err != nil {
			slog.Warn("memory.search.scan_failed", "error", err)
			continue
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// rrfMerge combines three ranked lists via Reciprocal Rank Fusion.
// score(d) = Σ 1/(k + rank_i) — scale-agnostic, no normalization needed.
// Each list is labelled so Sources on the merged chunk tracks which backends contributed.
func rrfMerge(fts, vec, graph []scoredChunk, k int) []scoredChunk {
	if k <= 0 {
		k = 60
	}

	type entry struct {
		chunk   scoredChunk
		score   float64
		sources map[string]bool
	}
	index := make(map[string]*entry)

	chunkKey := func(c scoredChunk) string {
		return fmt.Sprintf("%s:%d", c.Path, c.StartLine)
	}

	addList := func(list []scoredChunk, label string) {
		for rank, c := range list {
			rrf := 1.0 / float64(k+rank+1)
			k2 := chunkKey(c)
			if e, ok := index[k2]; ok {
				e.score += rrf
				e.sources[label] = true
				if c.UserID != nil {
					e.chunk.UserID = c.UserID
				}
			} else {
				cp := c
				index[k2] = &entry{
					chunk:   cp,
					score:   rrf,
					sources: map[string]bool{label: true},
				}
			}
		}
	}

	addList(fts, "fts")
	addList(vec, "vector")
	addList(graph, "graph")

	out := make([]scoredChunk, 0, len(index))
	for _, e := range index {
		e.chunk.Score = e.score
		for src := range e.sources {
			e.chunk.Sources = append(e.chunk.Sources, src)
		}
		out = append(out, e.chunk)
	}
	return out
}

// applyTemporalDecay multiplies each chunk's score by a time-based decay factor.
// Evergreen chunks skip decay. Access events slow decay (recency boost).
func applyTemporalDecay(chunks []scoredChunk, cfg store.MemorySearchConfig) {
	now := time.Now()
	halfLife := cfg.DecayHalfLifeDays
	if halfLife <= 0 {
		halfLife = 30.0
	}

	for i := range chunks {
		c := &chunks[i]
		if c.IsEvergreen || c.AccessedAt == nil {
			continue
		}

		ageDays := now.Sub(*c.AccessedAt).Hours() / 24
		// Access events slow aging
		effectiveAge := ageDays - float64(c.AccessCount)*cfg.DecayAccessFactor*halfLife
		if effectiveAge < 0 {
			effectiveAge = 0
		}

		decay := math.Pow(0.5, effectiveAge/halfLife)
		c.Score *= decay

		// Access boost: frequently-read chunks get a small score lift
		if c.AccessCount > 0 {
			c.Score *= 1 + math.Log(float64(c.AccessCount))*0.1
		}
	}
}

// mmrRerank applies Maximal Marginal Relevance to promote result diversity.
// Uses path-based similarity approximation to avoid loading stored embeddings.
// lambda=1.0 → pure relevance; lambda=0.0 → pure diversity.
func mmrRerank(chunks []scoredChunk, lambda float64, topK int) []scoredChunk {
	if len(chunks) == 0 {
		return chunks
	}
	if lambda >= 1.0 || topK >= len(chunks) {
		n := topK
		if n > len(chunks) {
			n = len(chunks)
		}
		return chunks[:n]
	}

	pathSim := func(a, b string) float64 {
		if a == b {
			return 0.8
		}
		dirA := a[:strings.LastIndex(a, "/")+1]
		dirB := b[:strings.LastIndex(b, "/")+1]
		if dirA != "" && dirA == dirB {
			return 0.3
		}
		return 0.1
	}

	remaining := make([]scoredChunk, len(chunks))
	copy(remaining, chunks)

	selected := make([]scoredChunk, 0, topK)

	for len(selected) < topK && len(remaining) > 0 {
		bestIdx := -1
		bestScore := math.Inf(-1)

		for i, c := range remaining {
			maxSim := 0.0
			for _, sel := range selected {
				sim := pathSim(c.Path, sel.Path)
				if sim > maxSim {
					maxSim = sim
				}
			}
			mmrScore := lambda*c.Score - (1-lambda)*maxSim
			if mmrScore > bestScore {
				bestScore = mmrScore
				bestIdx = i
			}
		}

		if bestIdx < 0 {
			break
		}
		selected = append(selected, remaining[bestIdx])
		remaining = append(remaining[:bestIdx], remaining[bestIdx+1:]...)
	}

	return selected
}
