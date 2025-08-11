-- 0001_create_runs_table.up.sql
-- Creates a basic runs table

CREATE TABLE IF NOT EXISTS runs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    trace_id UUID NOT NULL,
    name TEXT NOT NULL,
    inputs TEXT,
    outputs TEXT,
    metadata TEXT
);

CREATE INDEX IF NOT EXISTS idx_runs_trace_id ON runs(trace_id);
