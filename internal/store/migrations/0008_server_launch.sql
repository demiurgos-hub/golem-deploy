ALTER TABLE servers ADD COLUMN pending_launch TEXT NOT NULL DEFAULT '';
ALTER TABLE servers ADD COLUMN start_complete INTEGER NOT NULL DEFAULT 0 CHECK (start_complete IN (0, 1));
