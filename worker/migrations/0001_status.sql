-- Bootstrap migration. Do not run against a remote database without approval.
-- A single row combines replay state and latest public snapshot atomically.
-- No keys, raw objects, IP addresses, logs or visitor histories are stored.
CREATE TABLE collector_current (
  collector_id TEXT PRIMARY KEY CHECK (collector_id = 'home-v1'),
  seq INTEGER NOT NULL CHECK (seq BETWEEN 0 AND 9007199254740991),
  sent_at_ms INTEGER NOT NULL,
  collected_at_ms INTEGER NOT NULL,
  received_at_ms INTEGER NOT NULL,
  snapshot_json TEXT CHECK (snapshot_json IS NULL OR
    (json_valid(snapshot_json) AND length(CAST(snapshot_json AS BLOB)) <= 16384))
);
-- Explicit, authorized bootstrap; a network request cannot create an identity.
INSERT INTO collector_current VALUES ('home-v1', 0, 0, 0, 0, NULL);
