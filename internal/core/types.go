// Package core 实现勤务区撤离放行服务的领域模型与业务规则。
//
// 业务背景：运载火箭转运就位后、切换倒计时管制前，地面保障指挥必须确认
// 各危险区内的人员、车辆与活动门架已全部撤离。加注、消防、测量等班组经
// 不同频道报告撤离，本服务把危险区版本、岗位名册、车辆设备、门禁回执、
// 班组确认与倒计时里程碑关联起来，形成一致的清场结论。
package core

import "fmt"

// Role 操作角色。权限矩阵见 server 包路由注册。
type Role string

const (
	RoleCrewLead        Role = "crew_lead"        // 班组负责人：确认本组状态
	RoleGroundCommander Role = "ground_commander" // 地面保障指挥：核对区域、解除冻结、发布放行
	RoleLaunchCommander Role = "launch_commander" // 发射指挥：只能接收最终放行结论
	RoleContractor      Role = "contractor"       // 承包单位：仅见本单位人员资料与通知
	RoleGateOperator    Role = "gate_operator"    // 门禁系统：录入门禁回执
)

// Actor 已认证操作者。
type Actor struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Role   Role   `json:"role"`
	UnitID string `json:"unit_id,omitempty"` // 所属单位（承包单位/班组所属单位）
	CrewID string `json:"crew_id,omitempty"` // 所属班组（班组负责人）
}

// ObjectType 清场对象类型。
type ObjectType string

const (
	ObjectPerson  ObjectType = "person"
	ObjectVehicle ObjectType = "vehicle"
)

// Direction 门禁方向。
type Direction string

const (
	DirIn  Direction = "in"
	DirOut Direction = "out"
)

// 冻结原因。任一冻结未解除时，对应区域不得确认、核对与放行。
const (
	FreezeMissingReceipt   = "missing_receipt"   // 缺少门禁回执：名册对象无任何进出记录
	FreezeReentry          = "reentry"           // 人员/车辆在班组确认后重新进入危险区
	FreezeBoundaryExpanded = "boundary_expanded" // 区域边界扩大：既有确认全部作废
)

// Zone 危险区。
type Zone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ZoneVersion 危险区边界版本。边界每次变更（尤其扩大）产生新版本，
// 班组确认与区域核对都绑定到具体版本，版本变更后旧确认自动失效。
type ZoneVersion struct {
	ZoneID    string `json:"zone_id"`
	Version   int    `json:"version"`
	Boundary  string `json:"boundary"` // 边界描述（坐标串/说明）
	Expanded  bool   `json:"expanded"` // 相对上一版是否扩大
	CreatedAt int64  `json:"created_at"`
}

// Unit 责任单位（承包单位）。
type Unit struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Crew 班组（加注/消防/测量等）。
type Crew struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	UnitID    string `json:"unit_id"`
	Specialty string `json:"specialty"`
}

// Person 岗位名册人员。
type Person struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	UnitID string `json:"unit_id"`
	CrewID string `json:"crew_id"`
	Post   string `json:"post"` // 岗位
}

// Vehicle 车辆设备（含活动门架）。
type Vehicle struct {
	ID     string `json:"id"`
	Plate  string `json:"plate"`
	Kind   string `json:"kind"` // truck / gantry(活动门架) / fire_engine ...
	UnitID string `json:"unit_id"`
	CrewID string `json:"crew_id"`
}

// Shift 班次。起止为绝对时间戳，跨午夜班次（如 22:00–次日 06:00）
// 通过时间戳区间自然表达，归属判断不做时分比较。
type Shift struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	StartsAt int64  `json:"starts_at"`
	EndsAt   int64  `json:"ends_at"`
}

// Assignment 区域作业派遣：某对象被派往某区域作业，是清场核对的基准名册。
type Assignment struct {
	ID         int64      `json:"id"`
	ZoneID     string     `json:"zone_id"`
	ObjectType ObjectType `json:"object_type"`
	ObjectID   string     `json:"object_id"`
	CrewID     string     `json:"crew_id"`
	UnitID     string     `json:"unit_id"`
	ShiftID    string     `json:"shift_id"`
	Active     bool       `json:"active"`
}

// GateEvent 门禁回执。ReceiptNo 是门禁系统回执号，全库唯一，
// 重复扫码（网络重发、闸机重传）按幂等处理，不重复计数。
type GateEvent struct {
	ID         int64      `json:"id"`
	ReceiptNo  string     `json:"receipt_no"`
	GateID     string     `json:"gate_id"`
	ZoneID     string     `json:"zone_id"`
	ObjectType ObjectType `json:"object_type"`
	ObjectID   string     `json:"object_id"`
	Direction  Direction  `json:"direction"`
	OccurredAt int64      `json:"occurred_at"` // 门禁系统时间
	ReceivedAt int64      `json:"received_at"` // 本服务接收时间
	Duplicate  bool       `json:"duplicate"`   // 仅响应使用：本次提交命中已有回执
}

// CrewConfirmation 班组确认：班组负责人声明本组在某区域当前版本已撤离。
type CrewConfirmation struct {
	ZoneID      string `json:"zone_id"`
	ZoneVersion int    `json:"zone_version"`
	CrewID      string `json:"crew_id"`
	ShiftID     string `json:"shift_id"`
	ConfirmedBy string `json:"confirmed_by"`
	ConfirmedAt int64  `json:"confirmed_at"`
	Note        string `json:"note,omitempty"`
}

// ZoneVerification 区域核对结论：地面保障指挥确认某区域当前版本已清场。
type ZoneVerification struct {
	ZoneID      string `json:"zone_id"`
	ZoneVersion int    `json:"zone_version"`
	VerifiedBy  string `json:"verified_by"`
	VerifiedAt  int64  `json:"verified_at"`
	Result      string `json:"result"` // cleared
}

// Freeze 冻结记录。冻结未解除前，区域不得确认、核对、放行。
type Freeze struct {
	ID         int64  `json:"id"`
	ZoneID     string `json:"zone_id"`
	Reason     string `json:"reason"`
	ObjectKey  string `json:"object_key,omitempty"` // 关联对象（缺回执/重新进入）
	Detail     string `json:"detail"`
	RaisedAt   int64  `json:"raised_at"`
	RaisedBy   string `json:"raised_by"` // system 或操作者
	ResolvedAt *int64 `json:"resolved_at,omitempty"`
	ResolvedBy string `json:"resolved_by,omitempty"`
	Resolution string `json:"resolution,omitempty"`
}

// Milestone 倒计时里程碑。封存后本体不可变，只能追加偏差说明。
type Milestone struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Seq      int    `json:"seq"`
	Sealed   bool   `json:"sealed"`
	SealedAt *int64 `json:"sealed_at,omitempty"`
	SealedBy string `json:"sealed_by,omitempty"`
}

// Deviation 里程碑偏差说明（只增不改）。
type Deviation struct {
	ID          int64  `json:"id"`
	MilestoneID string `json:"milestone_id"`
	Note        string `json:"note"`
	CreatedBy   string `json:"created_by"`
	CreatedAt   int64  `json:"created_at"`
}

// Release 最终放行结论。发射指挥只能读取本结论。
type Release struct {
	ID         int64  `json:"id"`
	Conclusion string `json:"conclusion"`
	IssuedBy   string `json:"issued_by"`
	IssuedAt   int64  `json:"issued_at"`
	Basis      string `json:"basis"` // 放行依据摘要（各区域版本与核对时间，JSON）
}

// Notification 发往责任单位的通知；AckedAt 即通知回执。
type Notification struct {
	ID        int64  `json:"id"`
	ZoneID    string `json:"zone_id"`
	UnitID    string `json:"unit_id"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	CreatedAt int64  `json:"created_at"`
	AckedAt   *int64 `json:"acked_at,omitempty"`
	AckedBy   string `json:"acked_by,omitempty"`
}

// PendingObject 未撤离对象及其责任单位。
type PendingObject struct {
	ObjectType     ObjectType `json:"object_type"`
	ObjectID       string     `json:"object_id"`
	ObjectName     string     `json:"object_name"`
	UnitID         string     `json:"unit_id"`
	CrewID         string     `json:"crew_id"`
	Net            int        `json:"net"`             // 区内净数量（进-出）
	MissingReceipt bool       `json:"missing_receipt"` // 名册在列但无任何门禁回执
}

// ZoneDependency 区域依赖的清场状态。
type ZoneDependency struct {
	ZoneID  string `json:"zone_id"`
	Cleared bool   `json:"cleared"`
}

// ZoneStatus 区域清场状态，指挥席看板的基本单元。
type ZoneStatus struct {
	ZoneID             string             `json:"zone_id"`
	ZoneName           string             `json:"zone_name"`
	CurrentVersion     int                `json:"current_version"`
	Verified           bool               `json:"verified"`
	LastConfirmationAt *int64             `json:"last_confirmation_at,omitempty"`
	PendingObjects     []PendingObject    `json:"pending_objects"`
	OpenFreezes        []Freeze           `json:"open_freezes"`
	Confirmations      []CrewConfirmation `json:"confirmations"`
	Dependencies       []ZoneDependency   `json:"dependencies"`
	Notifications      []Notification     `json:"notifications"`
}

// Error 业务错误，携带 HTTP 状态码与可选明细（如未撤离对象清单）。
type Error struct {
	Status  int
	Message string
	Details any
}

func (e *Error) Error() string { return e.Message }

// BadRequest 400。
func BadRequest(format string, args ...any) *Error {
	return &Error{Status: 400, Message: fmt.Sprintf(format, args...)}
}

// Forbidden 403。
func Forbidden(format string, args ...any) *Error {
	return &Error{Status: 403, Message: fmt.Sprintf(format, args...)}
}

// NotFound 404。
func NotFound(format string, args ...any) *Error {
	return &Error{Status: 404, Message: fmt.Sprintf(format, args...)}
}

// Conflict 409，可携带明细（如未撤离对象列表）。
func Conflict(format string, args ...any) *Error {
	return &Error{Status: 409, Message: fmt.Sprintf(format, args...)}
}

// ConflictWithDetails 409 带明细。
func ConflictWithDetails(details any, format string, args ...any) *Error {
	return &Error{Status: 409, Message: fmt.Sprintf(format, args...), Details: details}
}
