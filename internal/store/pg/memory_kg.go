package pg

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// hubDegreeThreshold is the node degree above which BFS is capped to 1 hop.
// Prevents flooding from high-degree generic entities (e.g. "user", "project").
const hubDegreeThreshold = 15

// kgBFSMaxHops is the maximum BFS depth.
const kgBFSMaxHops = 3

// ─── KGIndexEntities ─────────────────────────────────────────────────────────

// KGIndexEntities persists entities and relations into the knowledge graph.
// Called explicitly by agents — no hidden LLM extraction pass.
func (s *PGMemoryStore) KGIndexEntities(
	ctx context.Context,
	agentID, userID string,
	entities []store.KGEntity,
	relations []store.KGRelation,
) error {
	aid := mustParseUUID(agentID)
	var uid *string
	if userID != "" {
		uid = &userID
	}

	// Upsert nodes and collect name → nodeID map.
	nameToID := make(map[string]uuid.UUID, len(entities))
	for _, e := range entities {
		if e.Name == "" {
			continue
		}
		nodeType := e.NodeType
		if nodeType == "" {
			nodeType = "entity"
		}

		var nodeID uuid.UUID
		err := s.db.QueryRowContext(ctx,
			`INSERT INTO memory_kg_nodes (id, agent_id, user_id, canonical_name, node_type)
			 VALUES (gen_random_uuid(), $1, $2, $3, $4)
			 ON CONFLICT (agent_id, COALESCE(user_id,''), canonical_name)
			 DO UPDATE SET node_type = EXCLUDED.node_type
			 RETURNING id`,
			aid, uid, e.Name, nodeType,
		).Scan(&nodeID)
		if err != nil {
			continue
		}
		nameToID[e.Name] = nodeID

		// Upsert aliases.
		for _, alias := range e.Aliases {
			if alias == "" || alias == e.Name {
				continue
			}
			s.db.ExecContext(ctx,
				`INSERT INTO memory_kg_aliases (agent_id, alias, node_id)
				 VALUES ($1, $2, $3)
				 ON CONFLICT (agent_id, alias) DO NOTHING`,
				aid, strings.ToLower(alias), nodeID,
			)
		}
	}

	// Upsert edges and keep degree counts fresh.
	for _, r := range relations {
		if r.From == "" || r.To == "" || r.Relation == "" {
			continue
		}
		srcID, srcOk := nameToID[r.From]
		tgtID, tgtOk := nameToID[r.To]
		if !srcOk || !tgtOk {
			// Resolve unknown names via DB lookup.
			if !srcOk {
				srcID = s.kgResolveNode(ctx, aid, r.From)
			}
			if !tgtOk {
				tgtID = s.kgResolveNode(ctx, aid, r.To)
			}
			if srcID == uuid.Nil || tgtID == uuid.Nil {
				continue
			}
		}

		weight := r.Weight
		if weight == 0 {
			weight = 1.0
		}

		_, err := s.db.ExecContext(ctx,
			`INSERT INTO memory_kg_edges
			 (id, agent_id, source_id, target_id, relation, weight, valid_from, valid_until)
			 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7)
			 ON CONFLICT (agent_id, source_id, target_id, relation)
			 DO UPDATE SET weight = EXCLUDED.weight,
			               valid_from  = COALESCE(EXCLUDED.valid_from,  memory_kg_edges.valid_from),
			               valid_until = COALESCE(EXCLUDED.valid_until, memory_kg_edges.valid_until)`,
			aid, srcID, tgtID, r.Relation, weight, r.ValidFrom, r.ValidUntil,
		)
		if err != nil {
			continue
		}
	}

	// Refresh degree cache for all touched nodes.
	s.db.ExecContext(ctx,
		`UPDATE memory_kg_nodes n
		 SET degree = (
		     SELECT COUNT(*) FROM memory_kg_edges e
		     WHERE e.agent_id = n.agent_id
		       AND (e.source_id = n.id OR e.target_id = n.id)
		 )
		 WHERE n.agent_id = $1`, aid)

	return nil
}

// kgResolveNode looks up a canonical node ID by name or alias.
func (s *PGMemoryStore) kgResolveNode(ctx context.Context, agentID uuid.UUID, name string) uuid.UUID {
	var id uuid.UUID
	// Try canonical name first.
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM memory_kg_nodes WHERE agent_id = $1 AND lower(canonical_name) = lower($2)`,
		agentID, name,
	).Scan(&id)
	if err == nil {
		return id
	}
	// Try alias.
	s.db.QueryRowContext(ctx,
		`SELECT node_id FROM memory_kg_aliases WHERE agent_id = $1 AND alias = lower($2)`,
		agentID, name,
	).Scan(&id)
	return id
}

// ─── Graph BFS search ─────────────────────────────────────────────────────────

type visitedNode struct {
	hop    int
	degree int
}

// kgFindSeeds matches entity names/aliases in the query and returns seed node IDs.
// If multiple seeds are found, the highest-degree node (hub) is excluded to reduce noise —
// it connects to everything and adds no signal when more specific seeds exist.
func (s *PGMemoryStore) kgFindSeeds(ctx context.Context, query string, agentID uuid.UUID) []uuid.UUID {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT n.id, n.degree
		 FROM memory_kg_nodes n
		 WHERE n.agent_id = $1
		   AND (
		       lower($2) LIKE '%' || lower(n.canonical_name) || '%'
		       OR EXISTS (
		           SELECT 1 FROM memory_kg_aliases a
		           WHERE a.agent_id = $1 AND a.node_id = n.id
		             AND lower($2) LIKE '%' || a.alias || '%'
		       )
		   )
		 LIMIT 10`,
		agentID, query,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	type seedNode struct {
		id     uuid.UUID
		degree int
	}
	var all []seedNode
	for rows.Next() {
		var sn seedNode
		if rows.Scan(&sn.id, &sn.degree) == nil {
			all = append(all, sn)
		}
	}

	if len(all) <= 1 {
		if len(all) == 1 {
			return []uuid.UUID{all[0].id}
		}
		return nil
	}

	// Find the top-degree hub and exclude it when other seeds exist.
	hubIdx := 0
	for i, sn := range all {
		if sn.degree > all[hubIdx].degree {
			hubIdx = i
		}
	}
	seeds := make([]uuid.UUID, 0, len(all)-1)
	for i, sn := range all {
		if i != hubIdx {
			seeds = append(seeds, sn.id)
		}
	}
	return seeds
}

// kgLoadEdges loads all edges for an agent into an adjacency map.
func (s *PGMemoryStore) kgLoadEdges(ctx context.Context, agentID uuid.UUID) map[uuid.UUID][]uuid.UUID {
	rows, err := s.db.QueryContext(ctx,
		`SELECT source_id, target_id FROM memory_kg_edges WHERE agent_id = $1 AND known_until IS NULL`,
		agentID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	adj := make(map[uuid.UUID][]uuid.UUID)
	for rows.Next() {
		var src, tgt uuid.UUID
		if rows.Scan(&src, &tgt) == nil {
			adj[src] = append(adj[src], tgt)
			adj[tgt] = append(adj[tgt], src) // undirected traversal
		}
	}
	return adj
}

// kgLoadDegrees returns a map of nodeID → degree for hub-capping.
func (s *PGMemoryStore) kgLoadDegrees(ctx context.Context, agentID uuid.UUID) map[uuid.UUID]int {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, degree FROM memory_kg_nodes WHERE agent_id = $1`, agentID)
	if err != nil {
		return nil
	}
	defer rows.Close()

	deg := make(map[uuid.UUID]int)
	for rows.Next() {
		var id uuid.UUID
		var d int
		if rows.Scan(&id, &d) == nil {
			deg[id] = d
		}
	}
	return deg
}

// kgBFS performs BFS from seed nodes, respecting hub-node capping.
// Returns map[nodeID] → {hop, degree}.
func kgBFS(seeds []uuid.UUID, adj map[uuid.UUID][]uuid.UUID, degrees map[uuid.UUID]int, maxHops int) map[uuid.UUID]visitedNode {
	visited := make(map[uuid.UUID]visitedNode)
	queue := make([]uuid.UUID, 0, len(seeds))

	for _, s := range seeds {
		visited[s] = visitedNode{hop: 0, degree: degrees[s]}
		queue = append(queue, s)
	}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		info := visited[cur]

		// Hub-node capping: high-degree nodes expand at most 1 hop.
		effectiveMax := maxHops
		if info.degree > hubDegreeThreshold {
			effectiveMax = 1
		}
		if info.hop >= effectiveMax {
			continue
		}

		for _, neighbor := range adj[cur] {
			if _, seen := visited[neighbor]; !seen {
				visited[neighbor] = visitedNode{hop: info.hop + 1, degree: degrees[neighbor]}
				queue = append(queue, neighbor)
			}
		}
	}
	return visited
}

// kgChunksFromVisited fetches chunks linked to any visited node,
// scoped to agent+user. Score = Σ 1/(1+hop) across all matched nodes per chunk,
// then multiplied by a seed-coverage bonus: chunks mentioned by more seeds rank higher
// (approximates intersection without discarding union results).
func (s *PGMemoryStore) kgChunksFromVisited(
	ctx context.Context,
	agentID uuid.UUID,
	userID string,
	visited map[uuid.UUID]visitedNode,
	seedCount int,
	limit int,
) ([]scoredChunk, error) {
	if len(visited) == 0 {
		return nil, nil
	}

	// Build IN list for node IDs and per-node hop scores.
	nodeIDs := make([]string, 0, len(visited))
	nodeScore := make(map[string]float64, len(visited))
	// Track which nodes are seeds (hop==0) for coverage counting.
	seedSet := make(map[string]bool)
	for id, v := range visited {
		nodeIDs = append(nodeIDs, "'"+id.String()+"'")
		nodeScore[id.String()] = 1.0 / float64(1+v.hop)
		if v.hop == 0 {
			seedSet[id.String()] = true
		}
	}

	inClause := strings.Join(nodeIDs, ",")

	// Fetch all (chunk_id, node_id) pairs in one query; aggregate in Go.
	var q string
	var args []interface{}
	if userID != "" {
		q = `SELECT c.id::text, c.path, c.start_line, c.end_line, c.text,
		         c.user_id, c.accessed_at, c.access_count, c.is_evergreen,
		         m.node_id::text
		     FROM memory_kg_chunk_mentions m
		     JOIN memory_chunks c ON c.id = m.chunk_id
		     WHERE m.node_id::text IN (` + inClause + `)
		       AND c.agent_id = $1
		       AND (c.user_id IS NULL OR c.user_id = $2)`
		args = []interface{}{agentID, userID}
	} else {
		q = `SELECT c.id::text, c.path, c.start_line, c.end_line, c.text,
		         c.user_id, c.accessed_at, c.access_count, c.is_evergreen,
		         m.node_id::text
		     FROM memory_kg_chunk_mentions m
		     JOIN memory_chunks c ON c.id = m.chunk_id
		     WHERE m.node_id::text IN (` + inClause + `)
		       AND c.agent_id = $1
		       AND c.user_id IS NULL`
		args = []interface{}{agentID}
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Aggregate per chunk: sum hop scores + count distinct seed nodes matched.
	type aggEntry struct {
		chunk      scoredChunk
		hopScore   float64
		seedsHit   int
	}
	agg := make(map[string]*aggEntry)

	for rows.Next() {
		var chunkID, nodeIDStr string
		var c scoredChunk
		rows.Scan(&chunkID, &c.Path, &c.StartLine, &c.EndLine, &c.Text,
			&c.UserID, &c.AccessedAt, &c.AccessCount, &c.IsEvergreen, &nodeIDStr)

		e, ok := agg[chunkID]
		if !ok {
			c.ID = chunkID
			e = &aggEntry{chunk: c}
			agg[chunkID] = e
		}
		e.hopScore += nodeScore[nodeIDStr]
		if seedSet[nodeIDStr] {
			e.seedsHit++
		}
	}

	// Compute final score: hop score × seed-coverage multiplier.
	// A chunk that mentions all seeds gets a 2× boost; one seed → 1×.
	results := make([]scoredChunk, 0, len(agg))
	for _, e := range agg {
		coverage := 1.0
		if seedCount > 1 && e.seedsHit > 0 {
			coverage = 1.0 + float64(e.seedsHit)/float64(seedCount)
		}
		e.chunk.Score = e.hopScore * coverage
		results = append(results, e.chunk)
	}

	// Sort by score desc, cap at limit.
	for i := 0; i < len(results); i++ {
		for j := i + 1; j < len(results); j++ {
			if results[j].Score > results[i].Score {
				results[i], results[j] = results[j], results[i]
			}
		}
	}
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

// graphSearch is the high-level entry point: seeds → BFS → chunk scores.
func (s *PGMemoryStore) graphSearch(
	ctx context.Context,
	query string,
	agentID uuid.UUID,
	userID string,
	limit int,
) ([]scoredChunk, error) {
	seeds := s.kgFindSeeds(ctx, query, agentID)
	if len(seeds) == 0 {
		return nil, nil
	}
	adj := s.kgLoadEdges(ctx, agentID)
	degrees := s.kgLoadDegrees(ctx, agentID)
	visited := kgBFS(seeds, adj, degrees, kgBFSMaxHops)
	return s.kgChunksFromVisited(ctx, agentID, userID, visited, len(seeds), limit)
}

// ─── KGStats ──────────────────────────────────────────────────────────────────

func (s *PGMemoryStore) KGStats(ctx context.Context, agentID string) (*store.KGStatsResult, error) {
	aid := mustParseUUID(agentID)

	var res store.KGStatsResult

	s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memory_kg_nodes WHERE agent_id = $1`, aid,
	).Scan(&res.NodeCount)

	s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memory_kg_edges WHERE agent_id = $1`, aid,
	).Scan(&res.EdgeCount)

	rows, err := s.db.QueryContext(ctx,
		`SELECT canonical_name, degree FROM memory_kg_nodes
		 WHERE agent_id = $1
		 ORDER BY degree DESC LIMIT 10`, aid)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var n store.KGNodeInfo
			if rows.Scan(&n.Name, &n.Degree) == nil {
				res.TopNodes = append(res.TopNodes, n)
			}
		}
	}

	return &res, nil
}

// ─── trackAccess ─────────────────────────────────────────────────────────────

// trackAccess bumps access_count and sets accessed_at for returned chunk IDs.
// Called asynchronously so it never blocks the search response.
func (s *PGMemoryStore) trackAccess(ctx context.Context, chunks []scoredChunk) {
	if len(chunks) == 0 {
		return
	}

	// Use parameterized query to avoid SQL injection from chunk IDs.
	params := make([]string, 0, len(chunks))
	args := make([]interface{}, 0, len(chunks))
	for _, c := range chunks {
		if c.ID != "" {
			args = append(args, c.ID)
			params = append(params, fmt.Sprintf("$%d::uuid", len(args)))
		}
	}
	if len(params) == 0 {
		return
	}

	_, err := s.db.ExecContext(ctx,
		`UPDATE memory_chunks
		 SET access_count = access_count + 1, accessed_at = NOW()
		 WHERE id IN (`+strings.Join(params, ",")+`)`,
		args...,
	)
	if err != nil {
		slog.Warn("memory.search.track_access_failed", "error", err, "chunk_count", len(params))
	}
}
