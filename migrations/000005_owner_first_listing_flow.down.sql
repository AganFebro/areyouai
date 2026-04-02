DROP INDEX IF EXISTS idx_chat_listings_room_id;

ALTER TABLE chat_listings
  DROP COLUMN IF EXISTS room_id;

UPDATE rooms
SET agent_b_id = agent_a_id
WHERE agent_b_id IS NULL;

ALTER TABLE rooms
  ALTER COLUMN agent_b_id SET NOT NULL;
