// handler_idempotency.go E2 幂等键防重发：Idempotency-Key 闸门（首次认领/
// 存档重放/并发 409/僵尸 pending 回收）与响应捕获存档。仅由 send 入口接线
// （handler_send.go），其余端点与无 key 请求路径完全不受影响。
package api

import (
	"bytes"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"catmail/internal/model"
)

// 幂等语义参数（契约）：
//   - idemArchiveTTL 存档保留期：超期档案懒惰清理后，同 key 视为「无行」重新发送；
//   - idemPendingTTL 僵尸 pending 判定期：pending 超过该时限视为进程崩溃残留，
//     可被新的同 key 请求原子认领重发（进行中的正常发送远快于此，见 captureWriter
//     处对极小重放窗口的说明）。
const (
	idemArchiveTTL = 24 * time.Hour
	idemPendingTTL = 5 * time.Minute
)

// 请求/响应头：Idempotency-Key 调用方提供的幂等键；Idempotent-Replay 仅在
// 重放响应上置 true，便于调用方与运维识别「本次未实际发送」。
const (
	idemHeader       = "Idempotency-Key"
	idemReplayHeader = "Idempotent-Replay"
)

// idemKeyMaxLen 幂等键上限（去首尾空格后的字符数）。
const idemKeyMaxLen = 128

// idemConflictMsg 同 key 并发进行中的 409 文案（契约指定）。
const idemConflictMsg = "相同的 Idempotency-Key 请求正在处理中"

// idempotencyKeyOf 提取并校验 Idempotency-Key：
//   - 头缺失（或去首尾空格后为空）→ ("", true)：完全不启用去重，现状行为不变；
//   - 非法（超 128 字符 / 含非可见 ASCII 字符）→ 写 400 invalid_request，返回 false。
//
// 非法 key 不静默降级为「无 key」：调用方显式携带 key 却被悄悄去掉去重，
// 网络重试可能造成重复发送，比直接报错更糟。可见打印 ASCII 以 0x21–0x7E 为准
// （空格不属于可见字符；首尾空格已 trim，键内空格同样拒绝）。
func idempotencyKeyOf(c *gin.Context) (string, bool) {
	key := strings.TrimSpace(c.GetHeader(idemHeader))
	if key == "" {
		return "", true // 缺省 = 不启用（纯空白头等价于缺省）
	}
	if utf8.RuneCountInString(key) > idemKeyMaxLen {
		badRequest(c, "Idempotency-Key 过长（≤128 字符）")
		return "", false
	}
	for _, r := range key {
		if r < 0x21 || r > 0x7e {
			badRequest(c, "Idempotency-Key 仅允许可见打印 ASCII 字符")
			return "", false
		}
	}
	return key, true
}

// idemClaim 幂等闸门：返回 true 表示本请求获得发送权，send 继续执行；
// 返回 false 表示已直接写出响应（重放/409/400/500），send 终止。
//
// 状态机（行 = (api_key_id, key) 唯一，重放/409 均以调用方维度隔离）：
//
//	无行               → 插入 pending 认领（唯一索引兜底并发首投：冲突则重读再判定）；
//	done               → 重放：原样返回存档的状态码+响应体（附 Idempotent-Replay: true）；
//	pending 且新鲜     → 409 invalid_request（同 key 上次仍在进行中）；
//	pending 且超 5min  → 视为崩溃残留：原子条件 UPDATE 认领，条件失败（并发方
//	                     恰好抢先认领/存档）→ 重读走上方分支。
func (s *Server) idemClaim(c *gin.Context, apiKeyID uint, key string) (uint, bool) {
	// 懒惰 TTL 清理：仅带 key 的请求顺手清一次过期档案（单条 DELETE 走
	// created_at 索引；单表量级说明见 model.IdempotencyRecord）。
	s.idemSweep()
	for attempt := 0; attempt < 4; attempt++ {
		var rec model.IdempotencyRecord
		err := s.db.Where("api_key_id = ? AND key = ?", apiKeyID, key).First(&rec).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			fresh := model.IdempotencyRecord{
				ApiKeyID:  apiKeyID,
				Key:       key,
				Status:    model.IdemStatusPending,
				CreatedAt: time.Now(),
			}
			createErr := s.db.Create(&fresh).Error
			if createErr == nil {
				return fresh.ID, true // 首次：获得发送权（ID 由 Create 回填）
			}
			if errors.Is(createErr, gorm.ErrDuplicatedKey) {
				continue // 并发首投竞态：对方已建行，重读走重放/409 分支
			}
			serverError(c, createErr)
			return 0, false
		}
		if err != nil {
			serverError(c, err)
			return 0, false
		}
		if rec.Status == model.IdemStatusDone {
			s.idemReplay(c, &rec) // 重放：不重发
			return 0, false
		}
		// pending：新鲜 = 有并发方在发送中；超期 = 崩溃残留，原子条件认领
		if time.Since(rec.CreatedAt) <= idemPendingTTL {
			writeErr(c, http.StatusConflict, TypeInvalidRequest, idemConflictMsg)
			return 0, false
		}
		res := s.db.Model(&model.IdempotencyRecord{}).
			Where("id = ? AND status = ? AND created_at < ?", rec.ID,
				model.IdemStatusPending, time.Now().Add(-idemPendingTTL)).
			Update("created_at", time.Now())
		if res.Error != nil {
			serverError(c, res.Error)
			return 0, false
		}
		if res.RowsAffected == 1 {
			return rec.ID, true // 抢占成功：本请求接管重发
		}
		// 条件失败：并发方抢先认领或已存档 → 重读再判定（有限次即可收敛）
	}
	// 极端竞态下循环耗尽：保守 409（行仍由并发方管辖，语义与分支 3 一致）
	writeErr(c, http.StatusConflict, TypeInvalidRequest, idemConflictMsg)
	return 0, false
}

// idemReplay 重放存档响应：状态码与响应体逐字节一致（存档即捕获的原始字节），
// Content-Type 与本 handler 全部出口统一的 application/json; charset=utf-8 相同。
func (s *Server) idemReplay(c *gin.Context, rec *model.IdempotencyRecord) {
	c.Header(idemReplayHeader, "true")
	c.Data(rec.ResponseStatus, "application/json; charset=utf-8", []byte(rec.ResponseBody))
}

// idemSweep 清理超过存档 TTL 的幂等档案（懒惰触发，不依赖 scheduler；调用方
// 带同 key 重试超期档案即视为无行重新发送）。失败仅记日志：过期档案多留一轮，
// 不阻断本次发送。
func (s *Server) idemSweep() {
	if err := s.db.Where("created_at < ?", time.Now().Add(-idemArchiveTTL)).
		Delete(&model.IdempotencyRecord{}).Error; err != nil {
		log.Printf("api: idempotency sweep failed: %v", err)
	}
}

// captureWriter gin 响应捕获包装：拦截 Write/WriteString 收集响应体字节，
// 状态码直接复用被包装 gin.ResponseWriter 自带的跟踪（WriteHeader 委托透传）。
//
// 方案取舍：相比「构造响应后先存档再写出」，包装器在 send 的所有响应出口
// （成功 200、参数校验 400、rotator 429/502/500、bodyLimit 400）统一生效，
// 响应构造代码零改动，gin 侵入最小（仅替换一次 c.Writer）。存档在响应写出
// 之后执行，存在极小窗口——同 key 请求恰好落在「已写出、未存档」间隙会先得
// 409 而非重放；相比为消除窗口而阻塞响应写出，对延迟零影响，属可接受取舍。
type captureWriter struct {
	gin.ResponseWriter
	buf bytes.Buffer
}

func (w *captureWriter) Write(b []byte) (int, error) {
	w.buf.Write(b) // 捕获为尽力而为，透传结果以真实写出为准
	return w.ResponseWriter.Write(b)
}

func (w *captureWriter) WriteString(s string) (int, error) {
	w.buf.WriteString(s)
	return w.ResponseWriter.WriteString(s)
}

// idemArchive 发送结束后存档「响应状态码+响应体+message_id」并置 done。
// 失败响应同样存档重放（400/429/502 幂等）：调用方带同 key 重试拿到同一错误
// 而非触发二次发送——首投失败、重试反而成功会造成重复发送歧义（契约明示取舍）。
//
// 条件 UPDATE（status=pending）：行被并发扰动（理论路径）则放弃存档，以行内
// 现状为准。存档失败仅记日志，不影响本次已写出的响应：下次同 key 到达要么
// （<5min）409、要么（超期）按僵尸回收重新认领重发，均为安全出路。
func (s *Server) idemArchive(recordID uint, status int, body []byte, messageID string) {
	err := s.db.Model(&model.IdempotencyRecord{}).
		Where("id = ? AND status = ?", recordID, model.IdemStatusPending).
		Updates(map[string]any{
			"status":          model.IdemStatusDone,
			"response_status": status,
			"response_body":   string(body),
			"message_id":      messageID,
		}).Error
	if err != nil {
		log.Printf("api: idempotency archive failed (record_id=%d): %v", recordID, err)
	}
}
