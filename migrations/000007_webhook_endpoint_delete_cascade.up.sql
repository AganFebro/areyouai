ALTER TABLE webhook_outbox
  DROP CONSTRAINT IF EXISTS webhook_outbox_endpoint_id_fkey;

ALTER TABLE webhook_outbox
  ADD CONSTRAINT webhook_outbox_endpoint_id_fkey
  FOREIGN KEY (endpoint_id)
  REFERENCES agent_webhook_endpoints(id)
  ON DELETE CASCADE;
