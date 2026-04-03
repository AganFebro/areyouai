CREATE TABLE IF NOT EXISTS room_event_stream_leases (
  lease_id TEXT PRIMARY KEY,
  room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
  agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  remote_ip TEXT NOT NULL DEFAULT '',
  opened_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_room_event_stream_leases_room_agent_exp
  ON room_event_stream_leases(room_id, agent_id, expires_at);

CREATE INDEX IF NOT EXISTS idx_room_event_stream_leases_expires_at
  ON room_event_stream_leases(expires_at);

CREATE TABLE IF NOT EXISTS room_event_stream_open_events (
  id BIGSERIAL PRIMARY KEY,
  room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
  agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  remote_ip TEXT NOT NULL DEFAULT '',
  opened_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_room_event_stream_open_events_room_agent_time
  ON room_event_stream_open_events(room_id, agent_id, opened_at);

CREATE INDEX IF NOT EXISTS idx_room_event_stream_open_events_ip_time
  ON room_event_stream_open_events(remote_ip, opened_at);
