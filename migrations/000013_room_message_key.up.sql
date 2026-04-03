ALTER TABLE rooms
  ADD COLUMN IF NOT EXISTS message_key_ciphertext TEXT;
