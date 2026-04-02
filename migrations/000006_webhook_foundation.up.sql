CREATE TABLE IF NOT EXISTS agent_webhook_endpoints (
  id TEXT PRIMARY KEY,
  agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  url TEXT NOT NULL,
  secret_ciphertext TEXT NOT NULL,
  key_id TEXT NOT NULL,
  enabled BOOLEAN NOT NULL DEFAULT TRUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_agent_webhook_endpoints_agent_id
  ON agent_webhook_endpoints(agent_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_agent_webhook_endpoints_enabled
  ON agent_webhook_endpoints(enabled);

CREATE TABLE IF NOT EXISTS webhook_outbox (
  id BIGSERIAL PRIMARY KEY,
  room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
  room_event_id BIGINT NOT NULL REFERENCES room_events(id) ON DELETE CASCADE,
  target_agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  endpoint_id TEXT NOT NULL REFERENCES agent_webhook_endpoints(id) ON DELETE CASCADE,
  event_type TEXT NOT NULL,
  payload JSONB NOT NULL DEFAULT '{}'::jsonb,
  status TEXT NOT NULL DEFAULT 'pending',
  attempt_count INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  last_error TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_webhook_outbox_status_next_attempt
  ON webhook_outbox(status, next_attempt_at, id);

CREATE INDEX IF NOT EXISTS idx_webhook_outbox_room_id
  ON webhook_outbox(room_id, id);

CREATE INDEX IF NOT EXISTS idx_webhook_outbox_target_agent_id
  ON webhook_outbox(target_agent_id, id);

CREATE TABLE IF NOT EXISTS room_scoped_tokens (
  id TEXT PRIMARY KEY,
  room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
  agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  token_hash TEXT NOT NULL UNIQUE,
  scope TEXT NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  revoked_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_room_scoped_tokens_room_agent_expires
  ON room_scoped_tokens(room_id, agent_id, expires_at DESC);

CREATE INDEX IF NOT EXISTS idx_room_scoped_tokens_active
  ON room_scoped_tokens(agent_id, expires_at)
  WHERE revoked_at IS NULL;

ALTER TABLE room_events
  DROP COLUMN IF EXISTS ciphertext;
