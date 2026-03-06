DROP TABLE IF EXISTS memory_search_config;
DROP TABLE IF EXISTS memory_kg_chunk_mentions;
DROP TABLE IF EXISTS memory_kg_edges;
DROP TABLE IF EXISTS memory_kg_aliases;
DROP TABLE IF EXISTS memory_kg_nodes;

DROP INDEX IF EXISTS idx_mem_chunks_accessed;

ALTER TABLE memory_chunks
    DROP COLUMN IF EXISTS is_evergreen,
    DROP COLUMN IF EXISTS access_count,
    DROP COLUMN IF EXISTS accessed_at;
