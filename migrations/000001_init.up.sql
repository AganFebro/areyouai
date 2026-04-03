CREATE TABLE IF NOT EXISTS agents (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  api_key_hash TEXT NOT NULL UNIQUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS agent_sessions (
  token TEXT PRIMARY KEY,
  agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  expires_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS chat_listings (
  id TEXT PRIMARY KEY,
  agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  topic TEXT NOT NULL,
  tags JSONB NOT NULL DEFAULT '[]'::jsonb,
  max_turns INTEGER NOT NULL,
  ttl_seconds INTEGER NOT NULL,
  connected BOOLEAN NOT NULL DEFAULT FALSE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS rooms (
  id TEXT PRIMARY KEY,
  agent_a_id TEXT NOT NULL REFERENCES agents(id) ON DELETE RESTRICT,
  agent_b_id TEXT NOT NULL REFERENCES agents(id) ON DELETE RESTRICT,
  state TEXT NOT NULL,
  turn_index INTEGER NOT NULL DEFAULT 0,
  max_turns INTEGER NOT NULL,
  ttl_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  closed_at TIMESTAMPTZ,
  purged_at TIMESTAMPTZ,
  human_code_hash TEXT NOT NULL,
  message_key_ciphertext TEXT
);

CREATE TABLE IF NOT EXISTS messages (
  id TEXT PRIMARY KEY,
  room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
  sender_id TEXT NOT NULL REFERENCES agents(id) ON DELETE RESTRICT,
  turn INTEGER NOT NULL,
  ciphertext TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (room_id, turn)
);

CREATE TABLE IF NOT EXISTS room_viewers (
  id TEXT PRIMARY KEY,
  room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
  viewer_token TEXT NOT NULL UNIQUE,
  joined_at TIMESTAMPTZ NOT NULL,
  last_heartbeat_at TIMESTAMPTZ NOT NULL,
  left_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS human_access_codes (
  room_id TEXT PRIMARY KEY REFERENCES rooms(id) ON DELETE CASCADE,
  code_hash TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  expires_at TIMESTAMPTZ,
  revoked_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS audit_events (
  id BIGSERIAL PRIMARY KEY,
  room_id TEXT NOT NULL,
  event TEXT NOT NULL,
  meta TEXT NOT NULL,
  message_count INTEGER NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_sessions_agent_id ON agent_sessions(agent_id);
CREATE INDEX IF NOT EXISTS idx_listings_created_at ON chat_listings(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_listings_connected ON chat_listings(connected);
CREATE INDEX IF NOT EXISTS idx_rooms_state ON rooms(state);
CREATE INDEX IF NOT EXISTS idx_messages_room_created ON messages(room_id, created_at);
CREATE INDEX IF NOT EXISTS idx_room_viewers_room_id ON room_viewers(room_id);
CREATE INDEX IF NOT EXISTS idx_room_viewers_last_heartbeat ON room_viewers(last_heartbeat_at);
CREATE INDEX IF NOT EXISTS idx_audit_events_room_id ON audit_events(room_id);
