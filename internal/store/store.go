// Package store 提供 SQLite 持久化。所有写操作通过幂等约束保证可重入，
// 进程重启后未决清场事项完整保留。
package store

import (
	"database/sql"
	"fmt"
	"time"

	"evac/internal/domain"

	_ "github.com/mattn/go-sqlite3"
)

const timeLayout = time.RFC3339Nano

func fmtTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) time.Time {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Querier 抽象 *sql.DB 与 *sql.Tx，使所有方法可在事务内复用。
type Querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// Store 数据访问层。
type Store struct {
	q  Querier
	db *sql.DB // 仅顶层 Store 持有，用于开启事务与关闭
}

// Open 打开（必要时创建）SQLite 数据库并执行建表。
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_busy_timeout=10000&_journal_mode=WAL&_fk=1&_synchronous=NORMAL", path)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	// 单连接串行化所有访问，避免 SQLITE_BUSY，配合事务保证并发确认/扫码的一致性。
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{q: db, db: db}, nil
}

// Close 关闭底层连接。
func (s *Store) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// Tx 在事务中执行 fn；fn 内必须使用传入的 tx Store。
func (s *Store) Tx(fn func(tx *Store) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	txs := &Store{q: tx}
	if err := fn(txs); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

const schema = `
CREATE TABLE IF NOT EXISTS zones (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS zone_versions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  zone_id TEXT NOT NULL REFERENCES zones(id),
  version INTEGER NOT NULL,
  boundary TEXT NOT NULL,
  expanded INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  UNIQUE(zone_id, version)
);
CREATE TABLE IF NOT EXISTS zone_dependencies (
  zone_id TEXT NOT NULL REFERENCES zones(id),
  depends_on TEXT NOT NULL REFERENCES zones(id),
  PRIMARY KEY(zone_id, depends_on)
);
CREATE TABLE IF NOT EXISTS units (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS crews (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  unit_id TEXT NOT NULL REFERENCES units(id),
  zone_id TEXT NOT NULL REFERENCES zones(id),
  lead_user_id TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS persons (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  post TEXT NOT NULL,
  unit_id TEXT NOT NULL REFERENCES units(id),
  crew_id TEXT NOT NULL REFERENCES crews(id),
  zone_id TEXT NOT NULL REFERENCES zones(id),
  shift_start TEXT NOT NULL,
  shift_end TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS assets (
  id TEXT PRIMARY KEY,
  label TEXT NOT NULL,
  kind TEXT NOT NULL,
  unit_id TEXT NOT NULL REFERENCES units(id),
  zone_id TEXT NOT NULL REFERENCES zones(id)
);
CREATE TABLE IF NOT EXISTS gate_receipts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  scan_id TEXT NOT NULL UNIQUE,
  subject_kind TEXT NOT NULL,
  subject_id TEXT NOT NULL,
  gate TEXT NOT NULL,
  direction TEXT NOT NULL,
  zone_id TEXT NOT NULL,
  zone_version INTEGER NOT NULL,
  scanned_at TEXT NOT NULL,
  received_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_receipts_subject ON gate_receipts(subject_kind, subject_id, scanned_at);
CREATE INDEX IF NOT EXISTS idx_receipts_zone ON gate_receipts(zone_id);
CREATE TABLE IF NOT EXISTS crew_confirmations (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  crew_id TEXT NOT NULL REFERENCES crews(id),
  zone_id TEXT NOT NULL,
  zone_version INTEGER NOT NULL,
  confirmed_by TEXT NOT NULL,
  confirmed_at TEXT NOT NULL,
  UNIQUE(crew_id, zone_id, zone_version)
);
CREATE TABLE IF NOT EXISTS freezes (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  zone_id TEXT NOT NULL,
  reason TEXT NOT NULL,
  detail TEXT NOT NULL,
  active INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  resolved_at TEXT,
  resolved_by TEXT
);
CREATE INDEX IF NOT EXISTS idx_freezes_zone ON freezes(zone_id, active);
CREATE TABLE IF NOT EXISTS releases (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  zone_id TEXT NOT NULL,
  zone_version INTEGER NOT NULL,
  conclusion TEXT NOT NULL,
  verified_by TEXT NOT NULL,
  verified_at TEXT NOT NULL,
  UNIQUE(zone_id, zone_version)
);
CREATE TABLE IF NOT EXISTS milestones (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  zone_id TEXT NOT NULL,
  zone_version INTEGER NOT NULL,
  sealed INTEGER NOT NULL DEFAULT 0,
  sealed_at TEXT,
  conclusion TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS milestone_deviations (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  milestone_id TEXT NOT NULL REFERENCES milestones(id),
  note TEXT NOT NULL,
  author TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS notifications (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  zone_id TEXT NOT NULL,
  zone_version INTEGER NOT NULL,
  unit_id TEXT NOT NULL,
  subject_kind TEXT NOT NULL,
  subject_id TEXT NOT NULL,
  message TEXT NOT NULL,
  created_at TEXT NOT NULL,
  acked_at TEXT,
  acked_by TEXT,
  UNIQUE(zone_id, zone_version, subject_kind, subject_id)
);
`

// ---------- 基础档案 ----------

func (s *Store) InsertZone(z domain.Zone) error {
	_, err := s.q.Exec(`INSERT INTO zones(id,name,created_at) VALUES(?,?,?)`, z.ID, z.Name, fmtTime(z.CreatedAt))
	return err
}

func (s *Store) GetZone(id string) (domain.Zone, error) {
	var z domain.Zone
	var ts string
	err := s.q.QueryRow(`SELECT id,name,created_at FROM zones WHERE id=?`, id).Scan(&z.ID, &z.Name, &ts)
	z.CreatedAt = parseTime(ts)
	return z, err
}

func (s *Store) ListZones() ([]domain.Zone, error) {
	rows, err := s.q.Query(`SELECT id,name,created_at FROM zones ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Zone
	for rows.Next() {
		var z domain.Zone
		var ts string
		if err := rows.Scan(&z.ID, &z.Name, &ts); err != nil {
			return nil, err
		}
		z.CreatedAt = parseTime(ts)
		out = append(out, z)
	}
	return out, rows.Err()
}

func (s *Store) InsertZoneVersion(v domain.ZoneVersion) error {
	_, err := s.q.Exec(`INSERT INTO zone_versions(zone_id,version,boundary,expanded,created_at) VALUES(?,?,?,?,?)`,
		v.ZoneID, v.Version, v.Boundary, v.Expanded, fmtTime(v.CreatedAt))
	return err
}

// CurrentZoneVersion 返回区域当前（最大）版本。
func (s *Store) CurrentZoneVersion(zoneID string) (domain.ZoneVersion, error) {
	var v domain.ZoneVersion
	var ts string
	err := s.q.QueryRow(`SELECT zone_id,version,boundary,expanded,created_at FROM zone_versions
		WHERE zone_id=? ORDER BY version DESC LIMIT 1`, zoneID).
		Scan(&v.ZoneID, &v.Version, &v.Boundary, &v.Expanded, &ts)
	v.CreatedAt = parseTime(ts)
	return v, err
}

func (s *Store) AddDependency(zoneID, dependsOn string) error {
	_, err := s.q.Exec(`INSERT OR IGNORE INTO zone_dependencies(zone_id,depends_on) VALUES(?,?)`, zoneID, dependsOn)
	return err
}

func (s *Store) ListDependencies(zoneID string) ([]string, error) {
	rows, err := s.q.Query(`SELECT depends_on FROM zone_dependencies WHERE zone_id=? ORDER BY depends_on`, zoneID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) InsertUnit(u domain.Unit) error {
	_, err := s.q.Exec(`INSERT INTO units(id,name) VALUES(?,?)`, u.ID, u.Name)
	return err
}

func (s *Store) UnitNames() (map[string]string, error) {
	rows, err := s.q.Query(`SELECT id,name FROM units`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

func (s *Store) InsertCrew(c domain.Crew) error {
	_, err := s.q.Exec(`INSERT INTO crews(id,name,unit_id,zone_id,lead_user_id) VALUES(?,?,?,?,?)`,
		c.ID, c.Name, c.UnitID, c.ZoneID, c.LeadUserID)
	return err
}

func (s *Store) GetCrew(id string) (domain.Crew, error) {
	var c domain.Crew
	err := s.q.QueryRow(`SELECT id,name,unit_id,zone_id,lead_user_id FROM crews WHERE id=?`, id).
		Scan(&c.ID, &c.Name, &c.UnitID, &c.ZoneID, &c.LeadUserID)
	return c, err
}

func (s *Store) ListCrewsByZone(zoneID string) ([]domain.Crew, error) {
	rows, err := s.q.Query(`SELECT id,name,unit_id,zone_id,lead_user_id FROM crews WHERE zone_id=? ORDER BY id`, zoneID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Crew
	for rows.Next() {
		var c domain.Crew
		if err := rows.Scan(&c.ID, &c.Name, &c.UnitID, &c.ZoneID, &c.LeadUserID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) InsertPerson(p domain.Person) error {
	_, err := s.q.Exec(`INSERT INTO persons(id,name,post,unit_id,crew_id,zone_id,shift_start,shift_end) VALUES(?,?,?,?,?,?,?,?)`,
		p.ID, p.Name, p.Post, p.UnitID, p.CrewID, p.ZoneID, p.ShiftStart, p.ShiftEnd)
	return err
}

func (s *Store) GetPerson(id string) (domain.Person, error) {
	var p domain.Person
	err := s.q.QueryRow(`SELECT id,name,post,unit_id,crew_id,zone_id,shift_start,shift_end FROM persons WHERE id=?`, id).
		Scan(&p.ID, &p.Name, &p.Post, &p.UnitID, &p.CrewID, &p.ZoneID, &p.ShiftStart, &p.ShiftEnd)
	return p, err
}

// ListPersons 按可选条件过滤；空字符串表示不过滤。
func (s *Store) ListPersons(zoneID, unitID, crewID string) ([]domain.Person, error) {
	rows, err := s.q.Query(`SELECT id,name,post,unit_id,crew_id,zone_id,shift_start,shift_end FROM persons
		WHERE (?='' OR zone_id=?) AND (?='' OR unit_id=?) AND (?='' OR crew_id=?) ORDER BY id`,
		zoneID, zoneID, unitID, unitID, crewID, crewID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Person
	for rows.Next() {
		var p domain.Person
		if err := rows.Scan(&p.ID, &p.Name, &p.Post, &p.UnitID, &p.CrewID, &p.ZoneID, &p.ShiftStart, &p.ShiftEnd); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) InsertAsset(a domain.Asset) error {
	_, err := s.q.Exec(`INSERT INTO assets(id,label,kind,unit_id,zone_id) VALUES(?,?,?,?,?)`,
		a.ID, a.Label, a.Kind, a.UnitID, a.ZoneID)
	return err
}

func (s *Store) GetAsset(id string) (domain.Asset, error) {
	var a domain.Asset
	err := s.q.QueryRow(`SELECT id,label,kind,unit_id,zone_id FROM assets WHERE id=?`, id).
		Scan(&a.ID, &a.Label, &a.Kind, &a.UnitID, &a.ZoneID)
	return a, err
}

func (s *Store) ListAssetsByZone(zoneID string) ([]domain.Asset, error) {
	rows, err := s.q.Query(`SELECT id,label,kind,unit_id,zone_id FROM assets WHERE zone_id=? ORDER BY id`, zoneID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Asset
	for rows.Next() {
		var a domain.Asset
		if err := rows.Scan(&a.ID, &a.Label, &a.Kind, &a.UnitID, &a.ZoneID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---------- 门禁回执 ----------

// InsertReceiptIdempotent 按 scan_id 幂等写入；返回是否真正插入。
func (s *Store) InsertReceiptIdempotent(r domain.GateReceipt) (bool, error) {
	res, err := s.q.Exec(`INSERT OR IGNORE INTO gate_receipts
		(scan_id,subject_kind,subject_id,gate,direction,zone_id,zone_version,scanned_at,received_at)
		VALUES(?,?,?,?,?,?,?,?,?)`,
		r.ScanID, r.SubjectKind, r.SubjectID, r.Gate, string(r.Direction), r.ZoneID, r.ZoneVersion,
		fmtTime(r.ScannedAt), fmtTime(r.ReceivedAt))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) GetReceiptByScanID(scanID string) (domain.GateReceipt, error) {
	return scanReceipt(s.q.QueryRow(`SELECT id,scan_id,subject_kind,subject_id,gate,direction,zone_id,zone_version,scanned_at,received_at
		FROM gate_receipts WHERE scan_id=?`, scanID))
}

func scanReceipt(row *sql.Row) (domain.GateReceipt, error) {
	var r domain.GateReceipt
	var dir, scanned, received string
	err := row.Scan(&r.ID, &r.ScanID, &r.SubjectKind, &r.SubjectID, &r.Gate, &dir, &r.ZoneID, &r.ZoneVersion, &scanned, &received)
	r.Direction = domain.Direction(dir)
	r.ScannedAt = parseTime(scanned)
	r.ReceivedAt = parseTime(received)
	return r, err
}

// LastReceipt 返回对象最近一次门禁事件（按扫码时间，再按插入序）。
func (s *Store) LastReceipt(subjectKind, subjectID string) (domain.GateReceipt, bool, error) {
	r, err := scanReceipt(s.q.QueryRow(`SELECT id,scan_id,subject_kind,subject_id,gate,direction,zone_id,zone_version,scanned_at,received_at
		FROM gate_receipts WHERE subject_kind=? AND subject_id=? ORDER BY scanned_at DESC, id DESC LIMIT 1`,
		subjectKind, subjectID))
	if err == sql.ErrNoRows {
		return domain.GateReceipt{}, false, nil
	}
	if err != nil {
		return domain.GateReceipt{}, false, err
	}
	return r, true, nil
}

func (s *Store) ListReceiptsByZone(zoneID string) ([]domain.GateReceipt, error) {
	rows, err := s.q.Query(`SELECT id,scan_id,subject_kind,subject_id,gate,direction,zone_id,zone_version,scanned_at,received_at
		FROM gate_receipts WHERE zone_id=? ORDER BY scanned_at ASC, id ASC`, zoneID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.GateReceipt
	for rows.Next() {
		var r domain.GateReceipt
		var dir, scanned, received string
		if err := rows.Scan(&r.ID, &r.ScanID, &r.SubjectKind, &r.SubjectID, &r.Gate, &dir, &r.ZoneID, &r.ZoneVersion, &scanned, &received); err != nil {
			return nil, err
		}
		r.Direction = domain.Direction(dir)
		r.ScannedAt = parseTime(scanned)
		r.ReceivedAt = parseTime(received)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) CountReceipts() (int, error) {
	var n int
	err := s.q.QueryRow(`SELECT COUNT(*) FROM gate_receipts`).Scan(&n)
	return n, err
}

// ---------- 班组确认 ----------

// InsertConfirmationIdempotent 按 (crew,zone,version) 幂等写入。
func (s *Store) InsertConfirmationIdempotent(c domain.CrewConfirmation) (bool, error) {
	res, err := s.q.Exec(`INSERT OR IGNORE INTO crew_confirmations(crew_id,zone_id,zone_version,confirmed_by,confirmed_at)
		VALUES(?,?,?,?,?)`, c.CrewID, c.ZoneID, c.ZoneVersion, c.ConfirmedBy, fmtTime(c.ConfirmedAt))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) ListConfirmations(zoneID string, version int) ([]domain.CrewConfirmation, error) {
	rows, err := s.q.Query(`SELECT id,crew_id,zone_id,zone_version,confirmed_by,confirmed_at
		FROM crew_confirmations WHERE zone_id=? AND zone_version=? ORDER BY crew_id`, zoneID, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.CrewConfirmation
	for rows.Next() {
		var c domain.CrewConfirmation
		var ts string
		if err := rows.Scan(&c.ID, &c.CrewID, &c.ZoneID, &c.ZoneVersion, &c.ConfirmedBy, &ts); err != nil {
			return nil, err
		}
		c.ConfirmedAt = parseTime(ts)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) CountConfirmations() (int, error) {
	var n int
	err := s.q.QueryRow(`SELECT COUNT(*) FROM crew_confirmations`).Scan(&n)
	return n, err
}

// ---------- 冻结 ----------

func (s *Store) InsertFreeze(f domain.Freeze) (int64, error) {
	res, err := s.q.Exec(`INSERT INTO freezes(zone_id,reason,detail,active,created_at) VALUES(?,?,?,1,?)`,
		f.ZoneID, string(f.Reason), f.Detail, fmtTime(f.CreatedAt))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) GetFreeze(id int64) (domain.Freeze, error) {
	return scanFreeze(s.q.QueryRow(`SELECT id,zone_id,reason,detail,active,created_at,resolved_at,resolved_by FROM freezes WHERE id=?`, id))
}

func scanFreeze(row *sql.Row) (domain.Freeze, error) {
	var f domain.Freeze
	var reason, created string
	var resolvedAt, resolvedBy sql.NullString
	var active int
	err := row.Scan(&f.ID, &f.ZoneID, &reason, &f.Detail, &active, &created, &resolvedAt, &resolvedBy)
	f.Reason = domain.FreezeReason(reason)
	f.Active = active == 1
	f.CreatedAt = parseTime(created)
	if resolvedAt.Valid {
		t := parseTime(resolvedAt.String)
		f.ResolvedAt = &t
	}
	f.ResolvedBy = resolvedBy.String
	return f, err
}

func (s *Store) ActiveFreezes(zoneID string) ([]domain.Freeze, error) {
	rows, err := s.q.Query(`SELECT id,zone_id,reason,detail,active,created_at,resolved_at,resolved_by
		FROM freezes WHERE zone_id=? AND active=1 ORDER BY id`, zoneID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Freeze
	for rows.Next() {
		var f domain.Freeze
		var reason, created string
		var resolvedAt, resolvedBy sql.NullString
		var active int
		if err := rows.Scan(&f.ID, &f.ZoneID, &reason, &f.Detail, &active, &created, &resolvedAt, &resolvedBy); err != nil {
			return nil, err
		}
		f.Reason = domain.FreezeReason(reason)
		f.Active = active == 1
		f.CreatedAt = parseTime(created)
		if resolvedAt.Valid {
			t := parseTime(resolvedAt.String)
			f.ResolvedAt = &t
		}
		f.ResolvedBy = resolvedBy.String
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) ResolveFreeze(id int64, by string, at time.Time) error {
	_, err := s.q.Exec(`UPDATE freezes SET active=0, resolved_at=?, resolved_by=? WHERE id=? AND active=1`,
		fmtTime(at), by, id)
	return err
}

// ---------- 放行 ----------

// InsertReleaseIdempotent 按 (zone,version) 幂等写入放行结论。
func (s *Store) InsertReleaseIdempotent(r domain.Release) (bool, error) {
	res, err := s.q.Exec(`INSERT OR IGNORE INTO releases(zone_id,zone_version,conclusion,verified_by,verified_at)
		VALUES(?,?,?,?,?)`, r.ZoneID, r.ZoneVersion, r.Conclusion, r.VerifiedBy, fmtTime(r.VerifiedAt))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) GetRelease(zoneID string, version int) (domain.Release, bool, error) {
	var r domain.Release
	var ts string
	err := s.q.QueryRow(`SELECT zone_id,zone_version,conclusion,verified_by,verified_at FROM releases
		WHERE zone_id=? AND zone_version=?`, zoneID, version).
		Scan(&r.ZoneID, &r.ZoneVersion, &r.Conclusion, &r.VerifiedBy, &ts)
	if err == sql.ErrNoRows {
		return domain.Release{}, false, nil
	}
	if err != nil {
		return domain.Release{}, false, err
	}
	r.VerifiedAt = parseTime(ts)
	return r, true, nil
}

func (s *Store) ListReleases() ([]domain.Release, error) {
	rows, err := s.q.Query(`SELECT zone_id,zone_version,conclusion,verified_by,verified_at FROM releases ORDER BY verified_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Release
	for rows.Next() {
		var r domain.Release
		var ts string
		if err := rows.Scan(&r.ZoneID, &r.ZoneVersion, &r.Conclusion, &r.VerifiedBy, &ts); err != nil {
			return nil, err
		}
		r.VerifiedAt = parseTime(ts)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------- 里程碑 ----------

func (s *Store) UpsertMilestone(m domain.Milestone) error {
	var sealedAt any
	if m.SealedAt != nil {
		sealedAt = fmtTime(*m.SealedAt)
	}
	_, err := s.q.Exec(`INSERT INTO milestones(id,name,zone_id,zone_version,sealed,sealed_at,conclusion)
		VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, sealed=excluded.sealed, sealed_at=excluded.sealed_at, conclusion=excluded.conclusion`,
		m.ID, m.Name, m.ZoneID, m.ZoneVersion, m.Sealed, sealedAt, m.Conclusion)
	return err
}

func (s *Store) GetMilestone(id string) (domain.Milestone, error) {
	var m domain.Milestone
	var sealed int
	var sealedAt sql.NullString
	err := s.q.QueryRow(`SELECT id,name,zone_id,zone_version,sealed,sealed_at,conclusion FROM milestones WHERE id=?`, id).
		Scan(&m.ID, &m.Name, &m.ZoneID, &m.ZoneVersion, &sealed, &sealedAt, &m.Conclusion)
	m.Sealed = sealed == 1
	if sealedAt.Valid {
		t := parseTime(sealedAt.String)
		m.SealedAt = &t
	}
	return m, err
}

func (s *Store) ListMilestones() ([]domain.Milestone, error) {
	rows, err := s.q.Query(`SELECT id,name,zone_id,zone_version,sealed,sealed_at,conclusion FROM milestones ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Milestone
	for rows.Next() {
		var m domain.Milestone
		var sealed int
		var sealedAt sql.NullString
		if err := rows.Scan(&m.ID, &m.Name, &m.ZoneID, &m.ZoneVersion, &sealed, &sealedAt, &m.Conclusion); err != nil {
			return nil, err
		}
		m.Sealed = sealed == 1
		if sealedAt.Valid {
			t := parseTime(sealedAt.String)
			m.SealedAt = &t
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// UpdateMilestoneDraft 仅允许修改未封存里程碑；返回是否命中未封存行。
func (s *Store) UpdateMilestoneDraft(id, name, conclusion string) (bool, error) {
	res, err := s.q.Exec(`UPDATE milestones SET name=?, conclusion=? WHERE id=? AND sealed=0`, name, conclusion, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) SealMilestone(id string, at time.Time) error {
	_, err := s.q.Exec(`UPDATE milestones SET sealed=1, sealed_at=? WHERE id=?`, fmtTime(at), id)
	return err
}

func (s *Store) AppendDeviation(d domain.Deviation) (int64, error) {
	res, err := s.q.Exec(`INSERT INTO milestone_deviations(milestone_id,note,author,created_at) VALUES(?,?,?,?)`,
		d.MilestoneID, d.Note, d.Author, fmtTime(d.CreatedAt))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) ListDeviations(milestoneID string) ([]domain.Deviation, error) {
	rows, err := s.q.Query(`SELECT id,milestone_id,note,author,created_at FROM milestone_deviations WHERE milestone_id=? ORDER BY id`, milestoneID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Deviation
	for rows.Next() {
		var d domain.Deviation
		var ts string
		if err := rows.Scan(&d.ID, &d.MilestoneID, &d.Note, &d.Author, &ts); err != nil {
			return nil, err
		}
		d.CreatedAt = parseTime(ts)
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---------- 通知 ----------

// UpsertNotification 按 (zone,version,subject) 幂等写入通知。
func (s *Store) UpsertNotification(n domain.Notification) (bool, error) {
	res, err := s.q.Exec(`INSERT OR IGNORE INTO notifications
		(zone_id,zone_version,unit_id,subject_kind,subject_id,message,created_at) VALUES(?,?,?,?,?,?,?)`,
		n.ZoneID, n.ZoneVersion, n.UnitID, n.SubjectKind, n.SubjectID, n.Message, fmtTime(n.CreatedAt))
	if err != nil {
		return false, err
	}
	cnt, err := res.RowsAffected()
	return cnt > 0, err
}

func (s *Store) GetNotification(id int64) (domain.Notification, error) {
	return scanNotification(s.q.QueryRow(`SELECT id,zone_id,zone_version,unit_id,subject_kind,subject_id,message,created_at,acked_at,acked_by
		FROM notifications WHERE id=?`, id))
}

func scanNotification(row *sql.Row) (domain.Notification, error) {
	var n domain.Notification
	var created string
	var ackedAt, ackedBy sql.NullString
	err := row.Scan(&n.ID, &n.ZoneID, &n.ZoneVersion, &n.UnitID, &n.SubjectKind, &n.SubjectID, &n.Message, &created, &ackedAt, &ackedBy)
	n.CreatedAt = parseTime(created)
	if ackedAt.Valid {
		t := parseTime(ackedAt.String)
		n.AckedAt = &t
	}
	n.AckedBy = ackedBy.String
	return n, err
}

// ListNotifications 按区域/单位过滤；空字符串表示不过滤。
func (s *Store) ListNotifications(zoneID, unitID string) ([]domain.Notification, error) {
	rows, err := s.q.Query(`SELECT id,zone_id,zone_version,unit_id,subject_kind,subject_id,message,created_at,acked_at,acked_by
		FROM notifications WHERE (?='' OR zone_id=?) AND (?='' OR unit_id=?) ORDER BY id`, zoneID, zoneID, unitID, unitID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Notification
	for rows.Next() {
		var n domain.Notification
		var created string
		var ackedAt, ackedBy sql.NullString
		if err := rows.Scan(&n.ID, &n.ZoneID, &n.ZoneVersion, &n.UnitID, &n.SubjectKind, &n.SubjectID, &n.Message, &created, &ackedAt, &ackedBy); err != nil {
			return nil, err
		}
		n.CreatedAt = parseTime(created)
		if ackedAt.Valid {
			t := parseTime(ackedAt.String)
			n.AckedAt = &t
		}
		n.AckedBy = ackedBy.String
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) AckNotification(id int64, by string, at time.Time) error {
	_, err := s.q.Exec(`UPDATE notifications SET acked_at=?, acked_by=? WHERE id=?`, fmtTime(at), by, id)
	return err
}
