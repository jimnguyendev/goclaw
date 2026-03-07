# Hệ thống Memory

Hệ thống memory của GoClaw lưu trữ kiến thức dài hạn cho agent trong **PostgreSQL** (pgvector + tsvector). Khác với OpenClaw (file Markdown là source of truth), GoClaw dùng database-first: mọi document, chunk, embedding và knowledge graph đều nằm trong PostgreSQL.

> **Yêu cầu**: PostgreSQL 15+ với extension `pgvector`. Tất cả migration cần được apply (`./goclaw migrate up`).

---

## Memory document

Mỗi agent có một tập document dạng Markdown, lưu theo path (VD: `memory/daily.md`, `MEMORY.md`). Document được chia thành **chunk** (~1000 ký tự, tách tại paragraph boundary) và index vào database.

### Hai tầng memory

| Tầng | Mô tả | Ví dụ path |
|------|-------|------------|
| **Global** | Kiến thức chung của agent, mọi user đều thấy | `memory/arch.md`, `MEMORY.md` |
| **Personal** | Kiến thức riêng per-user (sở thích, lịch sử cá nhân) | `memory/user-prefs.md` (với `user_id`) |

Khi tìm kiếm, kết quả bao gồm cả global và personal (nếu `userID` được cung cấp). Field `scope` trong kết quả cho biết nguồn: `"global"` hoặc `"personal"`.

### Các thao tác với document

```go
// CRUD
mem.PutDocument(ctx, agentID, userID, "memory/notes.md", content)
mem.GetDocument(ctx, agentID, userID, "memory/notes.md")
mem.DeleteDocument(ctx, agentID, userID, "memory/notes.md")
mem.ListDocuments(ctx, agentID, userID)

// Indexing (chunk + embedding + FTS)
mem.IndexDocument(ctx, agentID, userID, "memory/notes.md")
mem.IndexAll(ctx, agentID, userID)
```

---

## Memory tool

GoClaw cung cấp hai tool cho agent:

| Tool | Chức năng |
|------|-----------|
| `memory_search` | Tìm kiếm ngữ nghĩa qua các chunk đã index |
| `memory_get` | Đọc nội dung file memory theo path và line range |

Khi agent gọi `read_file("memory/...")` hoặc `write_file("memory/...")`, **MemoryInterceptor** tự động chuyển hướng sang database thay vì filesystem.

### Khi nào ghi memory

- Quyết định, sở thích, fact lâu dài → `MEMORY.md`
- Ghi chú hàng ngày, ngữ cảnh tạm → `memory/YYYY-MM-DD.md`
- User nói "nhớ điều này" → ghi ngay vào memory (đừng giữ trong context)

### Tự động flush trước compaction

Khi session sắp bị compaction (context gần đầy), agent loop trigger **memory flush** — nhắc model ghi lại kiến thức quan trọng trước khi context bị cắt. Tương tự tính năng `memoryFlush` của OpenClaw.

---

## Tìm kiếm: Pipeline Tri-Hybrid

GoClaw sử dụng pipeline **tri-hybrid** thay vì weighted average đơn giản. Đây là điểm khác biệt kiến trúc chính so với OpenClaw.

### So sánh với OpenClaw

| Đặc điểm | OpenClaw | GoClaw |
|-----------|----------|--------|
| **Backend** | SQLite + sqlite-vec (local-first) | PostgreSQL + pgvector (server-first) |
| **Merge** | Weighted average (`vecW=0.7, textW=0.3`) | **RRF** (Reciprocal Rank Fusion) — scale-agnostic |
| **Kênh tìm kiếm** | 2: BM25 + vector | **3**: FTS + vector + **Knowledge Graph** |
| **Diversity** | MMR (opt-in, Jaccard similarity) | **MMR** (mặc định, path-based similarity) |
| **Temporal decay** | Opt-in, exponential decay | **Mặc định**, half-life 30 ngày + access boost |
| **KG** | Không có | **BFS 3 hops** + hub-capping + seed coverage |
| **Embedding cache** | SQLite cache (50K entries) | **LRU in-memory** (50 entries, L1 cache) |
| **Config** | JSON5 config file | **Per-agent database config** (runtime tunable) |

### Tại sao cần thay đổi?

OpenClaw dùng weighted average để merge FTS và vector:

```
finalScore = 0.7 × vectorScore + 0.3 × textScore
```

**Vấn đề**: BM25 score từ PostgreSQL (`ts_rank`) nằm trong khoảng `0.0001–0.1`, trong khi cosine similarity từ pgvector là `0.0–1.0`. Vector luôn áp đảo bất kể keyword có khớp hay không. Chính OpenClaw cũng thừa nhận hạn chế này trong docs:

> *"This isn't IR-theory perfect [...] common next steps are Reciprocal Rank Fusion (RRF)"*

GoClaw đã thực hiện bước tiếp theo đó.

### Luồng xử lý

```
Query ──┬──► FTS Search (plainto_tsquery)  ──┐
        ├──► Vector Search (pgvector)       ──┼──► RRF Fusion
        └──► KG BFS (knowledge graph)       ──┘
                                                    │
                                              Temporal Decay
                                                    │
                                               Sort by score
                                                    │
                                              MMR Re-ranking
                                                    │
                                            Async trackAccess
                                                    │
                                         Filter (minScore, pathPrefix)
                                                    │
                                          []MemorySearchResult
```

Ba kênh tìm kiếm chạy **song song** (`sync.WaitGroup`). Mỗi kênh có thể thất bại độc lập mà không ảnh hưởng kết quả chung — lỗi được log qua `slog.Warn`.

---

## Thành phần 1: RRF Fusion

**Reciprocal Rank Fusion** merge kết quả dựa trên **vị trí xếp hạng**, không phải điểm số thô.

```
score(doc) = Σ  1 / (k + rank_i(doc))
```

| Tham số | Giá trị | Ý nghĩa |
|---------|---------|---------|
| `k` | 60 (mặc định) | Hằng số làm mượt — cao hơn = ít nhạy với rank |
| `rank_i` | Vị trí 1-based trong kênh `i` | FTS, vector, hoặc graph |

### Tại sao RRF tốt hơn weighted average?

| Thuộc tính | Weighted Average | RRF |
|------------|-----------------|-----|
| Thang điểm | Phải comparable giữa các kênh | Chỉ dùng rank — bất kể thang |
| Cần normalize | Có (hack `maxScore`) | Không |
| Rủi ro 1 kênh áp đảo | Cao (vector 0.7 luôn thắng) | Thấp — mỗi kênh đóng góp qua rank |
| Bonus multi-backend | Phụ thuộc điểm số | Đảm bảo (rank cộng dồn) |

**Ví dụ**: Chunk A xếp hạng 1 ở FTS nhưng hạng 5 ở vector. Chunk B xếp hạng 1 ở vector nhưng hạng 5 ở FTS. Weighted average cho B thắng (vì vector score 0.95 × 0.7 >> FTS score 0.001 × 0.3). RRF cho cả hai điểm gần bằng nhau vì rank giống nhau.

### Sources tracking

Mỗi kết quả trả về field `sources` cho biết chunk đến từ kênh nào:

```json
{
  "path": "memory/arch.md",
  "score": 0.048,
  "sources": ["fts", "vector"],
  "scope": "global"
}
```

Chunk xuất hiện ở nhiều kênh → điểm RRF cao hơn → xếp hạng tốt hơn.

---

## Thành phần 2: Knowledge Graph

Knowledge graph là kênh tìm kiếm thứ ba, giải quyết các truy vấn **quan hệ/nhân quả** mà FTS và vector không thể trả lời.

### Ví dụ

Truy vấn: *"Tại sao tôi bị stress?"*

- **FTS**: `plainto_tsquery('simple')` không stemming → "stress" ≠ "stressed" → 0 kết quả
- **Vector**: Nếu có embedding provider, có thể tìm thấy nhưng không chắc
- **KG**: Tìm entity "stress" → BFS → stress `CAUSED_BY` deadline → deadline `BELONGS_TO` GoClaw → trả về các chunk liên quan

### Schema (PostgreSQL)

```
memory_kg_nodes          Các entity node (agent-scoped)
├── canonical_name       Tên chuẩn (unique per agent+user)
├── node_type            'CONCEPT', 'PERSON', 'EVENT', 'EMOTION'...
└── degree               Số cạnh (cache cho hub-capping)

memory_kg_aliases        Alias → node mapping (tìm kiếm case-insensitive)

memory_kg_edges          Quan hệ có hướng, bi-temporal
├── relation             'CAUSED_BY', 'BELONGS_TO', 'REPLACES'...
├── weight               Trọng số (mặc định 1.0)
├── valid_from/until     T-timeline: khi fact đúng trong thực tế
└── known_from/until     T'-timeline: khi hệ thống ghi nhận

memory_kg_chunk_mentions Chunk ↔ node cross-index
```

### Luồng BFS

```
Query → kgFindSeeds()          Tìm seed node qua LIKE '%name%' hoặc alias
      → Hub exclusion           Nếu > 1 seed, bỏ node có degree cao nhất
      → kgBFS(maxHops=3)       BFS mở rộng từ seeds
      → Hub-capping             Node degree > 15 chỉ expand 1 hop
      → kgChunksFromVisited()  JOIN chunk_mentions → memory_chunks
      → Score = Σ 1/(1+hop) × coverage
```

**Seed coverage multiplier**: chunk mention nhiều seed entity → điểm cao hơn.

```
coverage = 1.0 + (seedsHit / seedCount)    // khi seedCount > 1
```

**Hub-capping**: Node generic (VD: "user", "system") có degree > 15 chỉ được expand tối đa 1 hop, tránh flood kết quả.

### Indexing KG

Agent **chủ động** gọi `KGIndexEntities()` — **không có hidden LLM extraction**:

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

Mọi lỗi trong quá trình upsert (node, alias, edge, degree refresh) đều được log qua `slog.Warn` để dễ debug data quality.

---

## Thành phần 3: Temporal Decay

Chunk cũ mà không được truy cập sẽ giảm điểm theo thời gian. Chunk được truy cập thường xuyên sẽ chống lại sự suy giảm.

### Công thức

```
effectiveAge = ageDays − accessCount × decayAccessFactor × halfLife
decay        = 0.5 ^ (effectiveAge / halfLife)
finalScore  *= decay × (1 + log(accessCount) × 0.1)
```

### Ví dụ với halfLife = 30 ngày

| Tuổi | Access count | Effective age | Decay factor | Ghi chú |
|------|-------------|---------------|-------------|---------|
| 1 ngày | 0 | 1 | 0.977 | Gần như không decay |
| 30 ngày | 0 | 30 | 0.500 | Giảm 50% |
| 30 ngày | 5 | 15 | 0.707 | Access boost giữ điểm cao hơn |
| 90 ngày | 0 | 90 | 0.125 | Giảm ~87.5% |
| Bất kỳ | — | — | 1.000 | `is_evergreen = true` → bỏ qua decay |

### So sánh với OpenClaw

| | OpenClaw | GoClaw |
|---|---------|--------|
| **Công thức** | `e^(-λ × age)` (exponential) | `0.5^(age/halfLife)` (half-life) |
| **Access boost** | Không | Có — mỗi lần search hit giảm effective age |
| **Evergreen** | Theo tên file (không có date → skip) | Theo cột DB `is_evergreen` (explicit flag) |
| **Mặc định** | Tắt (opt-in) | **Bật** |

### Cột database

```sql
ALTER TABLE memory_chunks
    ADD COLUMN accessed_at    TIMESTAMPTZ,
    ADD COLUMN access_count   INT NOT NULL DEFAULT 0,
    ADD COLUMN is_evergreen   BOOLEAN NOT NULL DEFAULT false;
```

### Access tracking

Sau mỗi search, `trackAccess()` chạy **async** (goroutine riêng) để cập nhật `accessed_at` và `access_count`:

```go
go s.trackAccess(context.Background(), reranked)
```

Dùng parameterized query (`$N::uuid`) để tránh SQL injection. Lỗi được log qua `slog.Warn`.

---

## Thành phần 4: MMR Re-ranking

**Maximal Marginal Relevance** ngăn việc trả về N chunk từ cùng một file.

### Công thức

```
MMR(d) = λ × rel(d) − (1 − λ) × max_sim(d, Selected)
```

### Path-based similarity

GoClaw dùng **path-based approximation** thay vì load lại embedding để tính similarity:

```
same file       → 0.8 similarity
same directory  → 0.3 similarity
different dir   → 0.1 similarity
```

So sánh với OpenClaw dùng **Jaccard text similarity** trên tokenized content. Path-based nhanh hơn (không cần tokenize) nhưng ít chính xác hơn cho cùng-file chunks có nội dung khác nhau.

| λ | Hành vi |
|---|---------|
| `1.0` | Chỉ relevance (không diversity) |
| `0.7` (mặc định) | 70% relevance, 30% diversity |
| `0.0` | Chỉ diversity (ít hữu ích) |

### Ví dụ

Query: *"memory architecture"*

Không có MMR — top 3:
```
1. memory/arch.md:1  (0.048) ← architecture overview
2. memory/arch.md:5  (0.045) ← cùng file, nội dung gần giống
3. memory/rrf.md:1   (0.032) ← RRF details
```

Có MMR (λ=0.7) — top 3:
```
1. memory/arch.md:1  (0.048) ← architecture overview
2. memory/rrf.md:1   (0.032) ← RRF details (diverse!)
3. memory/mmr.md:1   (0.028) ← MMR diversity (diverse!)
```

---

## Thành phần 5: L1 Embedding Cache

**CachedEmbeddingProvider** wrap bất kỳ `EmbeddingProvider` nào với LRU cache 50 entry trong memory.

### Cách hoạt động

```
Embed("hello") → cache miss → gọi API → lưu cache[SHA256("hello")] = embedding
Embed("hello") → cache hit  → trả về từ cache (0 API call)
```

| Tình huống | Kết quả |
|------------|---------|
| Chunk không thay đổi khi re-index | L1 hit → 0 API call |
| Query lặp lại trong session | L1 hit |
| Text mới / cold start | L1 miss → API call → lưu cache |
| Cache đầy (50 entries) | LRU eviction: entry cũ nhất bị xóa |

### So sánh với OpenClaw

| | OpenClaw | GoClaw |
|---|---------|--------|
| **Storage** | SQLite (50K entries, persistent) | In-memory LRU (50 entries, volatile) |
| **Eviction** | Không rõ | True LRU (promote on access) |
| **Scope** | Per-agent SQLite DB | Per-process (shared across agents) |

GoClaw cache nhỏ hơn nhiều nhưng đủ cho hot-path: re-index unchanged chunks và repeated queries.

### Sử dụng

```go
provider := memory.NewOpenAIEmbeddingProvider("openai", key, url, model)
cached   := memory.WithL1Cache(provider)  // wrap 1 lần khi khởi động
pgStores.Memory.SetEmbeddingProvider(cached)
```

---

## Cấu hình per-agent

Mỗi agent có thể tune pipeline tìm kiếm **tại runtime** qua bảng `memory_search_config` — không cần restart:

```sql
INSERT INTO memory_search_config (agent_id, key, value)
VALUES ('...', 'rrf_k', '30');
```

| Key | Mặc định | Hướng dẫn tuning |
|-----|---------|-----------------|
| `rrf_k` | `60` | Thấp hơn (30) = nhạy hơn với rank. Cao hơn (120) = merge mượt hơn |
| `decay_half_life` | `30.0` | Ngày để score giảm 50%. Session ngắn: 7. Agent dài hạn: 90 |
| `decay_access_factor` | `0.1` | Mỗi access giảm bao nhiêu decay. 0 = chỉ theo thời gian. 0.5 = boost mạnh |
| `mmr_lambda` | `0.7` | 1.0 = chỉ relevance. 0.5 = diversity mạnh |

So sánh: OpenClaw config qua JSON5 file, phải restart gateway. GoClaw config per-agent trong DB, apply ngay.

---

## Embedding provider

GoClaw hỗ trợ OpenAI-compatible embedding API:

```go
provider := memory.NewOpenAIEmbeddingProvider(
    "openai",           // tên provider
    apiKey,             // API key
    "https://api.openai.com/v1",  // base URL
    "text-embedding-3-small",     // model
)
provider.WithDimensions(1536)  // optional: truncate output
```

Tương thích với: OpenAI, OpenRouter, vLLM, hoặc bất kỳ endpoint nào theo chuẩn `/v1/embeddings`.

**Vector dimension**: pgvector column dùng `vector(1536)`. Đảm bảo embedding model output khớp dimension này.

---

## Database schema

### Bảng chính

```
memory_documents
├── id, agent_id, user_id, path
├── content TEXT                    -- nội dung Markdown
├── hash TEXT                       -- SHA256 để detect thay đổi
└── updated_at TIMESTAMPTZ

memory_chunks
├── id, document_id, agent_id, user_id
├── path, start_line, end_line
├── text TEXT                       -- nội dung chunk
├── hash TEXT                       -- SHA256 chunk content
├── embedding vector(1536)          -- pgvector embedding
├── tsv tsvector                    -- FTS index
├── accessed_at TIMESTAMPTZ         -- last search hit
├── access_count INT DEFAULT 0      -- total search hits
├── is_evergreen BOOLEAN DEFAULT false
└── updated_at TIMESTAMPTZ

memory_kg_nodes, memory_kg_aliases, memory_kg_edges, memory_kg_chunk_mentions
└── (xem phần Knowledge Graph ở trên)

memory_search_config
├── (agent_id, key) PK
└── value TEXT
```

### Index

```sql
CREATE INDEX idx_mem_vec  ON memory_chunks USING hnsw(embedding vector_cosine_ops);
CREATE INDEX idx_mem_tsv  ON memory_chunks USING gin(tsv);
```

---

## Chạy test

```bash
# Unit test (không cần DB) — 28 tests
go test -v -race ./internal/store/pg/ ./internal/memory/

# Test cụ thể theo component
go test -v -run "TestRRFMerge"        ./internal/store/pg/   # RRF fusion
go test -v -run "TestTemporalDecay"   ./internal/store/pg/   # Temporal decay
go test -v -run "TestMMR"             ./internal/store/pg/   # MMR diversity
go test -v -run "TestKGBFS"           ./internal/store/pg/   # KG BFS
go test -v -run "TestTrackAccess"     ./internal/store/pg/   # Access tracking
go test -v -run "TestGetSearchConfig" ./internal/store/pg/   # Config error paths
go test -v -run "TestCachedProvider"  ./internal/memory/     # L1 cache LRU

# Integration eval (cần PostgreSQL)
MEMORY_EVAL_DSN="postgres://user:pass@localhost:5432/goclaw?sslmode=disable" \
  go test -v -run TestMemoryEval ./internal/store/pg/
```

### Eval benchmark

Eval so sánh pipeline cũ (weighted average, FTS+vector) với pipeline mới (RRF+KG+decay+MMR) trên 15 queries × 3 types:

| Loại query | Số lượng | Kênh chính |
|------------|---------|-----------|
| `keyword` | 5 | FTS (exact term match) |
| `semantic` | 5 | Vector (paraphrase/intent) |
| `graph` | 5 | KG BFS (causal/relational) |

Metrics: **MRR** (Mean Reciprocal Rank), **P@3** (Precision at 3), **R@5** (Recall at 5).

---

## File reference

| File | Vai trò |
|------|---------|
| `internal/memory/embeddings.go` | `EmbeddingProvider` interface, `OpenAIEmbeddingProvider`, `CachedEmbeddingProvider` (L1 cache) |
| `internal/memory/embeddings_test.go` | Unit tests: cache hit/miss, LRU eviction, concurrency |
| `internal/store/memory_store.go` | `MemoryStore` interface, `KGEntity`, `KGRelation`, `MemorySearchConfig` |
| `internal/store/pg/memory_docs.go` | Document CRUD, `IndexDocument`, `BackfillEmbeddings` |
| `internal/store/pg/memory_search.go` | Pipeline: `Search`, `ftsSearch`, `vectorSearch`, `rrfMerge`, `applyTemporalDecay`, `mmrRerank` |
| `internal/store/pg/memory_kg.go` | Knowledge Graph: `KGIndexEntities`, `kgBFS`, `graphSearch`, `trackAccess` |
| `internal/store/pg/memory_config.go` | `GetSearchConfig`, `SetSearchConfig` |
| `internal/store/pg/memory_search_pure_test.go` | Pure unit tests (28): RRF, decay, MMR, BFS, error paths |
| `internal/store/pg/memory_eval_test.go` | Integration eval: old vs new pipeline benchmark |
| `internal/tools/memory.go` | Tool handlers: `memory_search`, `memory_get` |
| `internal/tools/memory_interceptor.go` | Route `read_file/write_file("memory/*")` → DB |
| `cmd/gateway.go` | Wire `WithL1Cache(provider)` vào memory + skills store |
| `migrations/000001_init_schema.up.sql` | Schema gốc: `memory_documents`, `memory_chunks` |
| `migrations/000011_memory_kg.up.sql` | KG tables, temporal columns, `memory_search_config` |
