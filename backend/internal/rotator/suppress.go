// suppress.go 收件人抑制列表拦截（送达链路三件套）：发送主链路的统一 hook 点。
// /v1/send、/emails、SMTP 入口（含日志重发）全部经 rotator.Send 出站，故拦截
// 收敛在 Send 入口单点：To+Cc+Bcc 任一命中 suppressions → 400 invalid_request
// （错误码 recipient_suppressed），被拒请求不进入轮换、不预留额度、不落发送日志
// ——与「参数类 400 拒绝在门外」口径一致。
package rotator

import (
	"fmt"
	"log"
	"strings"

	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/provider"
)

// CodeRecipientSuppressed 统一错误体的机器可读错误码（error.code 字段，随
// invalid_request 类型一并返回；SPEC §2 错误体的加法扩展，不影响既有调用方）。
const CodeRecipientSuppressed = "recipient_suppressed"

// suppressedPreviewMax 错误信息里最多列出的命中收件人数（其余以计数注记）。
const suppressedPreviewMax = 5

// checkSuppressions 抑制拦截（Send 唯一调用点，见文件头注释）：
//   - 收件人全未命中 → nil 放行（一次 IN 查询，命中 suppressions.email 唯一索引，
//     上限 100 收件人，量级有界）；
//   - 命中 → 400 invalid_request（code=recipient_suppressed），message 明确列出
//     命中的收件人（错误信息可见性是任务书硬要求）；
//   - 查询失败 fail-closed 视为命中拒绝？不——抑制是防护机制而非发送必要条件，
//     读库故障按放行处理（宁可放一封被抑制的信，不可让 DB 抖动打死全部发送），
//     记服务端日志（与 rotator 其余「记账失败不阻断」取舍一致）。
func checkSuppressions(db *gorm.DB, msg *provider.Message) *Err {
	if msg == nil {
		return nil
	}
	addrs := make([]string, 0, len(msg.To)+len(msg.Cc)+len(msg.Bcc))
	addrs = append(addrs, msg.To...)
	addrs = append(addrs, msg.Cc...)
	addrs = append(addrs, msg.Bcc...)
	seen := make(map[string]bool, len(addrs))
	uniq := make([]string, 0, len(addrs))
	for _, a := range addrs {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		uniq = append(uniq, a)
	}
	if len(uniq) == 0 {
		return nil
	}
	var hits []model.Suppression
	if err := db.Where("email IN ?", uniq).Find(&hits).Error; err != nil {
		// fail-open：读库故障放行（见函数注释），仅记日志
		log.Printf("rotator: suppression lookup failed (fail-open): %v", err)
		return nil
	}
	if len(hits) == 0 {
		return nil
	}
	preview := make([]string, 0, len(hits))
	for i, s := range hits {
		if i >= suppressedPreviewMax {
			break
		}
		reason := s.Reason
		if reason == "" {
			reason = "unknown"
		}
		preview = append(preview, fmt.Sprintf("%s（%s）", s.Email, reason))
	}
	listing := strings.Join(preview, ", ")
	if len(hits) > suppressedPreviewMax {
		listing += fmt.Sprintf(" 等 %d 个", len(hits))
	}
	return &Err{
		HTTPStatus: 400,
		Type:       ErrInvalidRequest,
		Code:       CodeRecipientSuppressed,
		Message:    fmt.Sprintf("收件人已被抑制列表拦截：%s", listing),
	}
}
