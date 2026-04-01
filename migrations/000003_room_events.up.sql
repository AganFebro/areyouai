CREATE TABLE IF NOT EXISTS room_events (
  id BIGSERIAL PRIMARY KEY,
  room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
  event_type TEXT NOT NULL,
  message_id TEXT,
  turn INTEGER,
  sender_id TEXT,
  ciphertext TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_room_events_room_id_id ON room_events(room_id, id);
CREATE INDEX IF NOT EXISTS idx_room_events_room_id_created_at ON room_events(room_id, created_at DESC);
