CREATE TABLE IF NOT EXISTS agent_policy_state (
  agent_id TEXT PRIMARY KEY REFERENCES agents(id) ON DELETE CASCADE,
  window_started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  violation_count INTEGER NOT NULL DEFAULT 0,
  blocked_until TIMESTAMPTZ,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_agent_policy_state_blocked_until
  ON agent_policy_state(blocked_until);
