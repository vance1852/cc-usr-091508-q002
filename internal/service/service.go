// Package service 实现撤离放行业务规则：区域版本、名册、回执、班组确认、
// 冻结与最终放行结论的关联判定。
package service

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"evac/internal/domain"
	"evac/internal/store"
)

// Error 业务错误，Kind 供 API 层映射 HTTP 状态码。
type Error struct {
	Kind    string // not_found / forbidden / conflict / bad_request
	Code    string // 机器可读码，如 zone_frozen
	Message string
	Details any
}

func (e *Error) Error() string { return e.Message }

func notFound(msg string) *Error  { return &Error{Kind: "not_found", Code: "not_found", Message: msg} }
func forbidden(msg string) *Error { return &Error{Kind: "forbidden", Code: "forbidden", Message: msg} }
func badRequest(msg string) *Error {
	return &Error{Kind: "bad_request", Code: "bad_request", Message: msg}
}
func conflict(code, msg string, details any) *Error {
	return &Error{Kind: "conflict", Code: code, Message: msg, Details: details}
}

// Principal 调用方身份（由 API 层从请求头解析）。
type Principal struct {
	UserID string
	Role   domain.Role
	UnitID string // 承包单位身份时为单位 ID
	CrewID string // 班组负责人身份时为班组 ID
}

// Service 业务服务。
type Service struct {
	st  *store.Store
	Now func() time.Time // 可注入时钟，便于跨午夜等场景测试
}

// New 创建服务。
func New(st *store.Store) *Service {
	return &Service{st: st, Now: func() time.Time { return time.Now().UTC() }}
}

// ---------- 档案登记（地面保障指挥） ----------

func (s *Service) CreateZone(id, name string) (domain.Zone, error) {
	z := domain.Zone{ID: id, Name: name, CreatedAt: s.Now()}
	if err := s.st.InsertZone(z); err != nil {
		return z, err
	}
	return z, nil
}

// PublishZoneVersion 发布区域新版本；边界扩大时立即冻结放行，
// 且旧版本上的班组确认对新版本不再有效（确认按版本记录）。
func (s *Service) PublishZoneVersion(zoneID, boundary string, expanded bool) (domain.ZoneVersion, error) {
	if _, err := s.st.GetZone(zoneID); err != nil {
		return domain.ZoneVersion{}, notFound("危险区不存在: " + zoneID)
	}
	cur, err := s.st.CurrentZoneVersion(zoneID)
	version := 1
	if err == nil {
		version = cur.Version + 1
	} else if !errors.Is(err, sql.ErrNoRows) {
		return domain.ZoneVersion{}, err
	}
	v := domain.ZoneVersion{ZoneID: zoneID, Version: version, Boundary: boundary, Expanded: expanded, CreatedAt: s.Now()}
	if err := s.st.InsertZoneVersion(v); err != nil {
		return v, err
	}
	if expanded {
		_, err = s.st.InsertFreeze(domain.Freeze{
			ZoneID:    zoneID,
			Reason:    domain.FreezeBoundaryExpanded,
			Detail:    fmt.Sprintf("区域边界扩大，版本升至 v%d，须重新清场并重新确认", version),
			CreatedAt: s.Now(),
		})
		if err != nil {
			return v, err
		}
	}
	return v, nil
}

func (s *Service) AddDependency(zoneID, dependsOn string) error {
	if zoneID == dependsOn {
		return badRequest("区域不能依赖自身")
	}
	if _, err := s.st.GetZone(zoneID); err != nil {
		return notFound("危险区不存在: " + zoneID)
	}
	if _, err := s.st.GetZone(dependsOn); err != nil {
		return notFound("被依赖区域不存在: " + dependsOn)
	}
	return s.st.AddDependency(zoneID, dependsOn)
}

func (s *Service) RegisterUnit(id, name string) error {
	return s.st.InsertUnit(domain.Unit{ID: id, Name: name})
}

func (s *Service) RegisterCrew(c domain.Crew) error {
	if _, err := s.st.GetZone(c.ZoneID); err != nil {
		return notFound("危险区不存在: " + c.ZoneID)
	}
	return s.st.InsertCrew(c)
}

func (s *Service) RegisterPerson(p domain.Person) error {
	if _, err := s.st.GetCrew(p.CrewID); err != nil {
		return notFound("班组不存在: " + p.CrewID)
	}
	return s.st.InsertPerson(p)
}

func (s *Service) RegisterAsset(a domain.Asset) error {
	if _, err := s.st.GetZone(a.ZoneID); err != nil {
		return notFound("危险区不存在: " + a.ZoneID)
	}
	return s.st.InsertAsset(a)
}

// ---------- 门禁回执 ----------

// ReceiptInput 门禁上报。
type ReceiptInput struct {
	ScanID      string
	SubjectKind string // person / asset
	SubjectID   string
	Gate        string
	Direction   domain.Direction
	ScannedAt   time.Time
}

// RecordGateReceipt 记录门禁回执。重复扫码（相同 ScanID）幂等返回已有记录；
// 对象在离场后再次进入时立即冻结该区域放行。
func (s *Service) RecordGateReceipt(in ReceiptInput) (receipt domain.GateReceipt, created bool, err error) {
	if in.ScanID == "" || in.SubjectID == "" || in.Gate == "" {
		return receipt, false, badRequest("scan_id、subject_id、gate 均不能为空")
	}
	if in.Direction != domain.DirExit && in.Direction != domain.DirEntry {
		return receipt, false, badRequest("direction 必须为 exit 或 entry")
	}
	var zoneID string
	switch in.SubjectKind {
	case "person":
		p, e := s.st.GetPerson(in.SubjectID)
		if e != nil {
			return receipt, false, notFound("人员不在名册: " + in.SubjectID)
		}
		zoneID = p.ZoneID
	case "asset":
		a, e := s.st.GetAsset(in.SubjectID)
		if e != nil {
			return receipt, false, notFound("车辆/设备未登记: " + in.SubjectID)
		}
		zoneID = a.ZoneID
	default:
		return receipt, false, badRequest("subject_kind 必须为 person 或 asset")
	}
	ver, err := s.st.CurrentZoneVersion(zoneID)
	if err != nil {
		return receipt, false, conflict("zone_unversioned", "危险区尚未发布任何版本: "+zoneID, nil)
	}

	err = s.st.Tx(func(tx *store.Store) error {
		// 重复扫码：直接返回已有回执，不产生新记录、不重复触发冻结。
		if existing, e := tx.GetReceiptByScanID(in.ScanID); e == nil {
			receipt = existing
			created = false
			return nil
		} else if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		last, hasLast, e := tx.LastReceipt(in.SubjectKind, in.SubjectID)
		if e != nil {
			return e
		}
		r := domain.GateReceipt{
			ScanID:      in.ScanID,
			SubjectKind: in.SubjectKind,
			SubjectID:   in.SubjectID,
			Gate:        in.Gate,
			Direction:   in.Direction,
			ZoneID:      zoneID,
			ZoneVersion: ver.Version,
			ScannedAt:   in.ScannedAt.UTC(),
			ReceivedAt:  s.Now(),
		}
		inserted, e := tx.InsertReceiptIdempotent(r)
		if e != nil {
			return e
		}
		if !inserted { // 并发下同 scan_id 已被写入
			existing, ge := tx.GetReceiptByScanID(in.ScanID)
			if ge != nil {
				return ge
			}
			receipt = existing
			created = false
			return nil
		}
		// 重新进入：上一次事件是离场，本次为进入 → 冻结放行。
		if in.Direction == domain.DirEntry && hasLast && last.Direction == domain.DirExit {
			_, e = tx.InsertFreeze(domain.Freeze{
				ZoneID:    zoneID,
				Reason:    domain.FreezeReEntry,
				Detail:    fmt.Sprintf("%s %s 离场后重新进入危险区（门禁 %s）", in.SubjectKind, in.SubjectID, in.Gate),
				CreatedAt: s.Now(),
			})
			if e != nil {
				return e
			}
		}
		stored, ge := tx.GetReceiptByScanID(in.ScanID)
		if ge != nil {
			return ge
		}
		receipt = stored
		created = true
		return nil
	})
	return receipt, created, err
}

// ---------- 班组确认 ----------

// ConfirmCrew 班组负责人确认本班组在当前区域版本下已撤离；重复确认幂等。
func (s *Service) ConfirmCrew(crewID string, p Principal) (domain.CrewConfirmation, bool, error) {
	crew, err := s.st.GetCrew(crewID)
	if err != nil {
		return domain.CrewConfirmation{}, false, notFound("班组不存在: " + crewID)
	}
	if p.Role != domain.RoleCrewLead || p.UserID != crew.LeadUserID {
		return domain.CrewConfirmation{}, false, forbidden("只能由本班组负责人确认本班组状态")
	}
	ver, err := s.st.CurrentZoneVersion(crew.ZoneID)
	if err != nil {
		return domain.CrewConfirmation{}, false, conflict("zone_unversioned", "危险区尚未发布任何版本: "+crew.ZoneID, nil)
	}
	c := domain.CrewConfirmation{
		CrewID:      crew.ID,
		ZoneID:      crew.ZoneID,
		ZoneVersion: ver.Version,
		ConfirmedBy: p.UserID,
		ConfirmedAt: s.Now(),
	}
	created, err := s.st.InsertConfirmationIdempotent(c)
	if err != nil {
		return c, false, err
	}
	// 回读以获取自增 ID；重复确认时返回已有记录（幂等）。
	confs, e := s.st.ListConfirmations(crew.ZoneID, ver.Version)
	if e != nil {
		return c, false, e
	}
	for _, existing := range confs {
		if existing.CrewID == crew.ID {
			return existing, created, nil
		}
	}
	return c, created, nil
}

// ---------- 清场评估 ----------

// PendingObject 尚未清场的对象及其责任单位。
type PendingObject struct {
	Kind     string `json:"kind"` // person / asset
	ID       string `json:"id"`
	Label    string `json:"label"`
	UnitID   string `json:"unit_id"`
	UnitName string `json:"unit_name"`
	Reason   string `json:"reason"` // missing_receipt / re_entered
	OnShift  bool   `json:"on_shift"`
}

// CrewStatus 班组确认状态。
type CrewStatus struct {
	CrewID      string     `json:"crew_id"`
	CrewName    string     `json:"crew_name"`
	UnitID      string     `json:"unit_id"`
	Confirmed   bool       `json:"confirmed"`
	ConfirmedBy string     `json:"confirmed_by,omitempty"`
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
}

// DependencyStatus 区域依赖放行状态。
type DependencyStatus struct {
	ZoneID   string `json:"zone_id"`
	ZoneName string `json:"zone_name"`
	Version  int    `json:"version"`
	Released bool   `json:"released"`
}

// ZoneClearance 区域清场评估结果。
type ZoneClearance struct {
	ZoneID             string             `json:"zone_id"`
	ZoneName           string             `json:"zone_name"`
	Version            int                `json:"version"`
	Status             domain.ZoneStatus  `json:"status"`
	Pending            []PendingObject    `json:"pending"`
	Crews              []CrewStatus       `json:"crews"`
	ActiveFreezes      []domain.Freeze    `json:"active_freezes"`
	Dependencies       []DependencyStatus `json:"dependencies"`
	LastConfirmationAt *time.Time         `json:"last_confirmation_at,omitempty"`
	Release            *domain.Release    `json:"release,omitempty"`
}

// Evaluate 计算区域当前版本下的清场状态：未撤离对象、班组确认、冻结与依赖。
func (s *Service) Evaluate(zoneID string) (*ZoneClearance, error) {
	zone, err := s.st.GetZone(zoneID)
	if err != nil {
		return nil, notFound("危险区不存在: " + zoneID)
	}
	ver, err := s.st.CurrentZoneVersion(zoneID)
	if err != nil {
		return nil, conflict("zone_unversioned", "危险区尚未发布任何版本: "+zoneID, nil)
	}
	unitNames, err := s.st.UnitNames()
	if err != nil {
		return nil, err
	}
	receipts, err := s.st.ListReceiptsByZone(zoneID)
	if err != nil {
		return nil, err
	}
	last := map[string]domain.GateReceipt{}
	for _, r := range receipts {
		last[r.SubjectKind+"/"+r.SubjectID] = r
	}
	now := s.Now()
	pending := []PendingObject{}

	persons, err := s.st.ListPersons(zoneID, "", "")
	if err != nil {
		return nil, err
	}
	for _, p := range persons {
		r, ok := last["person/"+p.ID]
		switch {
		case !ok:
			pending = append(pending, PendingObject{
				Kind: "person", ID: p.ID, Label: p.Name, UnitID: p.UnitID,
				UnitName: unitNames[p.UnitID], Reason: "missing_receipt",
				OnShift: domain.WithinShift(p.ShiftStart, p.ShiftEnd, now),
			})
		case r.Direction == domain.DirEntry:
			pending = append(pending, PendingObject{
				Kind: "person", ID: p.ID, Label: p.Name, UnitID: p.UnitID,
				UnitName: unitNames[p.UnitID], Reason: "re_entered",
				OnShift: domain.WithinShift(p.ShiftStart, p.ShiftEnd, now),
			})
		}
	}
	assets, err := s.st.ListAssetsByZone(zoneID)
	if err != nil {
		return nil, err
	}
	for _, a := range assets {
		r, ok := last["asset/"+a.ID]
		switch {
		case !ok:
			pending = append(pending, PendingObject{
				Kind: "asset", ID: a.ID, Label: a.Label, UnitID: a.UnitID,
				UnitName: unitNames[a.UnitID], Reason: "missing_receipt",
			})
		case r.Direction == domain.DirEntry:
			pending = append(pending, PendingObject{
				Kind: "asset", ID: a.ID, Label: a.Label, UnitID: a.UnitID,
				UnitName: unitNames[a.UnitID], Reason: "re_entered",
			})
		}
	}

	crews, err := s.st.ListCrewsByZone(zoneID)
	if err != nil {
		return nil, err
	}
	confs, err := s.st.ListConfirmations(zoneID, ver.Version)
	if err != nil {
		return nil, err
	}
	confByCrew := map[string]domain.CrewConfirmation{}
	var lastConf *time.Time
	for _, c := range confs {
		confByCrew[c.CrewID] = c
		t := c.ConfirmedAt
		if lastConf == nil || t.After(*lastConf) {
			lastConf = &t
		}
	}
	crewStatuses := []CrewStatus{}
	allConfirmed := true
	for _, c := range crews {
		cs := CrewStatus{CrewID: c.ID, CrewName: c.Name, UnitID: c.UnitID}
		if conf, ok := confByCrew[c.ID]; ok {
			cs.Confirmed = true
			cs.ConfirmedBy = conf.ConfirmedBy
			t := conf.ConfirmedAt
			cs.ConfirmedAt = &t
		} else {
			allConfirmed = false
		}
		crewStatuses = append(crewStatuses, cs)
	}

	freezes, err := s.st.ActiveFreezes(zoneID)
	if err != nil {
		return nil, err
	}
	if freezes == nil {
		freezes = []domain.Freeze{}
	}

	deps, err := s.st.ListDependencies(zoneID)
	if err != nil {
		return nil, err
	}
	depStatuses := []DependencyStatus{}
	for _, depID := range deps {
		ds := DependencyStatus{ZoneID: depID}
		if depZone, e := s.st.GetZone(depID); e == nil {
			ds.ZoneName = depZone.Name
		}
		if depVer, e := s.st.CurrentZoneVersion(depID); e == nil {
			ds.Version = depVer.Version
			if _, released, e2 := s.st.GetRelease(depID, depVer.Version); e2 == nil {
				ds.Released = released
			}
		}
		depStatuses = append(depStatuses, ds)
	}

	cl := &ZoneClearance{
		ZoneID:             zone.ID,
		ZoneName:           zone.Name,
		Version:            ver.Version,
		Pending:            pending,
		Crews:              crewStatuses,
		ActiveFreezes:      freezes,
		Dependencies:       depStatuses,
		LastConfirmationAt: lastConf,
	}
	if rel, ok, e := s.st.GetRelease(zoneID, ver.Version); e != nil {
		return nil, e
	} else if ok {
		cl.Release = &rel
	}

	switch {
	case cl.Release != nil:
		cl.Status = domain.StatusReleased
	case len(freezes) > 0:
		cl.Status = domain.StatusFrozen
	case len(pending) == 0 && allConfirmed:
		cl.Status = domain.StatusCleared
	default:
		cl.Status = domain.StatusPending
	}
	return cl, nil
}

// ---------- 冻结解除 ----------

// ResolveFreeze 由地面保障指挥在冻结诱因消除后解除冻结。
func (s *Service) ResolveFreeze(zoneID string, freezeID int64, by string) error {
	fz, err := s.st.GetFreeze(freezeID)
	if err != nil {
		return notFound(fmt.Sprintf("冻结记录不存在: %d", freezeID))
	}
	if fz.ZoneID != zoneID {
		return notFound(fmt.Sprintf("冻结记录 %d 不属于区域 %s", freezeID, zoneID))
	}
	if !fz.Active {
		return conflict("freeze_inactive", "冻结已解除，无需重复操作", nil)
	}
	cl, err := s.Evaluate(zoneID)
	if err != nil {
		return err
	}
	hasPending := func(reason string) bool {
		for _, p := range cl.Pending {
			if p.Reason == reason {
				return true
			}
		}
		return false
	}
	allConfirmed := true
	for _, c := range cl.Crews {
		if !c.Confirmed {
			allConfirmed = false
		}
	}
	switch fz.Reason {
	case domain.FreezeMissingReceipt:
		if hasPending("missing_receipt") {
			return conflict("freeze_condition_active", "仍存在缺少回执的对象，不能解除冻结", cl.Pending)
		}
	case domain.FreezeReEntry:
		if hasPending("re_entered") {
			return conflict("freeze_condition_active", "重新进入的对象尚未再次离场，不能解除冻结", cl.Pending)
		}
	case domain.FreezeBoundaryExpanded:
		if len(cl.Pending) > 0 || !allConfirmed {
			return conflict("freeze_condition_active", "边界扩大后须完成全部清场并按新版本重新确认", cl.Pending)
		}
	}
	return s.st.ResolveFreeze(freezeID, by, s.Now())
}

// ---------- 核对与放行 ----------

// VerifyZone 地面保障指挥核对区域：全部撤离、班组确认、无冻结、依赖区域已放行，
// 通过后生成最终放行结论并封存对应倒计时里程碑。重复核对幂等返回已有结论。
func (s *Service) VerifyZone(zoneID, by string) (*domain.Release, *ZoneClearance, error) {
	cl, err := s.Evaluate(zoneID)
	if err != nil {
		return nil, nil, err
	}
	if cl.Release != nil { // 幂等：已放行直接返回结论
		return cl.Release, cl, nil
	}
	// 缺少回执必须冻结放行：核对时发现即登记冻结。
	if hasMissing := pendingHas(cl.Pending, "missing_receipt"); hasMissing && !hasActiveFreeze(cl.ActiveFreezes, domain.FreezeMissingReceipt) {
		if _, err := s.st.InsertFreeze(domain.Freeze{
			ZoneID:    zoneID,
			Reason:    domain.FreezeMissingReceipt,
			Detail:    "核对时仍存在缺少门禁回执的对象",
			CreatedAt: s.Now(),
		}); err != nil {
			return nil, nil, err
		}
		cl, err = s.Evaluate(zoneID)
		if err != nil {
			return nil, nil, err
		}
	}
	if len(cl.ActiveFreezes) > 0 {
		return nil, cl, conflict("zone_frozen", "区域存在未解除的冻结，禁止放行", cl.ActiveFreezes)
	}
	if len(cl.Pending) > 0 {
		return nil, cl, conflict("not_cleared", "区域尚有对象未撤离", cl.Pending)
	}
	for _, c := range cl.Crews {
		if !c.Confirmed {
			return nil, cl, conflict("crew_unconfirmed", "尚有班组未确认: "+c.CrewID, cl.Crews)
		}
	}
	for _, d := range cl.Dependencies {
		if !d.Released {
			return nil, cl, conflict("dependency_blocked",
				fmt.Sprintf("依赖区域 %s 尚未放行，本区域不能放行", d.ZoneID), cl.Dependencies)
		}
	}

	conclusion := fmt.Sprintf("危险区 %s v%d 清场核对通过：名册人员、车辆设备全部离场，班组全部确认，准予进入倒计时管制",
		zoneID, cl.Version)
	rel := domain.Release{
		ZoneID:      zoneID,
		ZoneVersion: cl.Version,
		Conclusion:  conclusion,
		VerifiedBy:  by,
		VerifiedAt:  s.Now(),
	}
	err = s.st.Tx(func(tx *store.Store) error {
		if _, e := tx.InsertReleaseIdempotent(rel); e != nil {
			return e
		}
		// 关联倒计时里程碑：放行结论生成即封存。
		now := s.Now()
		return tx.UpsertMilestone(domain.Milestone{
			ID:          fmt.Sprintf("MS-REL-%s-v%d", zoneID, cl.Version),
			Name:        fmt.Sprintf("区域 %s 撤离放行", zoneID),
			ZoneID:      zoneID,
			ZoneVersion: cl.Version,
			Sealed:      true,
			SealedAt:    &now,
			Conclusion:  conclusion,
		})
	})
	if err != nil {
		return nil, nil, err
	}
	cl, err = s.Evaluate(zoneID)
	if err != nil {
		return nil, nil, err
	}
	return cl.Release, cl, nil
}

func pendingHas(pending []PendingObject, reason string) bool {
	for _, p := range pending {
		if p.Reason == reason {
			return true
		}
	}
	return false
}

func hasActiveFreeze(freezes []domain.Freeze, reason domain.FreezeReason) bool {
	for _, f := range freezes {
		if f.Reason == reason {
			return true
		}
	}
	return false
}

// ---------- 通知 ----------

// NotifyZone 针对当前未撤离对象向责任单位生成通知（幂等），返回该区域当前版本全部通知。
func (s *Service) NotifyZone(zoneID string) ([]domain.Notification, error) {
	cl, err := s.Evaluate(zoneID)
	if err != nil {
		return nil, err
	}
	for _, p := range cl.Pending {
		msg := fmt.Sprintf("危险区 %s v%d：%s「%s」尚未完成撤离（%s），请责任单位 %s 立即核实并撤离",
			zoneID, cl.Version, p.Kind, p.Label, p.Reason, p.UnitName)
		if _, err := s.st.UpsertNotification(domain.Notification{
			ZoneID:      zoneID,
			ZoneVersion: cl.Version,
			UnitID:      p.UnitID,
			SubjectKind: p.Kind,
			SubjectID:   p.ID,
			Message:     msg,
			CreatedAt:   s.Now(),
		}); err != nil {
			return nil, err
		}
	}
	return s.st.ListNotifications(zoneID, "")
}

// ListNotifications 按角色过滤通知：承包单位仅见本单位。
func (s *Service) ListNotifications(p Principal) ([]domain.Notification, error) {
	switch p.Role {
	case domain.RoleGroundCommand:
		return s.st.ListNotifications("", "")
	case domain.RoleContractor:
		return s.st.ListNotifications("", p.UnitID)
	default:
		return nil, forbidden("无权查看通知")
	}
}

// AckNotification 通知回执：承包单位只能回执本单位通知。
func (s *Service) AckNotification(id int64, p Principal) (domain.Notification, error) {
	n, err := s.st.GetNotification(id)
	if err != nil {
		return n, notFound(fmt.Sprintf("通知不存在: %d", id))
	}
	switch p.Role {
	case domain.RoleGroundCommand:
	case domain.RoleContractor:
		if n.UnitID != p.UnitID {
			return n, forbidden("承包单位不得操作其他单位的通知")
		}
	default:
		return n, forbidden("无权回执通知")
	}
	if err := s.st.AckNotification(id, p.UserID, s.Now()); err != nil {
		return n, err
	}
	return s.st.GetNotification(id)
}

// ---------- 人员资料（数据范围控制） ----------

// ListPersons 按角色限定数据范围：承包单位仅本单位，班组负责人仅本班组。
func (s *Service) ListPersons(p Principal, zoneID, unitID string) ([]domain.Person, error) {
	switch p.Role {
	case domain.RoleGroundCommand:
		return s.st.ListPersons(zoneID, unitID, "")
	case domain.RoleContractor:
		if p.UnitID == "" {
			return nil, forbidden("承包单位身份缺少单位标识")
		}
		return s.st.ListPersons(zoneID, p.UnitID, "") // 强制限定本单位，忽略他单位参数
	case domain.RoleCrewLead:
		return s.st.ListPersons(zoneID, "", p.CrewID)
	default:
		return nil, forbidden("发射指挥仅接收最终放行结论，无权查阅人员资料")
	}
}

// ---------- 里程碑 ----------

func (s *Service) CreateMilestone(zoneID, id, name, conclusion string) (domain.Milestone, error) {
	ver, err := s.st.CurrentZoneVersion(zoneID)
	if err != nil {
		return domain.Milestone{}, notFound("危险区尚未发布版本: " + zoneID)
	}
	m := domain.Milestone{ID: id, Name: name, ZoneID: zoneID, ZoneVersion: ver.Version, Conclusion: conclusion}
	if err := s.st.UpsertMilestone(m); err != nil {
		return m, err
	}
	return s.st.GetMilestone(id)
}

// UpdateMilestone 修改里程碑；已封存的里程碑拒绝修改，只能追加偏差说明。
func (s *Service) UpdateMilestone(id, name, conclusion string) (domain.Milestone, error) {
	m, err := s.st.GetMilestone(id)
	if err != nil {
		return m, notFound("里程碑不存在: " + id)
	}
	if m.Sealed {
		return m, conflict("milestone_sealed", "里程碑已封存，只能追加偏差说明", nil)
	}
	if _, err := s.st.UpdateMilestoneDraft(id, name, conclusion); err != nil {
		return m, err
	}
	return s.st.GetMilestone(id)
}

func (s *Service) SealMilestone(id string) (domain.Milestone, error) {
	m, err := s.st.GetMilestone(id)
	if err != nil {
		return m, notFound("里程碑不存在: " + id)
	}
	if !m.Sealed {
		if err := s.st.SealMilestone(id, s.Now()); err != nil {
			return m, err
		}
	}
	return s.st.GetMilestone(id)
}

// AppendDeviation 向已封存里程碑追加偏差说明。
func (s *Service) AppendDeviation(id, note, author string) (domain.Deviation, error) {
	m, err := s.st.GetMilestone(id)
	if err != nil {
		return domain.Deviation{}, notFound("里程碑不存在: " + id)
	}
	if !m.Sealed {
		return domain.Deviation{}, conflict("milestone_unsealed", "里程碑未封存，可直接修改结论，无需偏差说明", nil)
	}
	devID, err := s.st.AppendDeviation(domain.Deviation{
		MilestoneID: id, Note: note, Author: author, CreatedAt: s.Now(),
	})
	if err != nil {
		return domain.Deviation{}, err
	}
	devs, err := s.st.ListDeviations(id)
	if err != nil {
		return domain.Deviation{}, err
	}
	for _, d := range devs {
		if d.ID == devID {
			return d, nil
		}
	}
	return domain.Deviation{}, nil
}

func (s *Service) ListDeviations(milestoneID string) ([]domain.Deviation, error) {
	if _, err := s.st.GetMilestone(milestoneID); err != nil {
		return nil, notFound("里程碑不存在: " + milestoneID)
	}
	return s.st.ListDeviations(milestoneID)
}

// ListMilestones 全部倒计时里程碑。
func (s *Service) ListMilestones() ([]domain.Milestone, error) {
	return s.st.ListMilestones()
}

// ---------- 指挥席与放行结论 ----------

// ZoneDashboard 指挥席单区域视图。
type ZoneDashboard struct {
	*ZoneClearance
	Notifications []domain.Notification `json:"notifications"`
}

// Dashboard 指挥席总览：各区域最后确认时间、未撤离对象、冻结原因与通知回执。
func (s *Service) Dashboard() ([]ZoneDashboard, error) {
	zones, err := s.st.ListZones()
	if err != nil {
		return nil, err
	}
	out := []ZoneDashboard{}
	for _, z := range zones {
		cl, err := s.Evaluate(z.ID)
		if err != nil {
			return nil, err
		}
		notifs, err := s.st.ListNotifications(z.ID, "")
		if err != nil {
			return nil, err
		}
		if notifs == nil {
			notifs = []domain.Notification{}
		}
		out = append(out, ZoneDashboard{ZoneClearance: cl, Notifications: notifs})
	}
	return out, nil
}

// ReleaseView 发射指挥可见的最终放行结论（不含人员明细）。
type ReleaseView struct {
	ZoneID      string             `json:"zone_id"`
	ZoneName    string             `json:"zone_name"`
	Version     int                `json:"version"`
	Conclusion  string             `json:"conclusion"`
	VerifiedBy  string             `json:"verified_by"`
	VerifiedAt  time.Time          `json:"verified_at"`
	MilestoneID string             `json:"milestone_id"`
	Deviations  []domain.Deviation `json:"deviations"`
}

// ListReleases 全部最终放行结论。
func (s *Service) ListReleases() ([]ReleaseView, error) {
	releases, err := s.st.ListReleases()
	if err != nil {
		return nil, err
	}
	out := []ReleaseView{}
	for _, r := range releases {
		rv, err := s.releaseView(r)
		if err != nil {
			return nil, err
		}
		out = append(out, rv)
	}
	return out, nil
}

// GetRelease 单区域当前版本的放行结论。
func (s *Service) GetRelease(zoneID string) (ReleaseView, error) {
	ver, err := s.st.CurrentZoneVersion(zoneID)
	if err != nil {
		return ReleaseView{}, notFound("危险区尚未发布版本: " + zoneID)
	}
	rel, ok, err := s.st.GetRelease(zoneID, ver.Version)
	if err != nil {
		return ReleaseView{}, err
	}
	if !ok {
		return ReleaseView{}, notFound("区域尚未放行: " + zoneID)
	}
	return s.releaseView(rel)
}

func (s *Service) releaseView(r domain.Release) (ReleaseView, error) {
	rv := ReleaseView{
		ZoneID:      r.ZoneID,
		Version:     r.ZoneVersion,
		Conclusion:  r.Conclusion,
		VerifiedBy:  r.VerifiedBy,
		VerifiedAt:  r.VerifiedAt,
		MilestoneID: fmt.Sprintf("MS-REL-%s-v%d", r.ZoneID, r.ZoneVersion),
		Deviations:  []domain.Deviation{},
	}
	if z, err := s.st.GetZone(r.ZoneID); err == nil {
		rv.ZoneName = z.Name
	}
	devs, err := s.st.ListDeviations(rv.MilestoneID)
	if err == nil && devs != nil {
		rv.Deviations = devs
	}
	return rv, nil
}
