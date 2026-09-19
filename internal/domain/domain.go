// Package domain 定义勤务区撤离放行服务的核心实体与枚举。
package domain

import "time"

// Role 角色：决定调用方可见范围与可执行操作。
type Role string

const (
	RoleCrewLead      Role = "crew_lead"      // 班组负责人：确认本班组撤离状态
	RoleGroundCommand Role = "ground_command" // 地面保障指挥：核对区域、冻结/解冻、放行
	RoleLaunchCommand Role = "launch_command" // 发射指挥：只接收最终放行结论
	RoleContractor    Role = "contractor"     // 承包单位：仅本单位人员资料与通知
)

// Direction 门禁扫码方向。
type Direction string

const (
	DirExit  Direction = "exit"  // 离开危险区
	DirEntry Direction = "entry" // 进入危险区
)

// FreezeReason 冻结放行的原因。
type FreezeReason string

const (
	FreezeMissingReceipt   FreezeReason = "missing_receipt"   // 缺少门禁回执
	FreezeReEntry          FreezeReason = "re_entry"          // 人员/车辆重新进入
	FreezeBoundaryExpanded FreezeReason = "boundary_expanded" // 区域边界扩大
)

// ZoneStatus 区域清场状态。
type ZoneStatus string

const (
	StatusPending  ZoneStatus = "pending"  // 尚有对象未撤离或班组未确认
	StatusFrozen   ZoneStatus = "frozen"   // 存在未解除的冻结
	StatusCleared  ZoneStatus = "cleared"  // 全部撤离且确认完毕，待核对放行
	StatusReleased ZoneStatus = "released" // 已放行
)

// Zone 危险区。
type Zone struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// ZoneVersion 危险区版本；边界变化（尤其扩大）产生新版本。
type ZoneVersion struct {
	ZoneID    string    `json:"zone_id"`
	Version   int       `json:"version"`
	Boundary  string    `json:"boundary"`
	Expanded  bool      `json:"expanded"`
	CreatedAt time.Time `json:"created_at"`
}

// Unit 责任单位（含承包单位）。
type Unit struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Crew 班组，归属责任单位并驻守某危险区。
type Crew struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	UnitID     string `json:"unit_id"`
	ZoneID     string `json:"zone_id"`
	LeadUserID string `json:"lead_user_id"` // 班组负责人账号
}

// Person 岗位名册人员。
type Person struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Post       string `json:"post"` // 岗位
	UnitID     string `json:"unit_id"`
	CrewID     string `json:"crew_id"`
	ZoneID     string `json:"zone_id"`
	ShiftStart string `json:"shift_start"` // "HH:MM"，当地时间
	ShiftEnd   string `json:"shift_end"`   // 结束不晚于开始表示跨午夜班次
}

// Asset 车辆与设备（含活动门架）。
type Asset struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Kind   string `json:"kind"` // vehicle / gantry / equipment
	UnitID string `json:"unit_id"`
	ZoneID string `json:"zone_id"`
}

// GateReceipt 门禁回执；ScanID 为幂等键，重复扫码不产生第二条记录。
type GateReceipt struct {
	ID          int64     `json:"id"`
	ScanID      string    `json:"scan_id"`
	SubjectKind string    `json:"subject_kind"` // person / asset
	SubjectID   string    `json:"subject_id"`
	Gate        string    `json:"gate"`
	Direction   Direction `json:"direction"`
	ZoneID      string    `json:"zone_id"`
	ZoneVersion int       `json:"zone_version"`
	ScannedAt   time.Time `json:"scanned_at"`
	ReceivedAt  time.Time `json:"received_at"`
}

// CrewConfirmation 班组确认，按区域版本记录；新版本需重新确认。
type CrewConfirmation struct {
	ID          int64     `json:"id"`
	CrewID      string    `json:"crew_id"`
	ZoneID      string    `json:"zone_id"`
	ZoneVersion int       `json:"zone_version"`
	ConfirmedBy string    `json:"confirmed_by"`
	ConfirmedAt time.Time `json:"confirmed_at"`
}

// Freeze 冻结记录。
type Freeze struct {
	ID         int64        `json:"id"`
	ZoneID     string       `json:"zone_id"`
	Reason     FreezeReason `json:"reason"`
	Detail     string       `json:"detail"`
	Active     bool         `json:"active"`
	CreatedAt  time.Time    `json:"created_at"`
	ResolvedAt *time.Time   `json:"resolved_at,omitempty"`
	ResolvedBy string       `json:"resolved_by,omitempty"`
}

// Release 最终放行结论，按区域版本唯一。
type Release struct {
	ZoneID      string    `json:"zone_id"`
	ZoneVersion int       `json:"zone_version"`
	Conclusion  string    `json:"conclusion"`
	VerifiedBy  string    `json:"verified_by"`
	VerifiedAt  time.Time `json:"verified_at"`
}

// Milestone 倒计时里程碑；封存后结论不可改，只能追加偏差说明。
type Milestone struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	ZoneID      string     `json:"zone_id"`
	ZoneVersion int        `json:"zone_version"`
	Sealed      bool       `json:"sealed"`
	SealedAt    *time.Time `json:"sealed_at,omitempty"`
	Conclusion  string     `json:"conclusion"`
}

// Deviation 已封存里程碑的偏差说明（只增不改）。
type Deviation struct {
	ID          int64     `json:"id"`
	MilestoneID string    `json:"milestone_id"`
	Note        string    `json:"note"`
	Author      string    `json:"author"`
	CreatedAt   time.Time `json:"created_at"`
}

// Notification 发给责任单位的撤离通知及其回执。
type Notification struct {
	ID          int64      `json:"id"`
	ZoneID      string     `json:"zone_id"`
	ZoneVersion int        `json:"zone_version"`
	UnitID      string     `json:"unit_id"`
	SubjectKind string     `json:"subject_kind"`
	SubjectID   string     `json:"subject_id"`
	Message     string     `json:"message"`
	CreatedAt   time.Time  `json:"created_at"`
	AckedAt     *time.Time `json:"acked_at,omitempty"`
	AckedBy     string     `json:"acked_by,omitempty"`
}

// WithinShift 判断时刻 t 是否处于班次 [start,end) 内（"HH:MM" 本地时钟）。
// end <= start 视为跨午夜班次，例如 22:00-06:00 覆盖 22:00 之后与次日 06:00 之前。
func WithinShift(start, end string, t time.Time) bool {
	hm := t.Format("15:04")
	if start == end {
		return true // 24 小时值守
	}
	if end > start {
		return hm >= start && hm < end
	}
	return hm >= start || hm < end
}
