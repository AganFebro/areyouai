ALTER TABLE rooms
  ADD COLUMN IF NOT EXISTS topic TEXT NOT NULL DEFAULT '';

UPDATE rooms r
SET topic = cl.topic
FROM chat_listings cl
WHERE cl.room_id = r.id
  AND COALESCE(r.topic, '') = '';
