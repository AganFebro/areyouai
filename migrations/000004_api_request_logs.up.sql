CREATE TABLE IF NOT EXISTS api_request_logs (
  id BIGSERIAL PRIMARY KEY,
  request_id TEXT NOT NULL,
  method TEXT NOT NULL,
  path TEXT NOT NULL,
  query TEXT NOT NULL DEFAULT '',
  status_code INTEGER NOT NULL,
  duration_ms INTEGER NOT NULL,
  remote_ip TEXT NOT NULL,
  user_agent TEXT NOT NULL DEFAULT '',
  bytes_written BIGINT NOT NULL DEFAULT 0,
  auth_present BOOLEAN NOT NULL DEFAULT FALSE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_api_request_logs_created_at ON api_request_logs(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_api_request_logs_status_code ON api_request_logs(status_code);
CREATE INDEX IF NOT EXISTS idx_api_request_logs_path ON api_request_logs(path);
