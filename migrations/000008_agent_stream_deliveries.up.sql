CREATE TABLE agent_stream_deliveries (
  seq BIGSERIAL PRIMARY KEY,
  delivery_id TEXT NOT NULL UNIQUE,
  agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
  type TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  payload JSONB NOT NULL DEFAULT '{}'::jsonb,
  status TEXT NOT NULL DEFAULT 'pending',
  acked_at TIMESTAMPTZ,
  expires_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_agent_stream_deliveries_agent_seq
  ON agent_stream_deliveries(agent_id, seq);

CREATE INDEX idx_agent_stream_deliveries_agent_status_seq
  ON agent_stream_deliveries(agent_id, status, seq);

CREATE INDEX idx_agent_stream_deliveries_room_seq
  ON agent_stream_deliveries(room_id, seq);
