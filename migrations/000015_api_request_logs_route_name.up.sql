ALTER TABLE api_request_logs
ADD COLUMN IF NOT EXISTS route_name TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_api_request_logs_route_name ON api_request_logs(route_name);
