package pg

import (
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

func makeChunk(path string, startLine int, score float64) scoredChunk {
	return scoredChunk{Path: path, StartLine: startLine, Score: score}
}

func ptr[T any](v T) *T { return &v }

func containsSource(c scoredChunk, src string) bool {
	for _, s := range c.Sources {
		if s == src {
			return true
		}
	}
	return false
}

// ─── rrfMerge ─────────────────────────────────────────────────────────────────

func TestRRFMerge_ScaleAgnostic(t *testing.T) {
	// FTS scores are tiny (BM25 range), vector scores are large (cosine range).
	// Weighted average would be dominated by vector regardless of rank.
	// RRF must give roughly equal weight to rank-0 of each list.
	fts := []scoredChunk{
		makeChunk("a.md", 1, 0.001), // rank 0 in FTS
		makeChunk("b.md", 1, 0.0005),
	}
	vec := []scoredChunk{
		makeChunk("b.md", 1, 0.95), // rank 0 in vector
		makeChunk("a.md", 1, 0.90),
	}

	merged := rrfMerge(fts, vec, nil, 60)

	scoreOf := func(path string) float64 {
		for _, c := range merged {
			if c.Path == path {
				return c.Score
			}
		}
		return 0
	}

	sa, sb := scoreOf("a.md"), scoreOf("b.md")
	// Both appeared at rank 0 in one list and rank 1 in the other → near-equal scores.
	// Neither should be more than 10% higher than the other.
	if math.Abs(sa-sb)/math.Max(sa, sb) > 0.10 {
		t.Errorf("RRF should produce near-equal scores: a=%.4f b=%.4f (diff>10%%)", sa, sb)
	}
}

func TestRRFMerge_SameChunkAllThree_HighestScore(t *testing.T) {
	// A chunk appearing at rank 0 in all 3 lists should outscore any single-list chunk.
	best := makeChunk("winner.md", 1, 0.9)
	only_fts := makeChunk("fts_only.md", 1, 0.99) // higher raw score but single list

	fts := []scoredChunk{best, only_fts}
	vec := []scoredChunk{best}
	graph := []scoredChunk{best}

	merged := rrfMerge(fts, vec, graph, 60)

	scoreOf := func(path string) float64 {
		for _, c := range merged {
			if c.Path == path {
				return c.Score
			}
		}
		return 0
	}

	if scoreOf("winner.md") <= scoreOf("fts_only.md") {
		t.Errorf("chunk in all 3 lists should outscore single-list chunk: winner=%.4f fts_only=%.4f",
			scoreOf("winner.md"), scoreOf("fts_only.md"))
	}
}

func TestRRFMerge_SourcesTracked(t *testing.T) {
	fts := []scoredChunk{makeChunk("x.md", 1, 0.5)}
	vec := []scoredChunk{makeChunk("x.md", 1, 0.8)}
	graph := []scoredChunk{makeChunk("y.md", 1, 0.3)}

	merged := rrfMerge(fts, vec, graph, 60)

	for _, c := range merged {
		switch c.Path {
		case "x.md":
			if !containsSource(c, "fts") || !containsSource(c, "vector") {
				t.Errorf("x.md should have sources [fts vector], got %v", c.Sources)
			}
		case "y.md":
			if !containsSource(c, "graph") {
				t.Errorf("y.md should have source [graph], got %v", c.Sources)
			}
		}
	}
}

func TestRRFMerge_EmptyLists(t *testing.T) {
	fts := []scoredChunk{makeChunk("a.md", 1, 0.5)}
	merged := rrfMerge(fts, nil, nil, 60)
	if len(merged) != 1 {
		t.Errorf("expected 1 result, got %d", len(merged))
	}
}

// ─── applyTemporalDecay ───────────────────────────────────────────────────────

func TestTemporalDecay_EvergreensSkipped(t *testing.T) {
	old := time.Now().Add(-90 * 24 * time.Hour)
	chunks := []scoredChunk{
		{Score: 1.0, IsEvergreen: true, AccessedAt: &old},
		{Score: 1.0, IsEvergreen: false, AccessedAt: &old},
	}
	applyTemporalDecay(chunks, store.DefaultMemorySearchConfig())

	if chunks[0].Score != 1.0 {
		t.Errorf("evergreen score should not change, got %.4f", chunks[0].Score)
	}
	// 90 days with half-life=30 → 0.5^3 = 0.125
	if chunks[1].Score > 0.2 {
		t.Errorf("old chunk should be decayed, got %.4f (want <0.2)", chunks[1].Score)
	}
}

func TestTemporalDecay_NilAccessedAt_Skipped(t *testing.T) {
	chunks := []scoredChunk{
		{Score: 1.0, IsEvergreen: false, AccessedAt: nil},
	}
	applyTemporalDecay(chunks, store.DefaultMemorySearchConfig())
	if chunks[0].Score != 1.0 {
		t.Errorf("nil AccessedAt should skip decay, got %.4f", chunks[0].Score)
	}
}

func TestTemporalDecay_RecentChunkHighScore(t *testing.T) {
	recent := time.Now().Add(-1 * 24 * time.Hour)
	old := time.Now().Add(-60 * 24 * time.Hour)
	chunks := []scoredChunk{
		{Score: 1.0, IsEvergreen: false, AccessedAt: &recent},
		{Score: 1.0, IsEvergreen: false, AccessedAt: &old},
	}
	applyTemporalDecay(chunks, store.DefaultMemorySearchConfig())
	if chunks[0].Score <= chunks[1].Score {
		t.Errorf("recent chunk (%.4f) should outscore old chunk (%.4f)", chunks[0].Score, chunks[1].Score)
	}
}

func TestTemporalDecay_AccessBoostSlowsAging(t *testing.T) {
	accessed := time.Now().Add(-30 * 24 * time.Hour)
	noAccess := scoredChunk{Score: 1.0, IsEvergreen: false, AccessedAt: &accessed, AccessCount: 0}
	withAccess := scoredChunk{Score: 1.0, IsEvergreen: false, AccessedAt: &accessed, AccessCount: 10}

	chunks := []scoredChunk{noAccess, withAccess}
	applyTemporalDecay(chunks, store.DefaultMemorySearchConfig())

	if chunks[1].Score <= chunks[0].Score {
		t.Errorf("frequently accessed chunk (%.4f) should outscore unaccessed (%.4f)",
			chunks[1].Score, chunks[0].Score)
	}
}

// ─── mmrRerank ────────────────────────────────────────────────────────────────

func TestMMR_Lambda1_PureRelevance(t *testing.T) {
	// lambda=1.0 → MMR == original ranking, no diversity applied
	chunks := []scoredChunk{
		makeChunk("a.md", 1, 0.9),
		makeChunk("b.md", 1, 0.8),
		makeChunk("c.md", 1, 0.7),
	}
	result := mmrRerank(chunks, 1.0, 3)
	if result[0].Path != "a.md" || result[1].Path != "b.md" {
		t.Errorf("lambda=1.0 should preserve original order, got %v %v", result[0].Path, result[1].Path)
	}
}

func TestMMR_DiversityPromotesDifferentPaths(t *testing.T) {
	// Two high-score chunks from same file, one lower-score from different file.
	// With diversity, the different-path chunk should be selected over the 2nd same-path chunk.
	chunks := []scoredChunk{
		{Path: "notes/a.md", StartLine: 1, Score: 0.9},
		{Path: "notes/a.md", StartLine: 10, Score: 0.85}, // same dir as first
		{Path: "docs/b.md", StartLine: 1, Score: 0.7},    // different dir
	}
	result := mmrRerank(chunks, 0.3, 2) // lambda=0.3 → strong diversity
	if len(result) < 2 {
		t.Fatal("expected 2 results")
	}
	// After picking notes/a.md:1, the next should be docs/b.md (diverse) not notes/a.md:10 (same dir)
	if result[1].Path != "docs/b.md" {
		t.Errorf("diversity should prefer docs/b.md over notes/a.md, got %s", result[1].Path)
	}
}

func TestMMR_TopKCap(t *testing.T) {
	chunks := make([]scoredChunk, 10)
	for i := range chunks {
		chunks[i] = makeChunk("file.md", i+1, float64(10-i)*0.1)
	}
	result := mmrRerank(chunks, 0.7, 3)
	if len(result) != 3 {
		t.Errorf("expected topK=3, got %d", len(result))
	}
}

// ─── kgBFS ────────────────────────────────────────────────────────────────────

func TestKGBFS_BasicTraversal(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	adj := map[uuid.UUID][]uuid.UUID{a: {b}, b: {c}}
	degrees := map[uuid.UUID]int{a: 1, b: 1, c: 1}

	visited := kgBFS([]uuid.UUID{a}, adj, degrees, 3)

	if _, ok := visited[a]; !ok {
		t.Error("seed node a should be visited")
	}
	if visited[b].hop != 1 {
		t.Errorf("b should be at hop 1, got %d", visited[b].hop)
	}
	if visited[c].hop != 2 {
		t.Errorf("c should be at hop 2, got %d", visited[c].hop)
	}
}

func TestKGBFS_HubCapping(t *testing.T) {
	// Hub-capping: a high-degree node encountered at hop>=1 must not expand further.
	// Scenario: seed → hub (hop=1, degree=20) → deep (hop=2)
	// Without capping, deep would be visited at hop=2.
	// With capping, hub at hop=1 has effectiveMax=1, so hop(1) >= effectiveMax(1) → skipped.
	seed := uuid.New()
	hub := uuid.New()
	deep := uuid.New()

	adj := map[uuid.UUID][]uuid.UUID{
		seed: {hub},
		hub:  {deep},
	}
	degrees := map[uuid.UUID]int{seed: 1, hub: 20, deep: 1}

	visited := kgBFS([]uuid.UUID{seed}, adj, degrees, 3)

	if visited[hub].hop != 1 {
		t.Errorf("hub should be reached at hop 1, got %d", visited[hub].hop)
	}
	if _, reachable := visited[deep]; reachable {
		t.Error("deep should NOT be reachable: hub (degree=20) at hop=1 should not expand")
	}
}

func TestKGBFS_MaxHopsRespected(t *testing.T) {
	a, b, c, d := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	adj := map[uuid.UUID][]uuid.UUID{a: {b}, b: {c}, c: {d}}
	degrees := map[uuid.UUID]int{a: 1, b: 1, c: 1, d: 1}

	visited := kgBFS([]uuid.UUID{a}, adj, degrees, 2)

	if _, ok := visited[d]; ok {
		t.Error("d at hop 3 should not be visited with maxHops=2")
	}
}

// ─── kgFindSeeds hub exclusion ────────────────────────────────────────────────
// (tested at unit level via the exported BFS; DB-level tested in integration)

func TestKGBFS_MultiSeed_BothExpanded(t *testing.T) {
	s1, s2 := uuid.New(), uuid.New()
	n1, n2 := uuid.New(), uuid.New()
	shared := uuid.New()

	adj := map[uuid.UUID][]uuid.UUID{
		s1: {n1, shared},
		s2: {n2, shared},
	}
	degrees := map[uuid.UUID]int{s1: 2, s2: 2, n1: 1, n2: 1, shared: 2}

	visited := kgBFS([]uuid.UUID{s1, s2}, adj, degrees, 2)

	for _, id := range []uuid.UUID{s1, s2, n1, n2, shared} {
		if _, ok := visited[id]; !ok {
			t.Errorf("node %v should be reachable from multi-seed BFS", id)
		}
	}
}
