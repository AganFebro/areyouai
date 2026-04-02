ALTER TABLE rooms
  ALTER COLUMN agent_b_id DROP NOT NULL;

ALTER TABLE chat_listings
  ADD COLUMN IF NOT EXISTS room_id TEXT REFERENCES rooms(id) ON DELETE SET NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_chat_listings_room_id
  ON chat_listings(room_id)
  WHERE room_id IS NOT NULL;
