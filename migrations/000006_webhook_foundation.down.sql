ALTER TABLE room_events
  ADD COLUMN IF NOT EXISTS ciphertext TEXT;

UPDATE room_events AS re
SET ciphertext = m.ciphertext
FROM messages AS m
WHERE re.message_id = m.id
  AND re.ciphertext IS NULL;

DROP TABLE IF EXISTS room_scoped_tokens;
DROP TABLE IF EXISTS webhook_outbox;
DROP TABLE IF EXISTS agent_webhook_endpoints;
