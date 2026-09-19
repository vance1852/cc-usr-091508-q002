// Package server 提供勤务区撤离放行服务的 Fiber HTTP API。
//
// 认证：演示环境通过 X-Actor-Id 头标识操作者（对应 actors 表）。
// 授权：按角色路由级控制 —— 班组负责人只能确认本组，地面保障指挥核对区域，
// 发射指挥只能读取最终放行结论，承包单位仅见本单位人员资料与通知。
package server

import (
	"errors"
	"strconv"

	"github.com/gofiber/fiber/v2"

	"evacsvc/internal/core"
)

// New 组装 Fiber 应用。
func New(svc *core.Service) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			var ae *core.Error
			if errors.As(err, &ae) {
				body := fiber.Map{"error": ae.Message}
				if ae.Details != nil {
					body["details"] = ae.Details
				}
				return c.Status(ae.Status).JSON(body)
			}
			var fe *fiber.Error
			if errors.As(err, &fe) {
				return c.Status(fe.Code).JSON(fiber.Map{"error": fe.Message})
			}
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		},
	})

	api := app.Group("/api/v1", auth(svc))

	// 门禁回执：门禁系统/地面保障指挥/班组负责人可录入（重复扫码幂等）。
	api.Post("/gate-events", require(core.RoleGateOperator, core.RoleGroundCommander, core.RoleCrewLead), h(svc).recordGateEvent)

	// 区域状态与看板：班组负责人与地面保障指挥。
	api.Get("/zones", require(core.RoleCrewLead, core.RoleGroundCommander), h(svc).listZones)
	api.Get("/zones/:id/status", require(core.RoleCrewLead, core.RoleGroundCommander), h(svc).zoneStatus)
	api.Get("/zones/:id/pending", require(core.RoleCrewLead, core.RoleGroundCommander), h(svc).zonePending)
	api.Post("/zones/:id/boundary", require(core.RoleGroundCommander), h(svc).expandBoundary)
	api.Post("/zones/:id/crews/:crewId/confirm", require(core.RoleCrewLead), h(svc).confirmCrew)
	api.Post("/zones/:id/verify", require(core.RoleGroundCommander), h(svc).verifyZone)
	api.Post("/zones/:id/notify", require(core.RoleGroundCommander), h(svc).notifyUnit)
	api.Get("/dashboard", require(core.RoleCrewLead, core.RoleGroundCommander), h(svc).dashboard)

	// 冻结。
	api.Get("/freezes", require(core.RoleCrewLead, core.RoleGroundCommander), h(svc).listFreezes)
	api.Post("/freezes/:id/resolve", require(core.RoleGroundCommander), h(svc).resolveFreeze)

	// 名册：承包单位仅本单位，班组负责人仅本组（在 service 层过滤）。
	api.Get("/personnel", require(core.RoleCrewLead, core.RoleGroundCommander, core.RoleContractor), h(svc).personnel)
	api.Get("/vehicles", require(core.RoleCrewLead, core.RoleGroundCommander, core.RoleContractor), h(svc).vehicles)

	// 通知与回执。
	api.Get("/notifications", require(core.RoleGroundCommander, core.RoleContractor), h(svc).notifications)
	api.Post("/notifications/:id/ack", require(core.RoleContractor), h(svc).ackNotification)

	// 放行结论：地面保障指挥发布；发射指挥只能读取（含班组负责人可查）。
	api.Post("/releases", require(core.RoleGroundCommander), h(svc).issueRelease)
	api.Get("/releases", require(core.RoleGroundCommander, core.RoleLaunchCommander, core.RoleCrewLead), h(svc).listReleases)
	api.Get("/releases/latest", require(core.RoleGroundCommander, core.RoleLaunchCommander, core.RoleCrewLead), h(svc).latestRelease)

	// 里程碑：封存后只能追加偏差说明。
	api.Get("/milestones", require(core.RoleCrewLead, core.RoleGroundCommander), h(svc).listMilestones)
	api.Get("/milestones/:id/deviations", require(core.RoleCrewLead, core.RoleGroundCommander), h(svc).listDeviations)
	api.Post("/milestones/:id/seal", require(core.RoleGroundCommander), h(svc).sealMilestone)
	api.Post("/milestones/:id/deviations", require(core.RoleGroundCommander, core.RoleCrewLead), h(svc).addDeviation)

	// 班次回执（跨午夜班次归属）。
	api.Get("/shifts/:id/receipts", require(core.RoleCrewLead, core.RoleGroundCommander), h(svc).shiftReceipts)

	return app
}

// auth 解析 X-Actor-Id 并校验操作者存在。
func auth(svc *core.Service) fiber.Handler {
	return func(c *fiber.Ctx) error {
		id := c.Get("X-Actor-Id")
		if id == "" {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "缺少 X-Actor-Id 头"})
		}
		actor, err := svc.GetActor(id)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "操作者不存在或未注册"})
		}
		c.Locals("actor", actor)
		return c.Next()
	}
}

// require 角色准入；发射指挥不在任何写操作及看板路由的白名单内。
func require(roles ...core.Role) fiber.Handler {
	return func(c *fiber.Ctx) error {
		actor := c.Locals("actor").(core.Actor)
		for _, r := range roles {
			if actor.Role == r {
				return c.Next()
			}
		}
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "角色 " + string(actor.Role) + " 无权执行此操作"})
	}
}

func actorOf(c *fiber.Ctx) core.Actor { return c.Locals("actor").(core.Actor) }

type handlers struct{ svc *core.Service }

func h(svc *core.Service) *handlers { return &handlers{svc: svc} }

func (hh *handlers) recordGateEvent(c *fiber.Ctx) error {
	var body struct {
		ReceiptNo  string          `json:"receipt_no"`
		GateID     string          `json:"gate_id"`
		ZoneID     string          `json:"zone_id"`
		ObjectType core.ObjectType `json:"object_type"`
		ObjectID   string          `json:"object_id"`
		Direction  core.Direction  `json:"direction"`
		OccurredAt int64           `json:"occurred_at"`
	}
	if err := c.BodyParser(&body); err != nil {
		return core.BadRequest("请求体解析失败: %v", err)
	}
	ev, err := hh.svc.RecordGateEvent(core.GateEvent{
		ReceiptNo: body.ReceiptNo, GateID: body.GateID, ZoneID: body.ZoneID,
		ObjectType: body.ObjectType, ObjectID: body.ObjectID, Direction: body.Direction,
		OccurredAt: body.OccurredAt,
	})
	if err != nil {
		return err
	}
	if ev.Duplicate {
		return c.Status(fiber.StatusOK).JSON(ev) // 重复扫码：200 + duplicate 标记
	}
	return c.Status(fiber.StatusCreated).JSON(ev)
}

func (hh *handlers) listZones(c *fiber.Ctx) error {
	zones, err := hh.svc.ListZones()
	if err != nil {
		return err
	}
	return c.JSON(zones)
}

func (hh *handlers) zoneStatus(c *fiber.Ctx) error {
	st, err := hh.svc.ZoneStatus(c.Params("id"))
	if err != nil {
		return err
	}
	return c.JSON(st)
}

func (hh *handlers) zonePending(c *fiber.Ctx) error {
	pending, err := hh.svc.PendingObjects(c.Params("id"))
	if err != nil {
		return err
	}
	return c.JSON(pending)
}

func (hh *handlers) expandBoundary(c *fiber.Ctx) error {
	var body struct {
		Boundary string `json:"boundary"`
		Expanded bool   `json:"expanded"`
	}
	if err := c.BodyParser(&body); err != nil {
		return core.BadRequest("请求体解析失败: %v", err)
	}
	v, err := hh.svc.ExpandZoneBoundary(actorOf(c), c.Params("id"), body.Boundary, body.Expanded)
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(v)
}

func (hh *handlers) confirmCrew(c *fiber.Ctx) error {
	var body struct {
		Note string `json:"note"`
	}
	_ = c.BodyParser(&body) // note 可空
	cc, err := hh.svc.ConfirmCrew(actorOf(c), c.Params("id"), c.Params("crewId"), body.Note)
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(cc)
}

func (hh *handlers) verifyZone(c *fiber.Ctx) error {
	zv, err := hh.svc.VerifyZone(actorOf(c), c.Params("id"))
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(zv)
}

func (hh *handlers) notifyUnit(c *fiber.Ctx) error {
	var body struct {
		UnitID  string `json:"unit_id"`
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}
	if err := c.BodyParser(&body); err != nil {
		return core.BadRequest("请求体解析失败: %v", err)
	}
	n, err := hh.svc.NotifyUnit(actorOf(c), c.Params("id"), body.UnitID, body.Subject, body.Body)
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(n)
}

func (hh *handlers) dashboard(c *fiber.Ctx) error {
	st, err := hh.svc.Dashboard()
	if err != nil {
		return err
	}
	return c.JSON(st)
}

func (hh *handlers) listFreezes(c *fiber.Ctx) error {
	openOnly := c.Query("open", "true") == "true"
	freezes, err := hh.svc.ListFreezes(openOnly)
	if err != nil {
		return err
	}
	return c.JSON(freezes)
}

func (hh *handlers) resolveFreeze(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil {
		return core.BadRequest("冻结 ID 非法")
	}
	var body struct {
		Resolution string `json:"resolution"`
	}
	if err := c.BodyParser(&body); err != nil {
		return core.BadRequest("请求体解析失败: %v", err)
	}
	if err := hh.svc.ResolveFreeze(actorOf(c), id, body.Resolution); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"resolved": id})
}

func (hh *handlers) personnel(c *fiber.Ctx) error {
	list, err := hh.svc.Personnel(actorOf(c))
	if err != nil {
		return err
	}
	return c.JSON(list)
}

func (hh *handlers) vehicles(c *fiber.Ctx) error {
	list, err := hh.svc.Vehicles(actorOf(c))
	if err != nil {
		return err
	}
	return c.JSON(list)
}

func (hh *handlers) notifications(c *fiber.Ctx) error {
	list, err := hh.svc.ListNotifications(actorOf(c))
	if err != nil {
		return err
	}
	return c.JSON(list)
}

func (hh *handlers) ackNotification(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil {
		return core.BadRequest("通知 ID 非法")
	}
	n, err := hh.svc.AckNotification(actorOf(c), id)
	if err != nil {
		return err
	}
	return c.JSON(n)
}

func (hh *handlers) issueRelease(c *fiber.Ctx) error {
	var body struct {
		Conclusion string `json:"conclusion"`
	}
	if err := c.BodyParser(&body); err != nil {
		return core.BadRequest("请求体解析失败: %v", err)
	}
	r, err := hh.svc.IssueRelease(actorOf(c), body.Conclusion)
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(r)
}

func (hh *handlers) listReleases(c *fiber.Ctx) error {
	list, err := hh.svc.ListReleases()
	if err != nil {
		return err
	}
	return c.JSON(list)
}

func (hh *handlers) latestRelease(c *fiber.Ctx) error {
	latest, err := hh.svc.LatestRelease()
	if err != nil {
		return err
	}
	if latest == nil {
		return core.NotFound("尚无放行结论")
	}
	return c.JSON(latest)
}

func (hh *handlers) listMilestones(c *fiber.Ctx) error {
	list, err := hh.svc.ListMilestones()
	if err != nil {
		return err
	}
	return c.JSON(list)
}

func (hh *handlers) listDeviations(c *fiber.Ctx) error {
	list, err := hh.svc.ListDeviations(c.Params("id"))
	if err != nil {
		return err
	}
	return c.JSON(list)
}

func (hh *handlers) sealMilestone(c *fiber.Ctx) error {
	m, err := hh.svc.SealMilestone(actorOf(c), c.Params("id"))
	if err != nil {
		return err
	}
	return c.JSON(m)
}

func (hh *handlers) addDeviation(c *fiber.Ctx) error {
	var body struct {
		Note string `json:"note"`
	}
	if err := c.BodyParser(&body); err != nil {
		return core.BadRequest("请求体解析失败: %v", err)
	}
	d, err := hh.svc.AddDeviation(actorOf(c), c.Params("id"), body.Note)
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(d)
}

func (hh *handlers) shiftReceipts(c *fiber.Ctx) error {
	shift, events, err := hh.svc.ShiftReceipts(c.Params("id"))
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"shift": shift, "receipts": events, "count": len(events)})
}
