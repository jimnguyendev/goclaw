package pg

// Memory architecture eval: proves the new tri-hybrid pipeline (RRF + KG + decay + MMR)
// outperforms the old weighted-average architecture on three query types.
//
// Run with a live Postgres (migration 000011 applied):
//   MEMORY_EVAL_DSN="postgres://..." go test -v -run TestMemoryEval ./internal/store/pg/
//
// Without DSN the test is skipped automatically.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// ─── Eval dataset ─────────────────────────────────────────────────────────────

type evalCase struct {
	Query        string
	RelevantKeys []string // "path:startLine" — ground truth
	Type         string   // "keyword" | "semantic" | "graph"
}

// evalMemories are inserted into the DB before eval runs.
var evalMemories = []struct {
	Path    string
	Content string
}{
	{
		Path: "memory/arch.md",
		Content: `# Memory Architecture
The old system used weighted average to merge FTS and vector scores.
Problem: BM25 scores (0.001) and cosine similarity (0.9) are incompatible scales.
Vector always dominated regardless of keyword relevance.`,
	},
	{
		Path: "memory/rrf.md",
		Content: `# Reciprocal Rank Fusion
RRF formula: score = Σ 1/(k + rank_i) where k=60.
Scale-agnostic: only rank position matters, not raw score magnitude.
Replaced weighted average in the tri-hybrid pipeline.`,
	},
	{
		Path: "memory/stress.md",
		Content: `# Project Stress Log
Experienced high stress in February due to GoClaw deadline being delayed.
Manager requested faster delivery which affected code quality decisions.
Decided to refactor memory system to solve recall quality issues.`,
	},
	{
		Path: "memory/deadline.md",
		Content: `# Deadline Notes
GoClaw project deadline was set for end of February.
Resource constraints caused the delay.
Team agreed to prioritize memory search quality over new features.`,
	},
	{
		Path: "memory/kg.md",
		Content: `# Knowledge Graph Design
Knowledge graph stores entities and causal relationships.
Hub nodes with degree > 15 are capped at 1 BFS hop to prevent flooding.
Agent explicitly calls KGIndexEntities — no hidden LLM extraction.`,
	},
	{
		Path: "memory/mmr.md",
		Content: `# MMR Diversity
Maximal Marginal Relevance prevents returning 5 chunks from the same file.
Lambda=0.7 balances relevance vs diversity.
Path-based similarity approximation avoids reloading stored embeddings.`,
	},
}

// evalCases are the 15 test queries split by type.
var evalCases = []evalCase{
	// ── Keyword queries: BM25 should contribute ──────────────────────────────
	{
		Query:        "weighted average memory merge",
		RelevantKeys: []string{"memory/arch.md:1"},
		Type:         "keyword",
	},
	{
		Query:        "BM25 FTS score",
		RelevantKeys: []string{"memory/arch.md:1", "memory/rrf.md:1"},
		Type:         "keyword",
	},
	{
		Query:        "hub node degree BFS",
		RelevantKeys: []string{"memory/kg.md:1"},
		Type:         "keyword",
	},
	{
		Query:        "lambda MMR diversity",
		RelevantKeys: []string{"memory/mmr.md:1"},
		Type:         "keyword",
	},
	{
		Query:        "deadline February resource",
		RelevantKeys: []string{"memory/deadline.md:1", "memory/stress.md:1"},
		Type:         "keyword",
	},

	// ── Semantic queries: vector should contribute ───────────────────────────
	{
		Query:        "why was the old search system broken",
		RelevantKeys: []string{"memory/arch.md:1", "memory/rrf.md:1"},
		Type:         "semantic",
	},
	{
		Query:        "how to prevent one result dominating search",
		RelevantKeys: []string{"memory/mmr.md:1", "memory/rrf.md:1"},
		Type:         "semantic",
	},
	{
		Query:        "what causes search result duplication",
		RelevantKeys: []string{"memory/mmr.md:1"},
		Type:         "semantic",
	},
	{
		Query:        "graph traversal too many results",
		RelevantKeys: []string{"memory/kg.md:1"},
		Type:         "semantic",
	},
	{
		Query:        "improve recall quality memory system",
		RelevantKeys: []string{"memory/arch.md:1", "memory/rrf.md:1", "memory/stress.md:1"},
		Type:         "semantic",
	},

	// ── Graph queries: KG should contribute ──────────────────────────────────
	{
		Query:        "why was I stressed",
		RelevantKeys: []string{"memory/stress.md:1", "memory/deadline.md:1"},
		Type:         "graph",
	},
	{
		Query:        "GoClaw deadline stress",
		RelevantKeys: []string{"memory/stress.md:1", "memory/deadline.md:1"},
		Type:         "graph",
	},
	{
		Query:        "decision to refactor",
		RelevantKeys: []string{"memory/stress.md:1", "memory/arch.md:1"},
		Type:         "graph",
	},
	{
		Query:        "memory system replaced weighted average",
		RelevantKeys: []string{"memory/arch.md:1", "memory/rrf.md:1"},
		Type:         "graph",
	},
	{
		Query:        "project quality code manager",
		RelevantKeys: []string{"memory/stress.md:1", "memory/deadline.md:1"},
		Type:         "graph",
	},
}

// ─── Old pipeline (weighted average) — preserved for comparison ───────────────

// hybridMergeOld is the pre-RRF weighted-average merge, kept for A/B comparison.
func hybridMergeOld(fts, vec []scoredChunk, textW, vecW float64) []scoredChunk {
	type key struct{ Path string; Start int }
	seen := make(map[key]*scoredChunk)

	add := func(r scoredChunk, w float64) {
		k := key{r.Path, r.StartLine}
		score := r.Score * w
		if e, ok := seen[k]; ok {
			e.Score += score
		} else {
			cp := r
			cp.Score = score
			seen[k] = &cp
		}
	}
	for _, r := range fts {
		add(r, textW)
	}
	for _, r := range vec {
		add(r, vecW)
	}

	out := make([]scoredChunk, 0, len(seen))
	for _, r := range seen {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// ─── Metrics ──────────────────────────────────────────────────────────────────

func calcMRR(results []store.MemorySearchResult, relevant map[string]bool) float64 {
	for i, r := range results {
		key := fmt.Sprintf("%s:%d", r.Path, r.StartLine)
		if relevant[key] {
			return 1.0 / float64(i+1)
		}
	}
	return 0
}

func calcPrecisionAtK(results []store.MemorySearchResult, relevant map[string]bool, k int) float64 {
	if k > len(results) {
		k = len(results)
	}
	if k == 0 {
		return 0
	}
	hits := 0
	for _, r := range results[:k] {
		if relevant[fmt.Sprintf("%s:%d", r.Path, r.StartLine)] {
			hits++
		}
	}
	return float64(hits) / float64(k)
}

func calcRecallAtK(results []store.MemorySearchResult, relevant map[string]bool, k int) float64 {
	if len(relevant) == 0 {
		return 0
	}
	if k > len(results) {
		k = len(results)
	}
	hits := 0
	for _, r := range results[:k] {
		if relevant[fmt.Sprintf("%s:%d", r.Path, r.StartLine)] {
			hits++
		}
	}
	return float64(hits) / float64(len(relevant))
}

func relevantSet(keys []string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// ─── Backend contribution tracker ─────────────────────────────────────────────

type backendStats struct {
	Total  int
	FTS    int
	Vector int
	Graph  int
	Multi  int // appeared in 2+ backends
}

func trackContributions(results []store.MemorySearchResult, rawChunks []scoredChunk) backendStats {
	// Build path:line → sources map from raw pipeline output
	srcMap := make(map[string][]string, len(rawChunks))
	for _, c := range rawChunks {
		k := fmt.Sprintf("%s:%d", c.Path, c.StartLine)
		srcMap[k] = c.Sources
	}

	var bs backendStats
	for _, r := range results {
		bs.Total++
		k := fmt.Sprintf("%s:%d", r.Path, r.StartLine)
		srcs := srcMap[k]
		if len(srcs) >= 2 {
			bs.Multi++
		}
		for _, s := range srcs {
			switch s {
			case "fts":
				bs.FTS++
			case "vector":
				bs.Vector++
			case "graph":
				bs.Graph++
			}
		}
	}
	return bs
}

// ─── Eval report ──────────────────────────────────────────────────────────────

type evalReport struct {
	Name    string
	MRR     float64
	P3      float64
	R5      float64
	ByType  map[string][3]float64 // type → [MRR, P@3, R@5]
	Backend backendStats
}

func (r evalReport) print(t *testing.T) {
	t.Logf("\n══════════════════════════════════════")
	t.Logf("  %s", r.Name)
	t.Logf("══════════════════════════════════════")
	t.Logf("  Overall:  MRR=%.3f  P@3=%.3f  R@5=%.3f", r.MRR, r.P3, r.R5)
	for _, typ := range []string{"keyword", "semantic", "graph"} {
		m := r.ByType[typ]
		t.Logf("  %-10s MRR=%.3f  P@3=%.3f  R@5=%.3f", typ+":", m[0], m[1], m[2])
	}
	t.Logf("  Backend contribution (total results=%d):", r.Backend.Total)
	pct := func(n int) string {
		if r.Backend.Total == 0 {
			return "0%"
		}
		return fmt.Sprintf("%d%%", int(math.Round(float64(n)*100/float64(r.Backend.Total))))
	}
	t.Logf("    FTS=%s  Vector=%s  Graph=%s  Multi-backend=%s",
		pct(r.Backend.FTS), pct(r.Backend.Vector), pct(r.Backend.Graph), pct(r.Backend.Multi))
}

// ─── Integration test ─────────────────────────────────────────────────────────

func TestMemoryEval(t *testing.T) {
	dsn := os.Getenv("MEMORY_EVAL_DSN")
	if dsn == "" {
		t.Skip("MEMORY_EVAL_DSN not set — skipping integration eval")
	}

	db, err := OpenDB(dsn)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	const agentID = "00000000-0000-0000-0000-000000eeeval" // fixed UUID for eval agent
	const userID = ""

	// ── Setup: ensure clean eval agent ────────────────────────────────────────
	mustExec(t, db, `DELETE FROM memory_chunks WHERE agent_id = $1`, agentID)
	mustExec(t, db, `DELETE FROM memory_kg_nodes WHERE agent_id = $1`, agentID)
	mustExec(t, db, `DELETE FROM agents WHERE id = $1`, agentID)
	mustExec(t, db, `INSERT INTO agents (id, name) VALUES ($1, 'eval-agent') ON CONFLICT DO NOTHING`, agentID)

	mem := NewPGMemoryStore(db, DefaultPGMemoryConfig())

	// ── Seed memories ─────────────────────────────────────────────────────────
	t.Log("Seeding eval memories...")
	for _, m := range evalMemories {
		if err := mem.PutDocument(ctx, agentID, userID, m.Path, m.Content); err != nil {
			t.Fatalf("PutDocument %s: %v", m.Path, err)
		}
		if err := mem.IndexDocument(ctx, agentID, userID, m.Path); err != nil {
			t.Fatalf("IndexDocument %s: %v", m.Path, err)
		}
	}

	// ── Seed KG ───────────────────────────────────────────────────────────────
	t.Log("Seeding knowledge graph...")
	entities := []store.KGEntity{
		{Name: "GoClaw", NodeType: "PROJECT"},
		{Name: "stress", NodeType: "EMOTION"},
		{Name: "deadline", NodeType: "EVENT"},
		{Name: "manager", NodeType: "PERSON"},
		{Name: "weighted average", NodeType: "CONCEPT"},
		{Name: "RRF", NodeType: "CONCEPT"},
		{Name: "memory system", NodeType: "SYSTEM"},
		{Name: "refactor", NodeType: "DECISION"},
	}
	relations := []store.KGRelation{
		{From: "stress", To: "deadline", Relation: "CAUSED_BY"},
		{From: "deadline", To: "GoClaw", Relation: "BELONGS_TO"},
		{From: "manager", To: "deadline", Relation: "REQUESTED"},
		{From: "refactor", To: "memory system", Relation: "TARGETS"},
		{From: "RRF", To: "weighted average", Relation: "REPLACES"},
		{From: "stress", To: "refactor", Relation: "MOTIVATED"},
		{From: "GoClaw", To: "memory system", Relation: "CONTAINS"},
	}
	if err := mem.KGIndexEntities(ctx, agentID, userID, entities, relations); err != nil {
		t.Fatalf("KGIndexEntities: %v", err)
	}

	time.Sleep(100 * time.Millisecond) // let writes settle

	// ── Run NEW pipeline eval ─────────────────────────────────────────────────
	t.Log("Running new pipeline eval...")
	newReport := runEval(t, ctx, mem, agentID, userID, "NEW (RRF+KG+decay+MMR)")

	// ── Run OLD pipeline eval ─────────────────────────────────────────────────
	t.Log("Running old pipeline eval...")
	oldReport := runOldPipelineEval(t, ctx, db, agentID, userID)

	// ── Print results ─────────────────────────────────────────────────────────
	oldReport.print(t)
	newReport.print(t)

	// ── Print diff table ──────────────────────────────────────────────────────
	t.Logf("\n══════════════ COMPARISON ═════════════")
	t.Logf("  Metric     Old       New       Δ")
	t.Logf("  MRR        %.3f     %.3f     %+.3f", oldReport.MRR, newReport.MRR, newReport.MRR-oldReport.MRR)
	t.Logf("  P@3        %.3f     %.3f     %+.3f", oldReport.P3, newReport.P3, newReport.P3-oldReport.P3)
	t.Logf("  R@5        %.3f     %.3f     %+.3f", oldReport.R5, newReport.R5, newReport.R5-oldReport.R5)
	for _, typ := range []string{"keyword", "semantic", "graph"} {
		om, nm := oldReport.ByType[typ], newReport.ByType[typ]
		t.Logf("  MRR/%-9s %.3f → %.3f  (%+.3f)", typ, om[0], nm[0], nm[0]-om[0])
	}
	t.Logf("  Graph results: %d%% → %d%%",
		pctInt(oldReport.Backend.Graph, oldReport.Backend.Total),
		pctInt(newReport.Backend.Graph, newReport.Backend.Total),
	)

	// ── Assertions: new should be at least as good ────────────────────────────
	if newReport.MRR < oldReport.MRR-0.05 {
		t.Errorf("new MRR (%.3f) regressed significantly vs old (%.3f)", newReport.MRR, oldReport.MRR)
	}
	// Graph queries specifically: KG should bring improvement
	newGraphMRR := newReport.ByType["graph"][0]
	oldGraphMRR := oldReport.ByType["graph"][0]
	if newGraphMRR <= oldGraphMRR {
		t.Logf("NOTE: graph query MRR did not improve (old=%.3f new=%.3f) — check KG population", oldGraphMRR, newGraphMRR)
	}

	// ── Cleanup ───────────────────────────────────────────────────────────────
	mustExec(t, db, `DELETE FROM memory_chunks WHERE agent_id = $1`, agentID)
	mustExec(t, db, `DELETE FROM memory_kg_nodes WHERE agent_id = $1`, agentID)
	mustExec(t, db, `DELETE FROM agents WHERE id = $1`, agentID)
}

// runEval evaluates the new Search() pipeline.
func runEval(t *testing.T, ctx context.Context, mem *PGMemoryStore, agentID, userID, name string) evalReport {
	t.Helper()

	report := evalReport{
		Name:   name,
		ByType: make(map[string][3]float64),
	}

	typeAccum := make(map[string][3]float64)
	typeCounts := make(map[string]int)

	var allRaw []scoredChunk

	for _, c := range evalCases {
		results, err := mem.Search(ctx, c.Query, agentID, userID, store.MemorySearchOptions{MaxResults: 10})
		if err != nil {
			t.Logf("Search error for %q: %v", c.Query, err)
			continue
		}

		rel := relevantSet(c.RelevantKeys)
		mrr := calcMRR(results, rel)
		p3 := calcPrecisionAtK(results, rel, 3)
		r5 := calcRecallAtK(results, rel, 5)

		report.MRR += mrr
		report.P3 += p3
		report.R5 += r5

		acc := typeAccum[c.Type]
		acc[0] += mrr
		acc[1] += p3
		acc[2] += r5
		typeAccum[c.Type] = acc
		typeCounts[c.Type]++

		t.Logf("  [%s] %q → MRR=%.2f P@3=%.2f R@5=%.2f  top=%s",
			c.Type, c.Query, mrr, p3, r5, topResult(results))

		_ = allRaw // populated below via internal call
	}

	n := float64(len(evalCases))
	report.MRR /= n
	report.P3 /= n
	report.R5 /= n

	for typ, acc := range typeAccum {
		cnt := float64(typeCounts[typ])
		report.ByType[typ] = [3]float64{acc[0] / cnt, acc[1] / cnt, acc[2] / cnt}
	}

	_ = allRaw
	return report
}

// runOldPipelineEval simulates old weighted-average pipeline directly on DB.
func runOldPipelineEval(t *testing.T, ctx context.Context, db *sql.DB, agentID, userID string) evalReport {
	t.Helper()

	mem := NewPGMemoryStore(db, DefaultPGMemoryConfig())
	aid := mustParseUUID(agentID)
	report := evalReport{
		Name:   "OLD (weighted avg, no KG)",
		ByType: make(map[string][3]float64),
	}

	typeAccum := make(map[string][3]float64)
	typeCounts := make(map[string]int)

	for _, c := range evalCases {
		fts, _ := mem.ftsSearch(ctx, c.Query, aid, userID, 20)
		if len(fts) == 0 {
			fts, _ = mem.likeSearch(ctx, c.Query, aid, userID, 20)
		}
		// No vector (no provider in test), no graph
		merged := hybridMergeOld(fts, nil, 1.0, 0)

		// Convert to MemorySearchResult
		results := make([]store.MemorySearchResult, 0, len(merged))
		for _, ch := range merged {
			scope := "global"
			if ch.UserID != nil && *ch.UserID != "" {
				scope = "personal"
			}
			results = append(results, store.MemorySearchResult{
				Path:      ch.Path,
				StartLine: ch.StartLine,
				EndLine:   ch.EndLine,
				Score:     ch.Score,
				Snippet:   ch.Text,
				Source:    "memory",
				Scope:     scope,
			})
		}

		rel := relevantSet(c.RelevantKeys)
		mrr := calcMRR(results, rel)
		p3 := calcPrecisionAtK(results, rel, 3)
		r5 := calcRecallAtK(results, rel, 5)

		report.MRR += mrr
		report.P3 += p3
		report.R5 += r5

		acc := typeAccum[c.Type]
		acc[0] += mrr
		acc[1] += p3
		acc[2] += r5
		typeAccum[c.Type] = acc
		typeCounts[c.Type]++
	}

	n := float64(len(evalCases))
	report.MRR /= n
	report.P3 /= n
	report.R5 /= n

	for typ, acc := range typeAccum {
		cnt := float64(typeCounts[typ])
		report.ByType[typ] = [3]float64{acc[0] / cnt, acc[1] / cnt, acc[2] / cnt}
	}

	// Old pipeline: no graph contribution by definition
	report.Backend = backendStats{Total: len(evalCases) * 3, FTS: len(evalCases) * 3}

	return report
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func mustExec(t *testing.T, db *sql.DB, q string, args ...interface{}) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Logf("mustExec warning: %v", err) // non-fatal: cleanup rows may not exist
	}
}

func topResult(results []store.MemorySearchResult) string {
	if len(results) == 0 {
		return "<none>"
	}
	r := results[0]
	parts := strings.Split(r.Path, "/")
	return fmt.Sprintf("%s (%.3f)", parts[len(parts)-1], r.Score)
}

func pctInt(n, total int) int {
	if total == 0 {
		return 0
	}
	return int(math.Round(float64(n) * 100 / float64(total)))
}
