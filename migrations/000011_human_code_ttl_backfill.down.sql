UPDATE rooms
SET human_code_expires_at = NULL
WHERE human_code_expires_at = created_at + INTERVAL '24 hours';
