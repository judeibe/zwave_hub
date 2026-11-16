-- 001_init: initial schema for feature 001-coordinator-key-provisioning.
-- Source of truth: specs/001-coordinator-key-provisioning/data-model.md §10 "Physical schema".
-- Migrations are additive: a rollback ships the previous binary, which ignores columns it
-- does not know about. The Go runner applies each file inside one transaction and fails if
-- PRAGMA user_version did not advance.

PRAGMA user_version = 1;

CREATE TABLE coordinators (
  id                    TEXT PRIMARY KEY,
  usb_vid               TEXT NOT NULL,
  usb_pid               TEXT NOT NULL,
  usb_serial            TEXT,
  identity_kind         TEXT NOT NULL CHECK (identity_kind IN ('serial','topology')),
  manufacturer          TEXT,
  product               TEXT,
  firmware_version      TEXT,
  sdk_version           TEXT,
  zwave_manufacturer_id INTEGER,
  zwave_product_type    INTEGER,
  zwave_product_id      INTEGER,
  zwave_chip_type       TEXT,
  support               TEXT NOT NULL CHECK (support IN ('supported','unsupported','unverified')),
  support_reason        TEXT,
  last_path             TEXT,
  first_seen_at         TEXT NOT NULL,
  last_seen_at          TEXT NOT NULL
);

CREATE TABLE provisioning (
  id                    INTEGER PRIMARY KEY CHECK (id = 1),
  active_coordinator_id TEXT REFERENCES coordinators(id) ON DELETE RESTRICT,
  home_id               TEXT,
  key_revision          INTEGER NOT NULL DEFAULT 0,
  applied_revision      INTEGER,
  updated_at            TEXT NOT NULL
);

CREATE TABLE security_keys (
  security_class TEXT PRIMARY KEY CHECK (security_class IN (
                   'S2_AccessControl','S2_Authenticated','S2_Unauthenticated','S0_Legacy',
                   'S2_Authenticated_LR','S2_AccessControl_LR')),
  key_blob       BLOB NOT NULL,
  key_source     TEXT NOT NULL CHECK (key_source IN ('generated','supplied')),
  created_at     TEXT NOT NULL,
  changed_at     TEXT NOT NULL
);

CREATE TABLE confirmations (
  token_hash  TEXT PRIMARY KEY,
  operation   TEXT NOT NULL CHECK (operation IN ('keys.export','network.reset')),
  actor_addr  TEXT NOT NULL,
  issued_at   TEXT NOT NULL,
  expires_at  TEXT NOT NULL,
  consumed_at TEXT
);
CREATE INDEX confirmations_expires_at ON confirmations(expires_at);

CREATE TABLE audit_entries (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  at             TEXT NOT NULL,
  action         TEXT NOT NULL,
  actor_addr     TEXT NOT NULL,
  actor_label    TEXT,
  request_id     TEXT NOT NULL,
  coordinator_id TEXT,
  security_class TEXT,
  outcome        TEXT NOT NULL CHECK (outcome IN ('succeeded','failed','rejected')),
  detail         TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_entries_at ON audit_entries(at DESC, id DESC);
CREATE INDEX audit_entries_action ON audit_entries(action, at DESC);

INSERT INTO provisioning (id, updated_at) VALUES (1, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
