package core

import (
	"database/sql"
	"fmt"

	_ "github.com/mattn/go-sqlite3"
)

// Open 打开（必要时创建）SQLite 数据库并执行 schema 迁移。
// 单连接 + busy_timeout：SQLite 串行化写入，避免并发确认时的 SQLITE_BUSY。
func Open(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(schema)
	return err
}

const schema = `
CREATE TABLE IF NOT EXISTS units (
    id   TEXT PRIMARY KEY,
    name TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS crews (
    id        TEXT PRIMARY KEY,
    name      TEXT NOT NULL,
    unit_id   TEXT NOT NULL,
    specialty TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS actors (
    id      TEXT PRIMARY KEY,
    name    TEXT NOT NULL,
    role    TEXT NOT NULL,
    unit_id TEXT NOT NULL DEFAULT '',
    crew_id TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS zones (
    id   TEXT PRIMARY KEY,
    name TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS zone_deps (
    zone_id     TEXT NOT NULL,
    depends_on  TEXT NOT NULL,
    PRIMARY KEY (zone_id, depends_on)
);

CREATE TABLE IF NOT EXISTS zone_versions (
    zone_id    TEXT NOT NULL,
    version    INTEGER NOT NULL,
    boundary   TEXT NOT NULL,
    expanded   INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (zone_id, version)
);

CREATE TABLE IF NOT EXISTS personnel (
    id      TEXT PRIMARY KEY,
    name    TEXT NOT NULL,
    unit_id TEXT NOT NULL,
    crew_id TEXT NOT NULL,
    post    TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS vehicles (
    id      TEXT PRIMARY KEY,
    plate   TEXT NOT NULL,
    kind    TEXT NOT NULL,
    unit_id TEXT NOT NULL,
    crew_id TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS shifts (
    id        TEXT PRIMARY KEY,
    name      TEXT NOT NULL,
    starts_at INTEGER NOT NULL,
    ends_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS assignments (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    zone_id     TEXT NOT NULL,
    object_type TEXT NOT NULL,
    object_id   TEXT NOT NULL,
    crew_id     TEXT NOT NULL,
    unit_id     TEXT NOT NULL,
    shift_id    TEXT NOT NULL DEFAULT '',
    active      INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_assignments_zone ON assignments(zone_id, active);

-- 门禁回执：receipt_no 唯一，重复扫码幂等。
CREATE TABLE IF NOT EXISTS gate_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    receipt_no  TEXT NOT NULL UNIQUE,
    gate_id     TEXT NOT NULL,
    zone_id     TEXT NOT NULL,
    object_type TEXT NOT NULL,
    object_id   TEXT NOT NULL,
    direction   TEXT NOT NULL,
    occurred_at INTEGER NOT NULL,
    received_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_gate_events_obj ON gate_events(zone_id, object_type, object_id);

-- 班组确认：绑定区域版本，(区域,版本,班组) 唯一，重复确认幂等。
CREATE TABLE IF NOT EXISTS crew_confirmations (
    zone_id      TEXT NOT NULL,
    zone_version INTEGER NOT NULL,
    crew_id      TEXT NOT NULL,
    shift_id     TEXT NOT NULL DEFAULT '',
    confirmed_by TEXT NOT NULL,
    confirmed_at INTEGER NOT NULL,
    note         TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (zone_id, zone_version, crew_id)
);

-- 区域核对：每区域每版本一条结论。
CREATE TABLE IF NOT EXISTS zone_verifications (
    zone_id      TEXT NOT NULL,
    zone_version INTEGER NOT NULL,
    verified_by  TEXT NOT NULL,
    verified_at  INTEGER NOT NULL,
    result       TEXT NOT NULL,
    PRIMARY KEY (zone_id, zone_version)
);

-- 冻结：同一区域同一原因同一对象只允许一条未解除记录（部分唯一索引）。
CREATE TABLE IF NOT EXISTS freezes (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    zone_id     TEXT NOT NULL,
    reason      TEXT NOT NULL,
    object_key  TEXT NOT NULL DEFAULT '',
    detail      TEXT NOT NULL,
    raised_at   INTEGER NOT NULL,
    raised_by   TEXT NOT NULL,
    resolved_at INTEGER,
    resolved_by TEXT NOT NULL DEFAULT '',
    resolution  TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_freezes_open
    ON freezes(zone_id, reason, object_key) WHERE resolved_at IS NULL;

CREATE TABLE IF NOT EXISTS milestones (
    id        TEXT PRIMARY KEY,
    name      TEXT NOT NULL,
    seq       INTEGER NOT NULL,
    sealed    INTEGER NOT NULL DEFAULT 0,
    sealed_at INTEGER,
    sealed_by TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS milestone_deviations (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    milestone_id TEXT NOT NULL,
    note         TEXT NOT NULL,
    created_by   TEXT NOT NULL,
    created_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS releases (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    conclusion TEXT NOT NULL,
    issued_by  TEXT NOT NULL,
    issued_at  INTEGER NOT NULL,
    basis      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS notifications (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    zone_id    TEXT NOT NULL,
    unit_id    TEXT NOT NULL,
    subject    TEXT NOT NULL,
    body       TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    acked_at   INTEGER,
    acked_by   TEXT NOT NULL DEFAULT ''
);

-- 对象（人员/车辆）统一视图，便于清场核对时取名称与归属。
CREATE VIEW IF NOT EXISTS objects AS
    SELECT 'person' AS object_type, id, name, unit_id, crew_id FROM personnel
    UNION ALL
    SELECT 'vehicle' AS object_type, id, plate || ' (' || kind || ')', unit_id, crew_id FROM vehicles;
`
