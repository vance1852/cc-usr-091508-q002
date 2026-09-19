package core

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"time"
)

// querier 抽象 *sql.DB 与 *sql.Tx，便于在事务内外复用查询。
type querier interface {
	QueryRow(query string, args ...any) *sql.Row
}

// Service 勤务区撤离放行核心服务。所有状态持久化于 SQLite，
// 进程重启后未决清场事项（未撤离对象、未解除冻结）自动恢复。
type Service struct {
	db  *sql.DB
	now func() time.Time
}

// NewService 构造服务，clock 为 nil 时使用系统时钟（测试可注入固定时钟模拟跨午夜班次）。
func NewService(db *sql.DB, clock func() time.Time) *Service {
	if clock == nil {
		clock = time.Now
	}
	return &Service{db: db, now: clock}
}

func (s *Service) nowUnix() int64 { return s.now().Unix() }

// ---------------------------------------------------------------------------
// 基础查询
// ---------------------------------------------------------------------------

// GetActor 按 ID 取操作者（认证中间件使用）。
func (s *Service) GetActor(id string) (Actor, error) {
	var a Actor
	err := s.db.QueryRow(`SELECT id, name, role, unit_id, crew_id FROM actors WHERE id = ?`, id).
		Scan(&a.ID, &a.Name, &a.Role, &a.UnitID, &a.CrewID)
	if errors.Is(err, sql.ErrNoRows) {
		return a, NotFound("操作者 %s 不存在", id)
	}
	return a, err
}

// ListZones 列出全部危险区。
func (s *Service) ListZones() ([]Zone, error) {
	rows, err := s.db.Query(`SELECT id, name FROM zones ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Zone{}
	for rows.Next() {
		var z Zone
		if err := rows.Scan(&z.ID, &z.Name); err != nil {
			return nil, err
		}
		out = append(out, z)
	}
	return out, rows.Err()
}

func (s *Service) currentZoneVersion(q querier, zoneID string) (ZoneVersion, error) {
	var v ZoneVersion
	var expanded int
	err := q.QueryRow(`SELECT zone_id, version, boundary, expanded, created_at
		FROM zone_versions WHERE zone_id = ? ORDER BY version DESC LIMIT 1`, zoneID).
		Scan(&v.ZoneID, &v.Version, &v.Boundary, &expanded, &v.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return v, NotFound("危险区 %s 不存在或无边界版本", zoneID)
	}
	v.Expanded = expanded != 0
	return v, err
}

// ShiftFor 返回时刻 ts 所属班次。跨午夜班次（如 22:00–次日 06:00）
// 以绝对时间戳区间判定，23:59 与 00:01 自然同属一班。
func (s *Service) ShiftFor(ts int64) (*Shift, error) {
	return shiftFor(s.db, ts)
}

func shiftFor(q querier, ts int64) (*Shift, error) {
	var sh Shift
	err := q.QueryRow(`SELECT id, name, starts_at, ends_at FROM shifts
		WHERE starts_at <= ? AND ? < ends_at ORDER BY starts_at DESC LIMIT 1`, ts, ts).
		Scan(&sh.ID, &sh.Name, &sh.StartsAt, &sh.EndsAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sh, nil
}

// ShiftReceipts 统计班次窗口内的门禁回执（跨午夜正确归属）。
func (s *Service) ShiftReceipts(shiftID string) (Shift, []GateEvent, error) {
	var sh Shift
	err := s.db.QueryRow(`SELECT id, name, starts_at, ends_at FROM shifts WHERE id = ?`, shiftID).
		Scan(&sh.ID, &sh.Name, &sh.StartsAt, &sh.EndsAt)
	if errors.Is(err, sql.ErrNoRows) {
		return sh, nil, NotFound("班次 %s 不存在", shiftID)
	}
	if err != nil {
		return sh, nil, err
	}
	rows, err := s.db.Query(`SELECT id, receipt_no, gate_id, zone_id, object_type, object_id, direction, occurred_at, received_at
		FROM gate_events WHERE occurred_at >= ? AND occurred_at < ? ORDER BY occurred_at, id`, sh.StartsAt, sh.EndsAt)
	if err != nil {
		return sh, nil, err
	}
	defer rows.Close()
	events := []GateEvent{}
	for rows.Next() {
		var e GateEvent
		if err := rows.Scan(&e.ID, &e.ReceiptNo, &e.GateID, &e.ZoneID, &e.ObjectType, &e.ObjectID, &e.Direction, &e.OccurredAt, &e.ReceivedAt); err != nil {
			return sh, nil, err
		}
		events = append(events, e)
	}
	return sh, events, rows.Err()
}

// ---------------------------------------------------------------------------
// 门禁回执
// ---------------------------------------------------------------------------

// RecordGateEvent 录入门禁回执。同一 receipt_no 重复提交幂等返回已有记录；
// 若对象在班组确认后重新进入危险区，自动冻结该区域。
func (s *Service) RecordGateEvent(ev GateEvent) (GateEvent, error) {
	if ev.ReceiptNo == "" || ev.ZoneID == "" || ev.ObjectID == "" {
		return ev, BadRequest("receipt_no、zone_id、object_id 必填")
	}
	if ev.Direction != DirIn && ev.Direction != DirOut {
		return ev, BadRequest("direction 必须为 in 或 out")
	}
	if ev.ObjectType != ObjectPerson && ev.ObjectType != ObjectVehicle {
		return ev, BadRequest("object_type 必须为 person 或 vehicle")
	}
	if ev.OccurredAt == 0 {
		return ev, BadRequest("occurred_at（门禁时间）必填")
	}
	ev.ReceivedAt = s.nowUnix()

	tx, err := s.db.Begin()
	if err != nil {
		return ev, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`INSERT OR IGNORE INTO gate_events
		(receipt_no, gate_id, zone_id, object_type, object_id, direction, occurred_at, received_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		ev.ReceiptNo, ev.GateID, ev.ZoneID, ev.ObjectType, ev.ObjectID, ev.Direction, ev.OccurredAt, ev.ReceivedAt)
	if err != nil {
		return ev, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// 重复扫码：回查已有回执，幂等返回，不重复计数。
		var existing GateEvent
		err = tx.QueryRow(`SELECT id, receipt_no, gate_id, zone_id, object_type, object_id, direction, occurred_at, received_at
			FROM gate_events WHERE receipt_no = ?`, ev.ReceiptNo).
			Scan(&existing.ID, &existing.ReceiptNo, &existing.GateID, &existing.ZoneID, &existing.ObjectType, &existing.ObjectID, &existing.Direction, &existing.OccurredAt, &existing.ReceivedAt)
		if err != nil {
			return ev, err
		}
		existing.Duplicate = true
		return existing, tx.Commit()
	}
	ev.ID, _ = res.LastInsertId()

	// 重新进入检测：对象所属班组已确认当前版本（或区域已核对）后出现的进入事件 → 冻结。
	if ev.Direction == DirIn {
		if err := s.raiseReentryFreezeIfNeeded(tx, ev); err != nil {
			return ev, err
		}
	}
	return ev, tx.Commit()
}

func (s *Service) raiseReentryFreezeIfNeeded(tx *sql.Tx, ev GateEvent) error {
	ver, err := s.currentZoneVersion(tx, ev.ZoneID)
	if err != nil {
		var ae *Error
		if errors.As(err, &ae) {
			return nil // 区域未建模，不阻断回执录入
		}
		return err
	}
	var crewID string
	err = tx.QueryRow(`SELECT crew_id FROM objects WHERE object_type = ? AND id = ?`, ev.ObjectType, ev.ObjectID).Scan(&crewID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // 非名册对象
	}
	if err != nil {
		return err
	}
	var confirmed int
	if err := tx.QueryRow(`SELECT COUNT(1) FROM crew_confirmations WHERE zone_id = ? AND zone_version = ? AND crew_id = ?`,
		ev.ZoneID, ver.Version, crewID).Scan(&confirmed); err != nil {
		return err
	}
	var verified int
	if err := tx.QueryRow(`SELECT COUNT(1) FROM zone_verifications WHERE zone_id = ? AND zone_version = ?`,
		ev.ZoneID, ver.Version).Scan(&verified); err != nil {
		return err
	}
	if confirmed == 0 && verified == 0 {
		return nil
	}
	return s.raiseFreeze(tx, ev.ZoneID, FreezeReentry, string(ev.ObjectType)+":"+ev.ObjectID,
		"对象 "+string(ev.ObjectType)+":"+ev.ObjectID+" 在班组确认/区域核对后重新进入危险区（回执 "+ev.ReceiptNo+"）", "system")
}

// raiseFreeze 登记冻结；同区域同原因同对象的未解除冻结只保留一条。
func (s *Service) raiseFreeze(tx *sql.Tx, zoneID, reason, objectKey, detail, raisedBy string) error {
	_, err := tx.Exec(`INSERT INTO freezes (zone_id, reason, object_key, detail, raised_at, raised_by)
		SELECT ?,?,?,?,?,?
		WHERE NOT EXISTS (SELECT 1 FROM freezes WHERE zone_id = ? AND reason = ? AND object_key = ? AND resolved_at IS NULL)`,
		zoneID, reason, objectKey, detail, s.nowUnix(), raisedBy,
		zoneID, reason, objectKey)
	return err
}

// ---------------------------------------------------------------------------
// 班组确认 / 区域核对 / 放行
// ---------------------------------------------------------------------------

// ConfirmCrew 班组负责人确认本组在某区域当前版本已撤离。
// 前置条件：区域无未解除冻结；本组派遣对象全部有回执且已净离场。
// 重复确认幂等返回已有记录。
func (s *Service) ConfirmCrew(actor Actor, zoneID, crewID, note string) (CrewConfirmation, error) {
	var cc CrewConfirmation
	if actor.Role != RoleCrewLead || actor.CrewID != crewID {
		return cc, Forbidden("只有本班组负责人可以确认班组 %s 的状态", crewID)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return cc, err
	}
	defer tx.Rollback()

	ver, err := s.currentZoneVersion(tx, zoneID)
	if err != nil {
		return cc, err
	}
	// 先同步缺回执冻结，再做冻结检查：不依赖看板是否被查看过。
	if err := s.syncMissingReceiptFreezes(tx, zoneID); err != nil {
		return cc, err
	}
	if err := s.ensureNoOpenFreezes(tx, zoneID); err != nil {
		return cc, err
	}
	pending, err := s.pendingObjects(tx, zoneID, crewID)
	if err != nil {
		return cc, err
	}
	if len(pending) > 0 {
		return cc, ConflictWithDetails(pending, "班组 %s 在区域 %s 尚有 %d 个对象未撤离或缺少回执，不得确认", crewID, zoneID, len(pending))
	}

	now := s.nowUnix()
	shiftID := ""
	if sh, err := shiftFor(tx, now); err != nil {
		return cc, err
	} else if sh != nil {
		shiftID = sh.ID
	}
	_, err = tx.Exec(`INSERT OR IGNORE INTO crew_confirmations
		(zone_id, zone_version, crew_id, shift_id, confirmed_by, confirmed_at, note)
		VALUES (?,?,?,?,?,?,?)`, zoneID, ver.Version, crewID, shiftID, actor.ID, now, note)
	if err != nil {
		return cc, err
	}
	err = tx.QueryRow(`SELECT zone_id, zone_version, crew_id, shift_id, confirmed_by, confirmed_at, note
		FROM crew_confirmations WHERE zone_id = ? AND zone_version = ? AND crew_id = ?`, zoneID, ver.Version, crewID).
		Scan(&cc.ZoneID, &cc.ZoneVersion, &cc.CrewID, &cc.ShiftID, &cc.ConfirmedBy, &cc.ConfirmedAt, &cc.Note)
	if err != nil {
		return cc, err
	}
	return cc, tx.Commit()
}

// VerifyZone 地面保障指挥核对区域：当前版本无冻结、无未撤离对象、
// 涉及班组全部确认、依赖区域均已清场，方可核对通过。
func (s *Service) VerifyZone(actor Actor, zoneID string) (ZoneVerification, error) {
	var zv ZoneVerification
	if actor.Role != RoleGroundCommander {
		return zv, Forbidden("只有地面保障指挥可以核对区域")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return zv, err
	}
	defer tx.Rollback()

	ver, err := s.currentZoneVersion(tx, zoneID)
	if err != nil {
		return zv, err
	}
	if err := s.syncMissingReceiptFreezes(tx, zoneID); err != nil {
		return zv, err
	}
	if err := s.ensureNoOpenFreezes(tx, zoneID); err != nil {
		return zv, err
	}
	pending, err := s.pendingObjects(tx, zoneID, "")
	if err != nil {
		return zv, err
	}
	if len(pending) > 0 {
		return zv, ConflictWithDetails(pending, "区域 %s 尚有 %d 个未撤离对象，不得核对放行", zoneID, len(pending))
	}
	// 涉及班组必须全部确认当前版本。
	rows, err := tx.Query(`SELECT DISTINCT crew_id FROM assignments WHERE zone_id = ? AND active = 1`, zoneID)
	if err != nil {
		return zv, err
	}
	var crews []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return zv, err
		}
		crews = append(crews, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return zv, err
	}
	for _, c := range crews {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(1) FROM crew_confirmations WHERE zone_id = ? AND zone_version = ? AND crew_id = ?`,
			zoneID, ver.Version, c).Scan(&n); err != nil {
			return zv, err
		}
		if n == 0 {
			return zv, Conflict("区域 %s 当前版本 v%d 尚缺班组 %s 的确认", zoneID, ver.Version, c)
		}
	}
	// 依赖区域必须已清场。
	deps, err := s.zoneDependencies(tx, zoneID)
	if err != nil {
		return zv, err
	}
	for _, d := range deps {
		if !d.Cleared {
			return zv, Conflict("依赖区域 %s 尚未清场，区域 %s 不得核对", d.ZoneID, zoneID)
		}
	}

	now := s.nowUnix()
	_, err = tx.Exec(`INSERT INTO zone_verifications (zone_id, zone_version, verified_by, verified_at, result)
		VALUES (?,?,?,?,'cleared')
		ON CONFLICT(zone_id, zone_version) DO UPDATE SET verified_by = excluded.verified_by,
			verified_at = excluded.verified_at, result = excluded.result`,
		zoneID, ver.Version, actor.ID, now)
	if err != nil {
		return zv, err
	}
	zv = ZoneVerification{ZoneID: zoneID, ZoneVersion: ver.Version, VerifiedBy: actor.ID, VerifiedAt: now, Result: "cleared"}
	return zv, tx.Commit()
}

// ExpandZoneBoundary 登记区域边界新版本。边界扩大时自动冻结该区域；
// 班组确认与区域核对均绑定旧版本，自然失效。
func (s *Service) ExpandZoneBoundary(actor Actor, zoneID, boundary string, expanded bool) (ZoneVersion, error) {
	var v ZoneVersion
	if actor.Role != RoleGroundCommander {
		return v, Forbidden("只有地面保障指挥可以变更区域边界")
	}
	if boundary == "" {
		return v, BadRequest("boundary 必填")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return v, err
	}
	defer tx.Rollback()
	cur, err := s.currentZoneVersion(tx, zoneID)
	if err != nil {
		return v, err
	}
	exp := 0
	if expanded {
		exp = 1
	}
	now := s.nowUnix()
	_, err = tx.Exec(`INSERT INTO zone_versions (zone_id, version, boundary, expanded, created_at) VALUES (?,?,?,?,?)`,
		zoneID, cur.Version+1, boundary, exp, now)
	if err != nil {
		return v, err
	}
	if expanded {
		if err := s.raiseFreeze(tx, zoneID, FreezeBoundaryExpanded, "",
			"区域边界由 v"+itoa(cur.Version)+" 扩大至 v"+itoa(cur.Version+1)+"，既有确认作废，须重新清场", "system"); err != nil {
			return v, err
		}
	}
	v = ZoneVersion{ZoneID: zoneID, Version: cur.Version + 1, Boundary: boundary, Expanded: expanded, CreatedAt: now}
	return v, tx.Commit()
}

// ResolveFreeze 地面保障指挥排除原因后解除冻结。
func (s *Service) ResolveFreeze(actor Actor, id int64, resolution string) error {
	if actor.Role != RoleGroundCommander {
		return Forbidden("只有地面保障指挥可以解除冻结")
	}
	if resolution == "" {
		return BadRequest("resolution（解除说明）必填")
	}
	res, err := s.db.Exec(`UPDATE freezes SET resolved_at = ?, resolved_by = ?, resolution = ?
		WHERE id = ? AND resolved_at IS NULL`, s.nowUnix(), actor.ID, resolution, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return NotFound("冻结 %d 不存在或已解除", id)
	}
	return nil
}

// ListFreezes 冻结列表；openOnly 为 true 时只看未解除。
func (s *Service) ListFreezes(openOnly bool) ([]Freeze, error) {
	q := `SELECT id, zone_id, reason, object_key, detail, raised_at, raised_by, resolved_at, resolved_by, resolution FROM freezes`
	if openOnly {
		q += ` WHERE resolved_at IS NULL`
	}
	q += ` ORDER BY id`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFreezes(rows)
}

func scanFreezes(rows *sql.Rows) ([]Freeze, error) {
	out := []Freeze{}
	for rows.Next() {
		var f Freeze
		var resolvedAt sql.NullInt64
		if err := rows.Scan(&f.ID, &f.ZoneID, &f.Reason, &f.ObjectKey, &f.Detail, &f.RaisedAt, &f.RaisedBy,
			&resolvedAt, &f.ResolvedBy, &f.Resolution); err != nil {
			return nil, err
		}
		if resolvedAt.Valid {
			f.ResolvedAt = &resolvedAt.Int64
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// IssueRelease 发布最终放行结论：全部危险区当前版本均已核对清场且无未解除冻结。
func (s *Service) IssueRelease(actor Actor, conclusion string) (Release, error) {
	var r Release
	if actor.Role != RoleGroundCommander {
		return r, Forbidden("只有地面保障指挥可以发布放行结论")
	}
	if conclusion == "" {
		return r, BadRequest("conclusion（放行结论）必填")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return r, err
	}
	defer tx.Rollback()

	zrows, err := tx.Query(`SELECT id, name FROM zones ORDER BY id`)
	if err != nil {
		return r, err
	}
	var zones []Zone
	for zrows.Next() {
		var z Zone
		if err := zrows.Scan(&z.ID, &z.Name); err != nil {
			zrows.Close()
			return r, err
		}
		zones = append(zones, z)
	}
	zrows.Close()
	if err := zrows.Err(); err != nil {
		return r, err
	}
	type basisEntry struct {
		ZoneID     string `json:"zone_id"`
		Version    int    `json:"version"`
		VerifiedAt int64  `json:"verified_at"`
	}
	var basis []basisEntry
	for _, z := range zones {
		ver, err := s.currentZoneVersion(tx, z.ID)
		if err != nil {
			return r, err
		}
		if err := s.syncMissingReceiptFreezes(tx, z.ID); err != nil {
			return r, err
		}
		if err := s.ensureNoOpenFreezes(tx, z.ID); err != nil {
			return r, err
		}
		var verifiedAt int64
		err = tx.QueryRow(`SELECT verified_at FROM zone_verifications WHERE zone_id = ? AND zone_version = ? AND result = 'cleared'`,
			z.ID, ver.Version).Scan(&verifiedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return r, Conflict("区域 %s 当前版本 v%d 尚未核对清场，不得发布放行", z.ID, ver.Version)
		}
		if err != nil {
			return r, err
		}
		basis = append(basis, basisEntry{ZoneID: z.ID, Version: ver.Version, VerifiedAt: verifiedAt})
	}
	basisJSON, _ := json.Marshal(basis)
	now := s.nowUnix()
	res, err := tx.Exec(`INSERT INTO releases (conclusion, issued_by, issued_at, basis) VALUES (?,?,?,?)`,
		conclusion, actor.ID, now, string(basisJSON))
	if err != nil {
		return r, err
	}
	r = Release{Conclusion: conclusion, IssuedBy: actor.ID, IssuedAt: now, Basis: string(basisJSON)}
	r.ID, _ = res.LastInsertId()
	return r, tx.Commit()
}

// ReleaseView 放行结论及其当前有效性：发布后若出现冻结或区域状态倒退，
// 结论标记为失效，发射指挥看到的始终是当前安全状态。
type ReleaseView struct {
	Release
	Valid  bool     `json:"valid"`
	Reason []string `json:"invalid_reasons,omitempty"`
}

// LatestRelease 最近一次放行结论（含有效性评估）。
func (s *Service) LatestRelease() (*ReleaseView, error) {
	var r Release
	err := s.db.QueryRow(`SELECT id, conclusion, issued_by, issued_at, basis FROM releases ORDER BY id DESC LIMIT 1`).
		Scan(&r.ID, &r.Conclusion, &r.IssuedBy, &r.IssuedAt, &r.Basis)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	view := &ReleaseView{Release: r, Valid: true}
	// 有效性：无未解除冻结，且各区域当前版本与核对结论仍与放行依据一致。
	freezes, err := s.ListFreezes(true)
	if err != nil {
		return nil, err
	}
	for _, f := range freezes {
		view.Valid = false
		view.Reason = append(view.Reason, "区域 "+f.ZoneID+" 存在未解除冻结："+f.Detail)
	}
	type basisEntry struct {
		ZoneID     string `json:"zone_id"`
		Version    int    `json:"version"`
		VerifiedAt int64  `json:"verified_at"`
	}
	var basisList []basisEntry
	if err := json.Unmarshal([]byte(r.Basis), &basisList); err != nil {
		return nil, err
	}
	basis := map[string]basisEntry{}
	for _, b := range basisList {
		basis[b.ZoneID] = b
	}
	zones, err := s.ListZones()
	if err != nil {
		return nil, err
	}
	for _, z := range zones {
		ver, err := s.currentZoneVersion(s.db, z.ID)
		if err != nil {
			return nil, err
		}
		b, ok := basis[z.ID]
		if !ok || b.Version != ver.Version {
			view.Valid = false
			view.Reason = append(view.Reason, "区域 "+z.ID+" 当前版本 v"+itoa(ver.Version)+" 未在放行结论覆盖范围内")
			continue
		}
		var verifiedAt int64
		err = s.db.QueryRow(`SELECT verified_at FROM zone_verifications WHERE zone_id = ? AND zone_version = ? AND result = 'cleared'`,
			z.ID, ver.Version).Scan(&verifiedAt)
		if errors.Is(err, sql.ErrNoRows) || verifiedAt != b.VerifiedAt {
			view.Valid = false
			view.Reason = append(view.Reason, "区域 "+z.ID+" 的核对结论在放行后发生变化")
		} else if err != nil {
			return nil, err
		}
	}
	return view, nil
}

// ListReleases 全部放行结论。
func (s *Service) ListReleases() ([]Release, error) {
	rows, err := s.db.Query(`SELECT id, conclusion, issued_by, issued_at, basis FROM releases ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Release{}
	for rows.Next() {
		var r Release
		if err := rows.Scan(&r.ID, &r.Conclusion, &r.IssuedBy, &r.IssuedAt, &r.Basis); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 里程碑
// ---------------------------------------------------------------------------

// SealMilestone 封存里程碑。封存为幂等操作；封存后里程碑本体不可变，
// 仅允许通过 AddDeviation 追加偏差说明。
func (s *Service) SealMilestone(actor Actor, id string) (Milestone, error) {
	if actor.Role != RoleGroundCommander {
		return Milestone{}, Forbidden("只有地面保障指挥可以封存里程碑")
	}
	res, err := s.db.Exec(`UPDATE milestones SET sealed = 1, sealed_at = ?, sealed_by = ? WHERE id = ? AND sealed = 0`,
		s.nowUnix(), actor.ID, id)
	if err != nil {
		return Milestone{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var sealed int
		err := s.db.QueryRow(`SELECT sealed FROM milestones WHERE id = ?`, id).Scan(&sealed)
		if errors.Is(err, sql.ErrNoRows) {
			return Milestone{}, NotFound("里程碑 %s 不存在", id)
		}
		if err != nil {
			return Milestone{}, err
		}
		// 已封存：幂等返回现状。
	}
	return s.GetMilestone(id)
}

// GetMilestone 里程碑详情（含偏差说明）。
func (s *Service) GetMilestone(id string) (Milestone, error) {
	var m Milestone
	var sealedAt sql.NullInt64
	var sealed int
	err := s.db.QueryRow(`SELECT id, name, seq, sealed, sealed_at, sealed_by FROM milestones WHERE id = ?`, id).
		Scan(&m.ID, &m.Name, &m.Seq, &sealed, &sealedAt, &m.SealedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return m, NotFound("里程碑 %s 不存在", id)
	}
	if err != nil {
		return m, err
	}
	m.Sealed = sealed != 0
	if sealedAt.Valid {
		m.SealedAt = &sealedAt.Int64
	}
	return m, nil
}

// ListMilestones 里程碑列表。
func (s *Service) ListMilestones() ([]Milestone, error) {
	rows, err := s.db.Query(`SELECT id, name, seq, sealed, sealed_at, sealed_by FROM milestones ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Milestone{}
	for rows.Next() {
		var m Milestone
		var sealedAt sql.NullInt64
		var sealed int
		if err := rows.Scan(&m.ID, &m.Name, &m.Seq, &sealed, &sealedAt, &m.SealedBy); err != nil {
			return nil, err
		}
		m.Sealed = sealed != 0
		if sealedAt.Valid {
			m.SealedAt = &sealedAt.Int64
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AddDeviation 追加里程碑偏差说明（封存后唯一允许的写操作，只增不改）。
func (s *Service) AddDeviation(actor Actor, milestoneID, note string) (Deviation, error) {
	var d Deviation
	if actor.Role != RoleGroundCommander && actor.Role != RoleCrewLead {
		return d, Forbidden("只有地面保障指挥或班组负责人可以登记偏差说明")
	}
	if note == "" {
		return d, BadRequest("note（偏差说明）必填")
	}
	if _, err := s.GetMilestone(milestoneID); err != nil {
		return d, err
	}
	now := s.nowUnix()
	res, err := s.db.Exec(`INSERT INTO milestone_deviations (milestone_id, note, created_by, created_at) VALUES (?,?,?,?)`,
		milestoneID, note, actor.ID, now)
	if err != nil {
		return d, err
	}
	d = Deviation{MilestoneID: milestoneID, Note: note, CreatedBy: actor.ID, CreatedAt: now}
	d.ID, _ = res.LastInsertId()
	return d, nil
}

// ListDeviations 里程碑偏差说明列表。
func (s *Service) ListDeviations(milestoneID string) ([]Deviation, error) {
	rows, err := s.db.Query(`SELECT id, milestone_id, note, created_by, created_at FROM milestone_deviations WHERE milestone_id = ? ORDER BY id`, milestoneID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Deviation{}
	for rows.Next() {
		var d Deviation
		if err := rows.Scan(&d.ID, &d.MilestoneID, &d.Note, &d.CreatedBy, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 通知与回执
// ---------------------------------------------------------------------------

// NotifyUnit 向责任单位发送清场通知（通常针对未撤离对象）。
func (s *Service) NotifyUnit(actor Actor, zoneID, unitID, subject, body string) (Notification, error) {
	var n Notification
	if actor.Role != RoleGroundCommander {
		return n, Forbidden("只有地面保障指挥可以发送通知")
	}
	if unitID == "" || subject == "" {
		return n, BadRequest("unit_id、subject 必填")
	}
	now := s.nowUnix()
	res, err := s.db.Exec(`INSERT INTO notifications (zone_id, unit_id, subject, body, created_at) VALUES (?,?,?,?,?)`,
		zoneID, unitID, subject, body, now)
	if err != nil {
		return n, err
	}
	n = Notification{ZoneID: zoneID, UnitID: unitID, Subject: subject, Body: body, CreatedAt: now}
	n.ID, _ = res.LastInsertId()
	return n, nil
}

// AckNotification 承包单位回执通知，仅限本单位收到的通知。
func (s *Service) AckNotification(actor Actor, id int64) (Notification, error) {
	if actor.Role != RoleContractor {
		return Notification{}, Forbidden("只有承包单位可以回执通知")
	}
	n, err := s.getNotification(id)
	if err != nil {
		return n, err
	}
	if n.UnitID != actor.UnitID {
		return n, Forbidden("承包单位不得回执其他单位的通知")
	}
	if n.AckedAt != nil {
		return n, nil // 重复回执幂等
	}
	now := s.nowUnix()
	if _, err := s.db.Exec(`UPDATE notifications SET acked_at = ?, acked_by = ? WHERE id = ? AND acked_at IS NULL`, now, actor.ID, id); err != nil {
		return n, err
	}
	return s.getNotification(id)
}

func (s *Service) getNotification(id int64) (Notification, error) {
	var n Notification
	var ackedAt sql.NullInt64
	err := s.db.QueryRow(`SELECT id, zone_id, unit_id, subject, body, created_at, acked_at, acked_by FROM notifications WHERE id = ?`, id).
		Scan(&n.ID, &n.ZoneID, &n.UnitID, &n.Subject, &n.Body, &n.CreatedAt, &ackedAt, &n.AckedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return n, NotFound("通知 %d 不存在", id)
	}
	if ackedAt.Valid {
		n.AckedAt = &ackedAt.Int64
	}
	return n, err
}

// ListNotifications 通知列表；承包单位仅见本单位通知。
func (s *Service) ListNotifications(actor Actor) ([]Notification, error) {
	q := `SELECT id, zone_id, unit_id, subject, body, created_at, acked_at, acked_by FROM notifications`
	args := []any{}
	if actor.Role == RoleContractor {
		q += ` WHERE unit_id = ?`
		args = append(args, actor.UnitID)
	}
	q += ` ORDER BY id`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		var n Notification
		var ackedAt sql.NullInt64
		if err := rows.Scan(&n.ID, &n.ZoneID, &n.UnitID, &n.Subject, &n.Body, &n.CreatedAt, &ackedAt, &n.AckedBy); err != nil {
			return nil, err
		}
		if ackedAt.Valid {
			n.AckedAt = &ackedAt.Int64
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 名册查询（按角色隔离）
// ---------------------------------------------------------------------------

// Personnel 岗位名册。承包单位仅见本单位人员，班组负责人仅见本组人员。
func (s *Service) Personnel(actor Actor) ([]Person, error) {
	q := `SELECT id, name, unit_id, crew_id, post FROM personnel`
	args := []any{}
	switch actor.Role {
	case RoleContractor:
		q += ` WHERE unit_id = ?`
		args = append(args, actor.UnitID)
	case RoleCrewLead:
		q += ` WHERE crew_id = ?`
		args = append(args, actor.CrewID)
	}
	q += ` ORDER BY id`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Person{}
	for rows.Next() {
		var p Person
		if err := rows.Scan(&p.ID, &p.Name, &p.UnitID, &p.CrewID, &p.Post); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Vehicles 车辆设备名册，隔离规则同 Personnel。
func (s *Service) Vehicles(actor Actor) ([]Vehicle, error) {
	q := `SELECT id, plate, kind, unit_id, crew_id FROM vehicles`
	args := []any{}
	switch actor.Role {
	case RoleContractor:
		q += ` WHERE unit_id = ?`
		args = append(args, actor.UnitID)
	case RoleCrewLead:
		q += ` WHERE crew_id = ?`
		args = append(args, actor.CrewID)
	}
	q += ` ORDER BY id`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Vehicle{}
	for rows.Next() {
		var v Vehicle
		if err := rows.Scan(&v.ID, &v.Plate, &v.Kind, &v.UnitID, &v.CrewID); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 清场状态与看板
// ---------------------------------------------------------------------------

// pendingObjects 计算区域未撤离对象：净在区（进>出）或名册在列但无任何回执。
// crewID 非空时只统计该班组对象。
func (s *Service) pendingObjects(tx *sql.Tx, zoneID, crewID string) ([]PendingObject, error) {
	q := `SELECT a.object_type, a.object_id, COALESCE(o.name, a.object_id), a.unit_id, a.crew_id,
		COALESCE(SUM(CASE ge.direction WHEN 'in' THEN 1 WHEN 'out' THEN -1 ELSE 0 END), 0) AS net,
		COUNT(ge.id) AS events
	FROM assignments a
	LEFT JOIN objects o ON o.object_type = a.object_type AND o.id = a.object_id
	LEFT JOIN gate_events ge ON ge.zone_id = a.zone_id AND ge.object_type = a.object_type AND ge.object_id = a.object_id
	WHERE a.zone_id = ? AND a.active = 1`
	args := []any{zoneID}
	if crewID != "" {
		q += ` AND a.crew_id = ?`
		args = append(args, crewID)
	}
	q += ` GROUP BY a.id HAVING net > 0 OR events = 0 ORDER BY a.object_type, a.object_id`
	rows, err := tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PendingObject{}
	for rows.Next() {
		var p PendingObject
		var events int
		if err := rows.Scan(&p.ObjectType, &p.ObjectID, &p.ObjectName, &p.UnitID, &p.CrewID, &p.Net, &events); err != nil {
			return nil, err
		}
		p.MissingReceipt = events == 0
		out = append(out, p)
	}
	return out, rows.Err()
}

// ensureNoOpenFreezes 区域存在未解除冻结时返回 409。
func (s *Service) ensureNoOpenFreezes(tx *sql.Tx, zoneID string) error {
	rows, err := tx.Query(`SELECT id, zone_id, reason, object_key, detail, raised_at, raised_by, resolved_at, resolved_by, resolution
		FROM freezes WHERE zone_id = ? AND resolved_at IS NULL ORDER BY id`, zoneID)
	if err != nil {
		return err
	}
	freezes, err := scanFreezes(rows)
	if err != nil {
		return err
	}
	if len(freezes) > 0 {
		return ConflictWithDetails(freezes, "区域 %s 存在 %d 项未解除冻结，禁止确认/核对/放行", zoneID, len(freezes))
	}
	return nil
}

// syncMissingReceiptFreezes 为名册在列但无任何回执的对象登记缺回执冻结（幂等）。
// 在状态计算时调用：进程重启后重新计算仍能得到一致结论。
func (s *Service) syncMissingReceiptFreezes(tx *sql.Tx, zoneID string) error {
	rows, err := tx.Query(`SELECT a.object_type, a.object_id
		FROM assignments a
		LEFT JOIN gate_events ge ON ge.zone_id = a.zone_id AND ge.object_type = a.object_type AND ge.object_id = a.object_id
		WHERE a.zone_id = ? AND a.active = 1
		GROUP BY a.id HAVING COUNT(ge.id) = 0`, zoneID)
	if err != nil {
		return err
	}
	var keys [][2]string
	for rows.Next() {
		var t, id string
		if err := rows.Scan(&t, &id); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, [2]string{t, id})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, k := range keys {
		if err := s.raiseFreeze(tx, zoneID, FreezeMissingReceipt, k[0]+":"+k[1],
			"对象 "+k[0]+":"+k[1]+" 名册在列但无任何门禁回执", "system"); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) zoneDependencies(tx *sql.Tx, zoneID string) ([]ZoneDependency, error) {
	rows, err := tx.Query(`SELECT depends_on FROM zone_deps WHERE zone_id = ? ORDER BY depends_on`, zoneID)
	if err != nil {
		return nil, err
	}
	var deps []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			return nil, err
		}
		deps = append(deps, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]ZoneDependency, 0, len(deps))
	for _, d := range deps {
		dep := ZoneDependency{ZoneID: d}
		ver, err := s.currentZoneVersion(tx, d)
		if err == nil {
			var n int
			if err := tx.QueryRow(`SELECT COUNT(1) FROM zone_verifications WHERE zone_id = ? AND zone_version = ? AND result = 'cleared'`,
				d, ver.Version).Scan(&n); err != nil {
				return nil, err
			}
			dep.Cleared = n > 0
		}
		out = append(out, dep)
	}
	return out, nil
}

// ZoneStatus 区域清场状态：未撤离对象（含责任单位）、未解除冻结、
// 当前版本确认记录与最后确认时间、依赖区域清场状态、通知回执。
func (s *Service) ZoneStatus(zoneID string) (ZoneStatus, error) {
	var st ZoneStatus
	tx, err := s.db.Begin()
	if err != nil {
		return st, err
	}
	defer tx.Rollback()

	err = tx.QueryRow(`SELECT id, name FROM zones WHERE id = ?`, zoneID).Scan(&st.ZoneID, &st.ZoneName)
	if errors.Is(err, sql.ErrNoRows) {
		return st, NotFound("危险区 %s 不存在", zoneID)
	}
	if err != nil {
		return st, err
	}
	ver, err := s.currentZoneVersion(tx, zoneID)
	if err != nil {
		return st, err
	}
	st.CurrentVersion = ver.Version

	// 缺回执冻结的持久化同步（幂等），保证重启后结论一致。
	if err := s.syncMissingReceiptFreezes(tx, zoneID); err != nil {
		return st, err
	}

	st.PendingObjects, err = s.pendingObjects(tx, zoneID, "")
	if err != nil {
		return st, err
	}
	if st.PendingObjects == nil {
		st.PendingObjects = []PendingObject{}
	}

	frows, err := tx.Query(`SELECT id, zone_id, reason, object_key, detail, raised_at, raised_by, resolved_at, resolved_by, resolution
		FROM freezes WHERE zone_id = ? AND resolved_at IS NULL ORDER BY id`, zoneID)
	if err != nil {
		return st, err
	}
	st.OpenFreezes, err = scanFreezes(frows)
	if err != nil {
		return st, err
	}
	if st.OpenFreezes == nil {
		st.OpenFreezes = []Freeze{}
	}

	crows, err := tx.Query(`SELECT zone_id, zone_version, crew_id, shift_id, confirmed_by, confirmed_at, note
		FROM crew_confirmations WHERE zone_id = ? AND zone_version = ? ORDER BY crew_id`, zoneID, ver.Version)
	if err != nil {
		return st, err
	}
	defer crows.Close()
	st.Confirmations = []CrewConfirmation{}
	for crows.Next() {
		var c CrewConfirmation
		if err := crows.Scan(&c.ZoneID, &c.ZoneVersion, &c.CrewID, &c.ShiftID, &c.ConfirmedBy, &c.ConfirmedAt, &c.Note); err != nil {
			return st, err
		}
		st.Confirmations = append(st.Confirmations, c)
	}
	if err := crows.Err(); err != nil {
		return st, err
	}
	for _, c := range st.Confirmations {
		if st.LastConfirmationAt == nil || c.ConfirmedAt > *st.LastConfirmationAt {
			t := c.ConfirmedAt
			st.LastConfirmationAt = &t
		}
	}

	var verified int
	if err := tx.QueryRow(`SELECT COUNT(1) FROM zone_verifications WHERE zone_id = ? AND zone_version = ? AND result = 'cleared'`,
		zoneID, ver.Version).Scan(&verified); err != nil {
		return st, err
	}
	st.Verified = verified > 0

	st.Dependencies, err = s.zoneDependencies(tx, zoneID)
	if err != nil {
		return st, err
	}
	if st.Dependencies == nil {
		st.Dependencies = []ZoneDependency{}
	}

	nrows, err := tx.Query(`SELECT id, zone_id, unit_id, subject, body, created_at, acked_at, acked_by
		FROM notifications WHERE zone_id = ? ORDER BY id`, zoneID)
	if err != nil {
		return st, err
	}
	defer nrows.Close()
	st.Notifications = []Notification{}
	for nrows.Next() {
		var n Notification
		var ackedAt sql.NullInt64
		if err := nrows.Scan(&n.ID, &n.ZoneID, &n.UnitID, &n.Subject, &n.Body, &n.CreatedAt, &ackedAt, &n.AckedBy); err != nil {
			return st, err
		}
		if ackedAt.Valid {
			n.AckedAt = &ackedAt.Int64
		}
		st.Notifications = append(st.Notifications, n)
	}
	if err := nrows.Err(); err != nil {
		return st, err
	}

	return st, tx.Commit()
}

// Dashboard 指挥席看板：全部区域的清场状态。
func (s *Service) Dashboard() ([]ZoneStatus, error) {
	zones, err := s.ListZones()
	if err != nil {
		return nil, err
	}
	out := make([]ZoneStatus, 0, len(zones))
	for _, z := range zones {
		st, err := s.ZoneStatus(z.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

// PendingObjects 区域未撤离对象（含责任单位），供按区域依赖排查。
func (s *Service) PendingObjects(zoneID string) ([]PendingObject, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := s.currentZoneVersion(tx, zoneID); err != nil {
		return nil, err
	}
	if err := s.syncMissingReceiptFreezes(tx, zoneID); err != nil {
		return nil, err
	}
	pending, err := s.pendingObjects(tx, zoneID, "")
	if err != nil {
		return nil, err
	}
	if pending == nil {
		pending = []PendingObject{}
	}
	// 稳定排序：先人员后车辆，再按责任单位。
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].ObjectType != pending[j].ObjectType {
			return pending[i].ObjectType < pending[j].ObjectType
		}
		if pending[i].UnitID != pending[j].UnitID {
			return pending[i].UnitID < pending[j].UnitID
		}
		return pending[i].ObjectID < pending[j].ObjectID
	})
	return pending, tx.Commit()
}

func itoa(i int) string { return strconv.Itoa(i) }
