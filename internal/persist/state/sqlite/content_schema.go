package sqlite

// Content and its owners commit in the same database. Blobs are immutable;
// roots and edges describe reachability, and staging protects unpublished data.
const contentSchema = `
CREATE TABLE content_objects (
 id TEXT PRIMARY KEY CHECK(length(id) = 64),
 data BLOB NOT NULL,
 refs INTEGER NOT NULL DEFAULT 0 CHECK(refs >= 0)
);
CREATE TABLE content_roots (
 owner_kind TEXT NOT NULL,
 owner_id TEXT NOT NULL,
 content_id TEXT NOT NULL REFERENCES content_objects(id),
 PRIMARY KEY(owner_kind, owner_id, content_id)
);
CREATE INDEX content_roots_content ON content_roots(content_id);
CREATE TABLE content_edges (
 parent_id TEXT NOT NULL REFERENCES content_objects(id) ON DELETE CASCADE,
 child_id TEXT NOT NULL REFERENCES content_objects(id),
 PRIMARY KEY(parent_id, child_id)
);
CREATE INDEX content_edges_child ON content_edges(child_id);
CREATE TABLE content_staging (
 stage_id TEXT NOT NULL,
 content_id TEXT NOT NULL REFERENCES content_objects(id),
 PRIMARY KEY(stage_id, content_id)
);
CREATE TRIGGER content_fact_deleted AFTER DELETE ON turn_domain_facts BEGIN
 DELETE FROM content_roots WHERE owner_kind = 'fact'
 AND owner_id = OLD.turn_id || ':' || OLD.sequence;
END;
CREATE TRIGGER content_terminal_deleted AFTER DELETE ON turn_terminal_envelopes BEGIN
 DELETE FROM content_roots WHERE owner_kind = 'terminal' AND owner_id = OLD.turn_id;
END;
CREATE TRIGGER content_snapshot_deleted AFTER DELETE ON snapshots BEGIN
 DELETE FROM content_roots WHERE owner_kind = 'snapshot' AND owner_id = OLD.id;
END;
CREATE TRIGGER content_context_deleted AFTER DELETE ON context_rebases BEGIN
 DELETE FROM content_roots WHERE owner_kind = 'context' AND owner_id = OLD.compaction_id;
END;
CREATE TABLE event_watermark (
 id INTEGER PRIMARY KEY CHECK(id = 1),
 sequence INTEGER NOT NULL CHECK(sequence >= 0)
);
INSERT INTO event_watermark VALUES(1, 0);
CREATE TABLE deleted_event_threads (
 thread_id TEXT PRIMARY KEY,
 deleted_at TEXT NOT NULL
);
CREATE TABLE event_prune_queue (
 sequence INTEGER PRIMARY KEY
);
CREATE TRIGGER remember_deleted_event_thread BEFORE DELETE ON threads BEGIN
 INSERT INTO deleted_event_threads(thread_id, deleted_at)
 VALUES(OLD.id, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
 ON CONFLICT(thread_id) DO NOTHING;
END;
`
