package pg

import (
	"context"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// GetSearchConfig loads per-agent search config from memory_search_config,
// overlaying stored values on top of the system defaults.
func (s *PGMemoryStore) GetSearchConfig(ctx context.Context, agentID string) (store.MemorySearchConfig, error) {
	cfg := store.DefaultMemorySearchConfig()

	aid, err := uuid.Parse(agentID)
	if err != nil {
		return cfg, fmt.Errorf("parse agent_id: %w", err)
	}

	rows, err := s.db.QueryContext(ctx,
		"SELECT key, value FROM memory_search_config WHERE agent_id = $1", aid)
	if err != nil {
		return cfg, fmt.Errorf("query search config: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) != nil {
			continue
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			continue
		}
		switch k {
		case "rrf_k":
			cfg.RRFk = int(f)
		case "decay_half_life":
			cfg.DecayHalfLifeDays = f
		case "decay_access_factor":
			cfg.DecayAccessFactor = f
		case "mmr_lambda":
			cfg.MMRLambda = f
		}
	}
	return cfg, rows.Err()
}

// SetSearchConfig upserts one or more scoring parameters for an agent.
// Valid keys: rrf_k, decay_half_life, decay_access_factor, mmr_lambda.
func (s *PGMemoryStore) SetSearchConfig(ctx context.Context, agentID string, updates map[string]float64) error {
	aid := mustParseUUID(agentID)
	valid := map[string]bool{
		"rrf_k": true, "decay_half_life": true,
		"decay_access_factor": true, "mmr_lambda": true,
	}
	for k, v := range updates {
		if !valid[k] {
			continue
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO memory_search_config (agent_id, key, value)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (agent_id, key) DO UPDATE SET value = EXCLUDED.value`,
			aid, k, strconv.FormatFloat(v, 'f', -1, 64),
		); err != nil {
			return err
		}
	}
	return nil
}
