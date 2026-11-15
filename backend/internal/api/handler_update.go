// handler_update.go E8 自动更新：检查（GET /api/update/check）与落盘更新
// （POST /api/update/apply）。两端点挂在 /api 会话组内，同时受管理 IP 白名单
// 与会话鉴权保护。CATMAIL_UPDATE_REPO 未配置（updater 为 nil）= 功能关闭，
// 统一 400；GitHub 侧失败在 check 中如实入响应 error 字段（不 500，也不缓存），
// apply 的业务性拒绝（容器/Windows/校验失败/磁盘不足/并发）按 update.HTTPError
// 状态码透传文案，意外 IO 错误统一 500。
package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"catmail/internal/update"
)

// updateCheck GET /api/update/check：查询 GitHub Releases 最新版本并与当前版本
// 比较（结果进程内缓存 1h）。响应 CheckResult：GitHub 网络/限流错误如实返回
// {error: "…"}（HTTP 200——检查动作本身完成，失败原因在业务字段中，前端内联展示）。
func (s *Server) updateCheck(c *gin.Context) {
	if s.updater == nil {
		badRequest(c, "自动更新未启用（未配置 CATMAIL_UPDATE_REPO）")
		return
	}
	res, err := s.updater.Check(c.Request.Context())
	if err != nil {
		badRequest(c, err.Error()) // ErrDisabled 等配置态问题
		return
	}
	c.JSON(http.StatusOK, res)
}

// updateApply POST /api/update/apply：下载当前平台资产并 SHA-256 校验后原子替换
// 当前可执行文件。成功响应 {ok:true, installed_version, hint}；重启后生效。
func (s *Server) updateApply(c *gin.Context) {
	if s.updater == nil {
		badRequest(c, "自动更新未启用（未配置 CATMAIL_UPDATE_REPO）")
		return
	}
	res, err := s.updater.Apply(c.Request.Context())
	if err != nil {
		var he *update.HTTPError
		if errors.As(err, &he) {
			writeErr(c, he.Status, TypeInvalidRequest, he.Msg)
			return
		}
		serverError(c, err) // 意外 IO/网络错误：详情进服务端日志，客户端通用文案
		return
	}
	c.JSON(http.StatusOK, res)
}
