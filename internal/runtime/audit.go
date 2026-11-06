package runtime

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"uic/internal/alarm"
	"uic/internal/audit"
	"uic/internal/auth"
	"uic/internal/config"
	"uic/internal/northbound/httpapi"
	"uic/internal/rbac"
)

// openAudit 打开审计日志。若已打开，按指纹判断是否需要重开（热更新路径）。
//
// 与历史不同，审计打开失败默认**让启动失败**：一份「审计没开起来但服务
// 在跑」的部署，在事故复盘时会发现最关键的那段时间是空白。部署方想接受
// 这个风险，必须显式写 storage.audit.enabled=false。
func (r *Runtime) openAudit(f *config.File) error {
	a := f.Storage.Audit
	if a.Enabled == nil || !*a.Enabled {
		if r.audit != nil {
			// 从「开」改成「关」是显式动作，必须留下痕迹。
			r.log.Warn("audit_disabled",
				"detail", "storage.audit.enabled=false：写入、配置变更与认证失败都不会留痕")
			_ = r.audit.Close()
			r.audit = nil
		}
		return nil
	}
	if r.audit != nil && auditDir(r.audit) == a.Dir {
		return nil
	}
	store, err := audit.Open(audit.Options{
		Dir:          a.Dir,
		MaxBytes:     a.MaxBytes,
		MaxAge:       a.MaxAge.Duration(),
		SegmentBytes: a.SegmentBytes,
		Log:          r.log,
	})
	if err != nil {
		return httpapi.Errf(http.StatusUnprocessableEntity, "config.invalid_audit", "%v", err)
	}
	if r.audit != nil {
		_ = r.audit.Close()
	}
	r.audit = store
	return nil
}

// auditDir 返回当前审计存储目录（用于判断配置是否指向别处）。
func auditDir(s *audit.Store) string { return s.Dir() }

func (r *Runtime) closeAudit() error {
	if r.audit == nil {
		return nil
	}
	err := r.audit.Close()
	r.audit = nil
	return err
}

// buildAuth 用当前配置构造凭据表。
//
// 每次配置变更都重建：删掉一个用户必须立刻生效，而不是等重启——「已经
// 删掉的人还能继续写点位」是审计里最没法解释的一种记录。
func (r *Runtime) buildAuth(f *config.File) error {
	users := make([]auth.User, 0, len(f.Server.Users))
	for _, u := range f.Server.Users {
		role, err := rbac.Parse(u.Role)
		if err != nil {
			// 配置层已校验；走到这里说明有校验没覆盖到的路径。
			return httpapi.Errf(http.StatusUnprocessableEntity, "config.invalid_user", "%v", err)
		}
		users = append(users, auth.User{
			Name: u.Name, Role: role, Token: u.Token, Sha256: u.TokenSha256,
		})
	}
	store, err := auth.NewStore(users, firstNonEmpty(r.opt.Token, f.Server.Token))
	if err != nil {
		return httpapi.Errf(http.StatusUnprocessableEntity, "config.invalid_user", "%v", err)
	}
	// 第一次构建时创建可替换容器；之后只换内容，HTTP 层手里的引用始终有效。
	if r.auth == nil {
		r.auth = auth.NewReloadable(store)
	} else {
		r.auth.Set(store)
	}
	if store.Open() {
		// 不生成默认口令、也不偷偷放宽：把事实大声说出来。
		r.log.Warn("auth_open_mode",
			"detail", "未配置任何凭据：所有接口（含写入与配置变更）对任何人开放。生产部署必须配置 server.users")
	}
	return nil
}

// AuthStore 返回当前凭据表（供诊断与测试使用）。
func (r *Runtime) AuthStore() *auth.Store {
	if r.auth == nil {
		return nil
	}
	return r.auth.Store()
}

// ---------------------------------------------------------------------------
// 审计写入
// ---------------------------------------------------------------------------

// Audit 是审计写入的唯一入口。
//
// 它刻意**不返回错误**：审计失败不能让业务动作失败，否则一次磁盘满会让整条
// 产线停摆。但失败必须计数并打日志——「静默失败」才是审计系统真正不可接受
// 的状态。
func (r *Runtime) Audit(rec audit.Record) {
	if r.audit == nil {
		return
	}
	out, err := r.audit.Append(rec)
	if err != nil {
		r.auditErrors.Add(1)
		r.log.Error("audit_append_failed", "action", rec.Action, "error", errText(err))
		r.metrics.Inc("uic_audit_write_errors_total", nil)
		return
	}
	r.metrics.Inc("uic_audit_records_total", nil)
	r.metrics.Set("uic_audit_last_seq", float64(out.Seq), nil)
}

// sourceCtxKey 标记写入来源。审计要能回答「这条写是谁发起的」——同一条
// setpoint 写入可能来自 REST、从站主站或 OPC UA 客户端，事后追责时这个
// 区别比用户名字段更关键（内部写入的 User 都是 system）。
type sourceCtxKey struct{}

// withSource 给 ctx 打上来源标记。
func withSource(ctx context.Context, source string) context.Context {
	return context.WithValue(ctx, sourceCtxKey{}, source)
}

// sourceOf 返回来源标记；未标记时视为 REST。
func sourceOf(ctx context.Context) string {
	if s, ok := ctx.Value(sourceCtxKey{}).(string); ok && s != "" {
		return s
	}
	return "api"
}

// auditFrom 写入一条带调用方身份的记录。ctx 里没有身份（内部调用）时记为 system。
func (r *Runtime) auditFrom(ctx context.Context, action, target, result, code, msg string, detail map[string]string) {
	rec := audit.Record{
		Action: action, Target: target, Result: result,
		Code: code, Message: msg, Detail: detail,
		User: "system", Role: string(rbac.RoleAdmin),
	}
	if a, ok := httpapi.ActorFrom(ctx); ok {
		rec.User, rec.Role = a.User, string(a.Role)
		rec.Remote, rec.RequestID = a.Remote, a.RequestID
	}
	if detail != nil {
		if _, ok := detail["via"]; !ok {
			detail["via"] = sourceOf(ctx)
		}
	}
	r.Audit(rec)
}

// AuditHTTP 是 httpapi.Auditor 的实现：HTTP 层把「已发生的事实」传进来，
// 这里负责落到审计存储。它同样不返回错误——认证失败不能让请求处理链崩溃。
func (r *Runtime) AuditHTTP(ev httpapi.AuditEvent) {
	rec := audit.Record{
		Action: ev.Action, Target: ev.Target, Result: ev.Result,
		Code: ev.Code, Message: ev.Message, Detail: ev.Detail,
		User: ev.User, Role: ev.Role, Remote: ev.Remote, RequestID: ev.RequestID,
	}
	if rec.User == "" {
		rec.User = "anonymous"
	}
	r.Audit(rec)
}

// auditSystem 是给调度器、从站、OPC UA 这类没有 HTTP 身份的内部写入用的。
func (r *Runtime) auditSystem(action, target, result, code, msg string) {
	r.Audit(audit.Record{
		User: "system", Role: string(rbac.RoleAdmin),
		Action: action, Target: target, Result: result, Code: code, Message: msg,
	})
}

// ---------------------------------------------------------------------------
// Backend: 审计查询
// ---------------------------------------------------------------------------

// AuditQuery 执行一次审计查询。它是北向 Backend 的实现。
func (r *Runtime) AuditQuery(f audit.Filter) (audit.Result, error) {
	if r.audit == nil {
		return audit.Result{}, httpapi.Errf(http.StatusConflict, "audit.disabled",
			"audit storage is not enabled in this configuration")
	}
	res, err := r.audit.Query(f)
	switch {
	case err == nil:
		r.metrics.Inc("uic_audit_query_total", nil)
		return res, nil
	case errors.Is(err, audit.ErrInvalidQuery):
		return res, httpapi.Errf(http.StatusBadRequest, "audit.invalid_query", "%v", err)
	case errors.Is(err, audit.ErrClosed):
		return res, httpapi.Errf(http.StatusServiceUnavailable, "audit.unavailable",
			"audit storage is closed")
	default:
		return res, httpapi.Errf(http.StatusInternalServerError, "audit.query_failed", "%v", err)
	}
}

// AuditVerify 校验哈希链。
func (r *Runtime) AuditVerify() (audit.VerifyResult, error) {
	if r.audit == nil {
		return audit.VerifyResult{}, httpapi.Errf(http.StatusConflict, "audit.disabled",
			"audit storage is not enabled in this configuration")
	}
	return r.audit.Verify()
}

// AuditStats 返回水位。第二个返回值表示是否启用。
func (r *Runtime) AuditStats() (audit.Stats, bool) {
	if r.audit == nil {
		return audit.Stats{}, false
	}
	return r.audit.Stats(), true
}

// ---------------------------------------------------------------------------
// 埋点：命令、报警确认、配置
// ---------------------------------------------------------------------------

// Command 执行白名单命令并审计。
//
// 审计包在**外面**而不是每个 case 里：命令白名单会继续增加，逐个 case 写
// 审计必然漏掉一两个，而漏掉的往往就是最危险的那个。
func (r *Runtime) Command(ctx context.Context, action string, params map[string]any) (any, error) {
	out, err := r.command(ctx, action, params)
	detail := map[string]string{}
	// 只记录标量参数，并截断长度：params 里可能有表达式或大量点位 ID，
	// 原样写进审计会让单条记录失控。
	for k, v := range params {
		if s, ok := v.(string); ok {
			if len(s) > 128 {
				s = s[:128] + "…"
			}
			detail["param."+k] = s
		}
	}
	if err != nil {
		code, msg := "command.failed", err.Error()
		var ae *httpapi.APIError
		if errors.As(err, &ae) {
			code, msg = ae.Code, ae.Message
		}
		r.auditFrom(ctx, "command."+action, action, "error", code, msg, detail)
		return out, err
	}
	r.auditFrom(ctx, "command."+action, action, "ok", "", "", detail)
	return out, nil
}

// AckAlarms 确认报警并审计。
//
// 确认本身不改设备，但它是「谁按下了确认」的唯一证据——事故复盘时这条记录
// 比报警文本更重要。
func (r *Runtime) AckAlarms(ctx context.Context, ids []string, all bool, by string) ([]alarm.Status, error) {
	out, err := r.ackAlarms(ctx, ids, all, by)
	target := strings.Join(ids, ",")
	if all {
		target = "*"
	}
	if err != nil {
		code, msg := "alarm.ack_failed", err.Error()
		var ae *httpapi.APIError
		if errors.As(err, &ae) {
			code, msg = ae.Code, ae.Message
		}
		r.auditFrom(ctx, "alarm.ack", target, "error", code, msg, map[string]string{"by": by})
		return out, err
	}
	r.auditFrom(ctx, "alarm.ack", target, "ok", "", "", map[string]string{
		"by": by, "count": strconv.Itoa(len(out)),
	})
	return out, nil
}

// AuditConfigChange 记录一次配置变更。source 说明变更从哪来（api/rollback）。
func (r *Runtime) auditConfigChange(ctx context.Context, action, version, source, result, code, msg string) {
	r.auditFrom(ctx, action, version, result, code, msg, map[string]string{"source": source})
}
