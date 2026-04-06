DROP INDEX IF EXISTS idx_api_request_logs_route_name;

ALTER TABLE api_request_logs
DROP COLUMN IF EXISTS route_name;
