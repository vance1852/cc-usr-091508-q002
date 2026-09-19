// Package api 提供 Fiber HTTP 接口与基于角色的访问控制。
package api

import (
	"errors"
	"time"

	"evac/internal/domain"
	"evac/internal/service"

	"github.com/gofiber/fiber/v2"
)

// NewApp 构建 Fiber 应用。身份通过请求头传入：
// X-User-Id、X-Role（crew_lead/ground_command/launch_command/contractor）、
// X-Unit-Id（承包单位）、X-Crew-Id（班组负责人）。
func NewApp(svc *service.Service) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			var se *service.Error
			if errors.As(err, &se) {
				status := fiber.StatusBadRequest
				switch se.Kind {
				case "not_found":
					status = fiber.StatusNotFound
				case "forbidden":
					status = fiber.StatusForbidden
				case "conflict":
					status = fiber.StatusConflict
				}
				return c.Status(status).JSON(fiber.Map{
					"error":   se.Code,
					"message": se.Message,
					"details": se.Details,
				})
			}
			var fe *fiber.Error
			if errors.As(err, &fe) {
				return c.Status(fe.Code).JSON(fiber.Map{"error": "http_error", "message": fe.Message})
			}
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal", "message": err.Error()})
		},
	})

	principal := func(c *fiber.Ctx) service.Principal {
		return service.Principal{
			UserID: c.Get("X-User-Id"),
			Role:   domain.Role(c.Get("X-Role")),
			UnitID: c.Get("X-Unit-Id"),
			CrewID: c.Get("X-Crew-Id"),
		}
	}
	require := func(roles ...domain.Role) fiber.Handler {
		return func(c *fiber.Ctx) error {
			p := principal(c)
			if p.UserID == "" {
				return fiber.ErrUnauthorized
			}
			for _, r := range roles {
				if p.Role == r {
					return c.Next()
				}
			}
			return &service.Error{Kind: "forbidden", Code: "forbidden", Message: "当前角色无权执行该操作"}
		}
	}
	gc := require(domain.RoleGroundCommand)

	api := app.Group("/api")

	// ---- 档案登记（地面保障指挥） ----
	api.Post("/zones", gc, func(c *fiber.Ctx) error {
		var body struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := c.BodyParser(&body); err != nil || body.ID == "" || body.Name == "" {
			return &service.Error{Kind: "bad_request", Code: "bad_request", Message: "id 与 name 必填"}
		}
		z, err := svc.CreateZone(body.ID, body.Name)
		if err != nil {
			return err
		}
		return c.Status(fiber.StatusCreated).JSON(z)
	})
	api.Post("/zones/:id/versions", gc, func(c *fiber.Ctx) error {
		var body struct {
			Boundary string `json:"boundary"`
			Expanded bool   `json:"expanded"`
		}
		if err := c.BodyParser(&body); err != nil || body.Boundary == "" {
			return &service.Error{Kind: "bad_request", Code: "bad_request", Message: "boundary 必填"}
		}
		v, err := svc.PublishZoneVersion(c.Params("id"), body.Boundary, body.Expanded)
		if err != nil {
			return err
		}
		return c.Status(fiber.StatusCreated).JSON(v)
	})
	api.Post("/zones/:id/dependencies", gc, func(c *fiber.Ctx) error {
		var body struct {
			DependsOn string `json:"depends_on"`
		}
		if err := c.BodyParser(&body); err != nil || body.DependsOn == "" {
			return &service.Error{Kind: "bad_request", Code: "bad_request", Message: "depends_on 必填"}
		}
		if err := svc.AddDependency(c.Params("id"), body.DependsOn); err != nil {
			return err
		}
		return c.SendStatus(fiber.StatusNoContent)
	})
	api.Post("/units", gc, func(c *fiber.Ctx) error {
		var body domain.Unit
		if err := c.BodyParser(&body); err != nil || body.ID == "" || body.Name == "" {
			return &service.Error{Kind: "bad_request", Code: "bad_request", Message: "id 与 name 必填"}
		}
		if err := svc.RegisterUnit(body.ID, body.Name); err != nil {
			return err
		}
		return c.Status(fiber.StatusCreated).JSON(body)
	})
	api.Post("/crews", gc, func(c *fiber.Ctx) error {
		var body domain.Crew
		if err := c.BodyParser(&body); err != nil || body.ID == "" {
			return &service.Error{Kind: "bad_request", Code: "bad_request", Message: "id 必填"}
		}
		if err := svc.RegisterCrew(body); err != nil {
			return err
		}
		return c.Status(fiber.StatusCreated).JSON(body)
	})
	api.Post("/persons", gc, func(c *fiber.Ctx) error {
		var body domain.Person
		if err := c.BodyParser(&body); err != nil || body.ID == "" {
			return &service.Error{Kind: "bad_request", Code: "bad_request", Message: "id 必填"}
		}
		if err := svc.RegisterPerson(body); err != nil {
			return err
		}
		return c.Status(fiber.StatusCreated).JSON(body)
	})
	api.Post("/assets", gc, func(c *fiber.Ctx) error {
		var body domain.Asset
		if err := c.BodyParser(&body); err != nil || body.ID == "" {
			return &service.Error{Kind: "bad_request", Code: "bad_request", Message: "id 必填"}
		}
		if err := svc.RegisterAsset(body); err != nil {
			return err
		}
		return c.Status(fiber.StatusCreated).JSON(body)
	})

	// ---- 门禁回执 ----
	api.Post("/gate-receipts", gc, func(c *fiber.Ctx) error {
		var body struct {
			ScanID      string `json:"scan_id"`
			SubjectKind string `json:"subject_kind"`
			SubjectID   string `json:"subject_id"`
			Gate        string `json:"gate"`
			Direction   string `json:"direction"`
			ScannedAt   string `json:"scanned_at"`
		}
		if err := c.BodyParser(&body); err != nil {
			return &service.Error{Kind: "bad_request", Code: "bad_request", Message: "请求体格式错误"}
		}
		scannedAt, err := time.Parse(time.RFC3339, body.ScannedAt)
		if err != nil {
			return &service.Error{Kind: "bad_request", Code: "bad_request", Message: "scanned_at 须为 RFC3339 时间"}
		}
		r, created, err := svc.RecordGateReceipt(service.ReceiptInput{
			ScanID:      body.ScanID,
			SubjectKind: body.SubjectKind,
			SubjectID:   body.SubjectID,
			Gate:        body.Gate,
			Direction:   domain.Direction(body.Direction),
			ScannedAt:   scannedAt,
		})
		if err != nil {
			return err
		}
		status := fiber.StatusCreated
		if !created {
			status = fiber.StatusOK // 重复扫码：幂等返回原回执
		}
		return c.Status(status).JSON(fiber.Map{"receipt": r, "created": created})
	})

	// ---- 班组确认（班组负责人） ----
	api.Post("/crews/:id/confirmations", require(domain.RoleCrewLead), func(c *fiber.Ctx) error {
		conf, created, err := svc.ConfirmCrew(c.Params("id"), principal(c))
		if err != nil {
			return err
		}
		status := fiber.StatusCreated
		if !created {
			status = fiber.StatusOK
		}
		return c.Status(status).JSON(fiber.Map{"confirmation": conf, "created": created})
	})

	// ---- 清场评估 / 核对放行 / 冻结（地面保障指挥） ----
	api.Get("/zones/:id/clearance", gc, func(c *fiber.Ctx) error {
		cl, err := svc.Evaluate(c.Params("id"))
		if err != nil {
			return err
		}
		return c.JSON(cl)
	})
	api.Post("/zones/:id/verify", gc, func(c *fiber.Ctx) error {
		rel, cl, err := svc.VerifyZone(c.Params("id"), principal(c).UserID)
		if err != nil {
			return err
		}
		return c.JSON(fiber.Map{"release": rel, "clearance": cl})
	})
	api.Post("/zones/:id/notify", gc, func(c *fiber.Ctx) error {
		ns, err := svc.NotifyZone(c.Params("id"))
		if err != nil {
			return err
		}
		return c.JSON(fiber.Map{"notifications": ns})
	})
	api.Post("/zones/:id/freezes/:fid/resolve", gc, func(c *fiber.Ctx) error {
		fid, err := c.ParamsInt("fid")
		if err != nil {
			return &service.Error{Kind: "bad_request", Code: "bad_request", Message: "冻结 ID 非法"}
		}
		if err := svc.ResolveFreeze(c.Params("id"), int64(fid), principal(c).UserID); err != nil {
			return err
		}
		return c.SendStatus(fiber.StatusNoContent)
	})

	// ---- 指挥席总览（地面保障指挥） ----
	api.Get("/dashboard", gc, func(c *fiber.Ctx) error {
		dash, err := svc.Dashboard()
		if err != nil {
			return err
		}
		return c.JSON(fiber.Map{"zones": dash})
	})

	// ---- 最终放行结论（发射指挥只读接收；地面保障指挥亦可查看） ----
	api.Get("/releases", require(domain.RoleLaunchCommand, domain.RoleGroundCommand), func(c *fiber.Ctx) error {
		rs, err := svc.ListReleases()
		if err != nil {
			return err
		}
		return c.JSON(fiber.Map{"releases": rs})
	})
	api.Get("/releases/:zoneId", require(domain.RoleLaunchCommand, domain.RoleGroundCommand), func(c *fiber.Ctx) error {
		rv, err := svc.GetRelease(c.Params("zoneId"))
		if err != nil {
			return err
		}
		return c.JSON(rv)
	})

	// ---- 人员资料（数据范围按角色限定） ----
	api.Get("/persons", require(domain.RoleGroundCommand, domain.RoleContractor, domain.RoleCrewLead), func(c *fiber.Ctx) error {
		ps, err := svc.ListPersons(principal(c), c.Query("zone_id"), c.Query("unit_id"))
		if err != nil {
			return err
		}
		return c.JSON(fiber.Map{"persons": ps})
	})

	// ---- 通知与回执 ----
	api.Get("/notifications", require(domain.RoleGroundCommand, domain.RoleContractor), func(c *fiber.Ctx) error {
		ns, err := svc.ListNotifications(principal(c))
		if err != nil {
			return err
		}
		return c.JSON(fiber.Map{"notifications": ns})
	})
	api.Post("/notifications/:id/ack", require(domain.RoleGroundCommand, domain.RoleContractor), func(c *fiber.Ctx) error {
		id, err := c.ParamsInt("id")
		if err != nil {
			return &service.Error{Kind: "bad_request", Code: "bad_request", Message: "通知 ID 非法"}
		}
		n, err := svc.AckNotification(int64(id), principal(c))
		if err != nil {
			return err
		}
		return c.JSON(n)
	})

	// ---- 倒计时里程碑 ----
	api.Post("/zones/:id/milestones", gc, func(c *fiber.Ctx) error {
		var body struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
		}
		if err := c.BodyParser(&body); err != nil || body.ID == "" || body.Name == "" {
			return &service.Error{Kind: "bad_request", Code: "bad_request", Message: "id 与 name 必填"}
		}
		m, err := svc.CreateMilestone(c.Params("id"), body.ID, body.Name, body.Conclusion)
		if err != nil {
			return err
		}
		return c.Status(fiber.StatusCreated).JSON(m)
	})
	api.Put("/milestones/:id", gc, func(c *fiber.Ctx) error {
		var body struct {
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
		}
		if err := c.BodyParser(&body); err != nil {
			return &service.Error{Kind: "bad_request", Code: "bad_request", Message: "请求体格式错误"}
		}
		m, err := svc.UpdateMilestone(c.Params("id"), body.Name, body.Conclusion)
		if err != nil {
			return err
		}
		return c.JSON(m)
	})
	api.Post("/milestones/:id/seal", gc, func(c *fiber.Ctx) error {
		m, err := svc.SealMilestone(c.Params("id"))
		if err != nil {
			return err
		}
		return c.JSON(m)
	})
	api.Post("/milestones/:id/deviations", gc, func(c *fiber.Ctx) error {
		var body struct {
			Note string `json:"note"`
		}
		if err := c.BodyParser(&body); err != nil || body.Note == "" {
			return &service.Error{Kind: "bad_request", Code: "bad_request", Message: "note 必填"}
		}
		d, err := svc.AppendDeviation(c.Params("id"), body.Note, principal(c).UserID)
		if err != nil {
			return err
		}
		return c.Status(fiber.StatusCreated).JSON(d)
	})
	api.Get("/milestones", require(domain.RoleGroundCommand, domain.RoleLaunchCommand), func(c *fiber.Ctx) error {
		ms, err := svc.ListMilestones()
		if err != nil {
			return err
		}
		return c.JSON(fiber.Map{"milestones": ms})
	})
	api.Get("/milestones/:id/deviations", require(domain.RoleGroundCommand, domain.RoleLaunchCommand), func(c *fiber.Ctx) error {
		ds, err := svc.ListDeviations(c.Params("id"))
		if err != nil {
			return err
		}
		return c.JSON(fiber.Map{"deviations": ds})
	})

	return app
}
