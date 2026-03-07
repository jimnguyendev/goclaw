# 14 - Memory Search: Tri-Hybrid Pipeline

This document describes the improved memory search architecture introduced via local commits on top of upstream. It supersedes the "Hybrid Search" section in `07-bootstrap-skills-memory.md` for managed mode.

> **Scope**: Managed mode only (PostgreSQL). The tri-hybrid pipeline replaces the original FTS+vector weighted-average approach.

---

## 1. What Changed and Why

### Original Pipeline (upstream baseline)

```
Query → FTS  ─┐
Query → VEC  ─┴─► weighted_avg(textW=0.3, vecW=0.7) → sort → results
```

**Problems with weighted average**:
- BM25 scores from PostgreSQL FTS (`ts_rank`) are in range `0.0001–0.1`
- Cosine similarity scores from pgvector are in range `0.0–1.0`
- Vector always dominated regardless of keyword relevance
- Scale normalization divided by `maxScore` but that only helps intra-channel, not cross-channel
- No decay: stale chunks rank equally with recent ones
- No diversity: top 6 results could all come from the same file
- No graph traversal: causal/associative queries ("why was I stressed", "what caused the deadline") returned zero results

### New Pipeline (tri-hybrid)

```
Query → FTS  ─┐
Query → VEC  ─┼─► RRF fusion → temporal decay → MMR re-rank → trackAccess → results
Query → KG   ─┘
```

**Key design decisions**:
- **RRF** (Reciprocal Rank Fusion): rank-based, scale-agnostic — eliminates the BM25 vs cosine incompatibility
- **Knowledge Graph BFS**: third retrieval channel for causal/relational queries
- **Temporal decay**: recent/frequently accessed chunks rank higher
- **MMR**: prevents returning N chunks from same file
- **L1 embedding cache**: reduces API calls on re-index and repeated queries

---

## 2. Full Pipeline Architecture

```mermaid
flowchart TD
    Q["Search(query, agentID, userID, opts)"] --> CFG["GetSearchConfig(agentID)\nloads per-agent tuning params"]
    CFG --> PAR

    subgraph PAR["Parallel Retrieval (fetchN = maxResults × 3)"]
        direction LR
        FTS["FTS Search\nplainto_tsquery('simple')\nFallback → ILIKE keyword scan"]
        VEC["Vector Search\npgvector cosine distance\n(embedding <=> $vec::vector)"]
        KG["KG BFS Search\nseed nodes → BFS 3 hops\nhub-capping at degree > 15"]
    end

    PAR --> RRF["RRF Fusion\nscore = Σ 1/(k + rank_i)\nk = cfg.RRFk (default 60)"]
    RRF --> DECAY["Temporal Decay\nscore *= 0.5^(effectiveAge / halfLife)\nEvergreen chunks: skip decay\nAccess boost: × (1 + log(count) × 0.1)"]
    DECAY --> SORT["Sort by score DESC"]
    SORT --> MMR["MMR Re-ranking\nMMR(d) = λ·rel(d) − (1−λ)·max_sim(d, selected)\nλ = cfg.MMRLambda (default 0.7)"]
    MMR --> TRACK["Async trackAccess()\nUPDATE memory_chunks\nSET accessed_at, access_count"]
    TRACK --> FILTER["Apply opts.MinScore, opts.PathPrefix\nLimit to maxResults"]
    FILTER --> OUT["[]MemorySearchResult"]
```

---

## 3. Component: RRF Fusion

**Formula**:
```
score(doc) = Σ_i  1 / (k + rank_i(doc))
```

| Variable | Value | Meaning |
|----------|-------|---------|
| `k` | 60 (default) | Smoothing constant — higher = less sensitive to rank differences |
| `rank_i` | 1-based position in channel `i` | |
| `Sources` | `["fts", "vector", "graph"]` | Which backends contributed this chunk |

**Why RRF beats weighted average**:

| Property | Weighted Avg | RRF |
|----------|-------------|-----|
| Score scale | Must be comparable across channels | Rank only — scale-agnostic |
| Normalization needed | Yes (`maxScore` hack) | No |
| Channel dominance risk | High (vector 0.7 dominates) | Low — each channel contributes via rank |
| Multi-backend bonus | Additive (depends on score) | Additive (guaranteed) |

**Implementation** (`internal/store/pg/memory_search.go`):
```go
func rrfMerge(fts, vec, graph []scoredChunk, k int) []scoredChunk {
    type key struct{ Path string; Start int }
    scores := make(map[key]*scoredChunk)

    addRanked := func(list []scoredChunk, source string) {
        for rank, c := range list {
            k_val := key{c.Path, c.StartLine}
            rrf := 1.0 / float64(k+rank+1)
            if e, ok := scores[k_val]; ok {
                e.Score += rrf
                e.Sources = append(e.Sources, source)
            } else {
                cp := c
                cp.Score = rrf
                cp.Sources = []string{source}
                scores[k_val] = &cp
            }
        }
    }
    addRanked(fts, "fts")
    addRanked(vec, "vector")
    addRanked(graph, "graph")
    // ... collect and sort
}
```

---

## 4. Component: Knowledge Graph

### Schema (migration `000011_memory_kg.up.sql`)

```
memory_kg_nodes        canonical entity nodes (agent-scoped)
memory_kg_aliases      alias → node mapping (case-insensitive lookup)
memory_kg_edges        directed relations, bi-temporal
memory_kg_chunk_mentions  chunk ↔ node cross-index
```

**Bi-temporal edges** (`memory_kg_edges`):

| Column | Timeline | Meaning |
|--------|----------|---------|
| `valid_from / valid_until` | T (world time) | When the fact was true in the real world |
| `known_from / known_until` | T' (system time) | When GoClaw recorded/retracted the fact |

### BFS Search Flow

```mermaid
flowchart TD
    Q["query string"] --> SEED["kgFindSeeds()\nLIKE '%canonical_name%' OR alias match\nLimit 10 seeds"]
    SEED --> HUB{"seedCount > 1?"}
    HUB -->|Yes| EXCL["Exclude highest-degree hub\n(generic node, too much noise)"]
    HUB -->|No| BFS
    EXCL --> BFS

    BFS["kgBFS(seeds, adj, maxHops=3)"] --> CAPDEG{"node.degree > 15?"}
    CAPDEG -->|Yes| CAP1["Expand max 1 hop (hub-capping)"]
    CAPDEG -->|No| CAP2["Expand up to 3 hops"]
    CAP1 & CAP2 --> CHUNKS

    CHUNKS["kgChunksFromVisited()\nJOIN chunk_mentions → memory_chunks"]
    CHUNKS --> SCORE["score = Σ (1/(1+hop)) × coverage\ncoverage = 1 + seedsHit/seedCount"]
    SCORE --> RESULT["[]scoredChunk with source='graph'"]
```

**Seed coverage multiplier**: a chunk that mentions multiple seed entities ranks higher.
```
coverage = 1.0 + (seedsHit / seedCount)   // when seedCount > 1
```

**Hub-capping rationale**: high-degree nodes (e.g., "user", "system") are too generic. Capping at 1 hop prevents BFS from flooding results with loosely-related chunks.

### Indexing

Agents explicitly call `KGIndexEntities()` — there is **no hidden LLM extraction**:
```go
mem.KGIndexEntities(ctx, agentID, userID,
    []store.KGEntity{
        {Name: "stress", NodeType: "EMOTION"},
        {Name: "deadline", NodeType: "EVENT"},
    },
    []store.KGRelation{
        {From: "stress", To: "deadline", Relation: "CAUSED_BY"},
    },
)
```

---

## 5. Component: Temporal Decay

Chunks that haven't been accessed recently decay in score. Frequently-accessed chunks resist decay.

**Formula**:
```
effectiveAge = ageDays − accessCount × decayAccessFactor × halfLife
decay        = 0.5 ^ (effectiveAge / halfLife)
finalScore  *= decay × (1 + log(accessCount) × 0.1)   // access boost
```

**Special cases**:
- `is_evergreen = true` → skip decay entirely (pinned important facts)
- `accessed_at IS NULL` → skip decay (freshly inserted, no age data yet)
- `effectiveAge < 0` → decay = 1.0 (access pattern fully compensates age)

**DB columns added** (migration `000011`):
```sql
ALTER TABLE memory_chunks
    ADD COLUMN accessed_at    TIMESTAMPTZ,
    ADD COLUMN access_count   INT NOT NULL DEFAULT 0,
    ADD COLUMN is_evergreen   BOOLEAN NOT NULL DEFAULT false;
```

**Access tracking** runs asynchronously after each search:
```go
go s.trackAccess(context.Background(), reranked)
// → UPDATE memory_chunks SET accessed_at = NOW(), access_count = access_count + 1
```

---

## 6. Component: MMR Re-ranking

Prevents returning N results from the same file by penalising similarity to already-selected results.

**Formula**:
```
MMR(d) = λ × rel(d) − (1 − λ) × max_sim(d, Selected)
```

**Path-based similarity approximation** (avoids reloading embeddings from DB):
```go
func pathSim(a, b string) float64 {
    if a == b                           { return 0.8 }  // same file
    if filepath.Dir(a) == filepath.Dir(b) { return 0.3 }  // same directory
    return 0.1                                           // different directory
}
```

| λ value | Behaviour |
|---------|-----------|
| `1.0` | Pure relevance (no diversity) |
| `0.7` (default) | 70% relevance, 30% diversity |
| `0.0` | Pure diversity (bad for most use cases) |

---

## 7. Component: L1 Embedding Cache

**Location**: `internal/memory/embeddings.go` — `CachedEmbeddingProvider`

Wraps any `EmbeddingProvider` with a 50-entry in-memory LRU cache keyed by `ContentHash(text)` (SHA-256 first 16 bytes). Applied at both memory and skills call sites in `cmd/gateway.go`.

```
API call reduction per re-index:
  - unchanged chunk → L1 hit (0 API calls)
  - query repeated in same session → L1 hit
  - cold start / new text → L1 miss → API call → stored in cache
```

**Usage**:
```go
provider := memory.NewOpenAIEmbeddingProvider("openai", key, url, model)
cached   := memory.WithL1Cache(provider)  // wrap once at startup
pgStores.Memory.SetEmbeddingProvider(cached)
```

**Eviction**: LRU — when 50 entries full, oldest inserted key is dropped.

---

## 8. Benchmark Evaluation

### Eval Harness

**File**: `internal/store/pg/memory_eval_test.go`

The eval compares the new pipeline against the old weighted-average pipeline directly on a live PostgreSQL database.

**Dataset**: 6 memory documents × 3 query types = 15 test cases

| Query Type | Count | Description |
|------------|-------|-------------|
| `keyword` | 5 | Exact term matches — BM25 should contribute |
| `semantic` | 5 | Paraphrase / intent queries — vector should contribute |
| `graph` | 5 | Causal / associative queries — KG BFS should contribute |

**Metrics**:

| Metric | Formula | What it measures |
|--------|---------|-----------------|
| MRR | `1 / rank(first relevant result)` | How quickly the first correct result appears |
| P@3 | `relevant_in_top_3 / 3` | Precision of top-3 results |
| R@5 | `relevant_in_top_5 / total_relevant` | Coverage within top-5 |

### How to Run

```bash
# Apply migration 000011 first
./goclaw migrate up

# Run eval with live Postgres
MEMORY_EVAL_DSN="postgres://user:pass@localhost:5432/goclaw?sslmode=disable" \
  go test -v -run TestMemoryEval ./internal/store/pg/

# Pure unit tests (no DB required)
go test -v -run "TestRRFMerge|TestTemporalDecay|TestMMR|TestKGBFS" ./internal/store/pg/

# L1 cache tests
go test -v -race ./internal/memory/
```

### Expected Output Format

```
══════════════════════════════════════
  OLD (weighted avg, no KG)
══════════════════════════════════════
  Overall:  MRR=X.XXX  P@3=X.XXX  R@5=X.XXX
  keyword:  MRR=X.XXX  P@3=X.XXX  R@5=X.XXX
  semantic: MRR=X.XXX  P@3=X.XXX  R@5=X.XXX
  graph:    MRR=X.XXX  P@3=X.XXX  R@5=X.XXX
  Backend contribution (total results=45):
    FTS=100%  Vector=0%  Graph=0%  Multi-backend=0%

══════════════════════════════════════
  NEW (RRF+KG+decay+MMR)
══════════════════════════════════════
  Overall:  MRR=X.XXX  P@3=X.XXX  R@5=X.XXX
  ...
  Backend contribution (total results=45):
    FTS=XX%  Vector=XX%  Graph=XX%  Multi-backend=XX%

══════════════════════════════════════ COMPARISON ═════════════
  Metric     Old       New       Δ
  MRR        X.XXX     X.XXX     +X.XXX
  P@3        X.XXX     X.XXX     +X.XXX
  R@5        X.XXX     X.XXX     +X.XXX
  MRR/keyword    X.XXX → X.XXX  (+X.XXX)
  MRR/semantic   X.XXX → X.XXX  (+X.XXX)
  MRR/graph      X.XXX → X.XXX  (+X.XXX)
  Graph results: 0% → XX%
```

### Expected Improvement Pattern

The following table shows the **qualitative direction** of metric changes by query type. Actual numbers depend on the embedding model used and KG population.

| Query Type | Old Behaviour | New Behaviour | Expected Δ |
|------------|--------------|--------------|------------|
| **keyword** | FTS works; vector may override low-score FTS hits | RRF protects FTS rank regardless of score magnitude | MRR flat or slight `+` |
| **semantic** | Exact-term FTS only; vector dominates if available | Vector rank preserved via RRF; KG adds context nodes | MRR `+` with embedding provider |
| **graph** | Only finds docs where query terms literally appear | KG BFS traverses causal chains (e.g. stress→deadline→GoClaw) | MRR `++` even FTS-only |
| **staleness** | Old chunks rank equally with recent ones | `is_evergreen` content stays high; stale content decays | P@3 `+` for recent docs |
| **diversity** | Top-k can be 6 chunks from same file | MMR forces at most 1–2 chunks per file path | Result set diversity `++` |

**Key insight on graph queries**: The `plainto_tsquery('simple')` FTS uses the `simple` dictionary, which performs **no stemming/lemmatization**. Query "why was I stressed" will NOT match document text "Experienced high stress" via FTS. KG traversal via the `stress` entity node is the only way to surface this document without vector search.

---

## 9. Per-Agent Search Configuration

Each agent can tune search pipeline parameters at runtime via `memory_search_config` (no restart needed):

```sql
INSERT INTO memory_search_config (agent_id, key, value)
VALUES ('...', 'rrf_k', '30');  -- more sensitive ranking for small corpora
```

| Key | Default | Tuning Guide |
|-----|---------|-------------|
| `rrf_k` | `60` | Lower (30) = more sensitive to rank differences. Higher (120) = smoother merging |
| `decay_half_life` | `30.0` | Days before a chunk's score halves. Short sessions: lower (7). Long-term agents: higher (90) |
| `decay_access_factor` | `0.1` | How much each access slows decay. 0 = pure time decay. 0.5 = heavily access-boosted |
| `mmr_lambda` | `0.7` | 1.0 = pure relevance. 0.5 = strong diversity. Default covers most cases |

---

## 10. DB Schema Summary

```
memory_chunks (existing + new columns)
├── accessed_at    TIMESTAMPTZ    -- when last accessed via search
├── access_count   INT DEFAULT 0  -- total search hits
└── is_evergreen   BOOLEAN        -- skip temporal decay

memory_kg_nodes
├── id, agent_id, user_id
├── canonical_name TEXT           -- unique per (agent_id, user_id)
├── node_type TEXT                -- 'CONCEPT', 'PERSON', 'EVENT', etc.
└── degree INT                    -- cached edge count for hub-capping

memory_kg_aliases
├── (agent_id, alias) PK
└── node_id → memory_kg_nodes

memory_kg_edges
├── source_id, target_id → memory_kg_nodes
├── relation TEXT                 -- 'CAUSED_BY', 'BELONGS_TO', etc.
├── weight FLOAT
├── valid_from / valid_until      -- T-timeline (world facts)
└── known_from / known_until      -- T'-timeline (system records)

memory_kg_chunk_mentions
├── chunk_id → memory_chunks
└── node_id → memory_kg_nodes

memory_search_config
├── (agent_id, key) PK
└── value TEXT
```

---

## 11. File Reference

| File | Role |
|------|------|
| `internal/memory/embeddings.go` | `EmbeddingProvider` interface, `OpenAIEmbeddingProvider`, `CachedEmbeddingProvider` (L1 cache) |
| `internal/memory/embeddings_test.go` | Unit tests: cache hit/miss, LRU eviction, concurrency safety |
| `internal/store/memory_store.go` | `MemoryStore` interface, `KGEntity`, `KGRelation`, `MemorySearchOptions`, `MemorySearchConfig` |
| `internal/store/pg/memory_docs.go` | `PGMemoryStore`: document CRUD, `IndexDocument`, `BackfillEmbeddings` |
| `internal/store/pg/memory_search.go` | Tri-hybrid pipeline: `Search`, `ftsSearch`, `vectorSearch`, `likeSearch`, `rrfMerge`, `applyTemporalDecay`, `mmrRerank`, `trackAccess` |
| `internal/store/pg/memory_kg.go` | Knowledge Graph: `KGIndexEntities`, `kgBFS`, `kgChunksFromVisited`, `graphSearch` |
| `internal/store/pg/memory_config.go` | `GetSearchConfig`, `SetSearchConfig` |
| `internal/store/pg/memory_search_pure_test.go` | Pure unit tests (no DB): RRF×4, Temporal Decay×4, MMR×3, KG BFS×4 |
| `internal/store/pg/memory_eval_test.go` | Integration eval: old vs new pipeline, MRR/P@3/R@5 across 15 queries |
| `internal/tools/memory.go` | `memory_search` + `memory_get` tool handlers |
| `internal/tools/memory_interceptor.go` | Routes `read_file`/`write_file`/`list_files("memory/*")` → DB |
| `cmd/gateway.go` | Wires `WithL1Cache(provider)` to both memory and skill stores |
| `migrations/000011_memory_kg.up.sql` | Adds temporal columns, KG tables, `memory_search_config` |

---

## Cross-References

| Document | Relevant Content |
|----------|-----------------|
| [07-bootstrap-skills-memory.md](./07-bootstrap-skills-memory.md) | Bootstrap, context files, system prompt — memory flush before compaction (upstream baseline) |
| [06-store-data-model.md](./06-store-data-model.md) | `memory_documents`, `memory_chunks` base schema |
| [01-agent-loop.md](./01-agent-loop.md) | Memory flush trigger (pre-compaction), agent loop flow |
| [03-tools-system.md](./03-tools-system.md) | `memory_search` tool, `MemoryInterceptor` tool routing |
