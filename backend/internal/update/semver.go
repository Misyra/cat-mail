// semver.go 受限 semver 比较（E8，手写零依赖）：go.mod 明确不新增依赖，
// golang.org/x/mod/semver 不可用，且本项目版本即 vX.Y.Z 纯数字三段形态，
// 自写百行内的比较器足够并保持「解析失败即不可比」的诚实语义。
package update

import (
	"errors"
	"fmt"
	"strings"
)

// errIncomparable 版本号解析失败（非 vX.Y.Z 纯数字段）：调用方应视为
// 「不可比较」并如实呈现，绝不猜测升级方向。
var errIncomparable = errors.New("不是可比较的版本号（须为 vX.Y.Z 纯数字段）")

// Compare 比较两个版本号：返回 a>b → 正数、a<b → 负数、相等 → 0。
// 规则（与发布约定一致）：
//   - 容忍可选的 v/V 前缀（v1.2.3 与 1.2.3 等价）；
//   - 按 "." 分段逐段十进制数字比较，位数不齐右侧补 0（1.2 == 1.2.0）；
//   - 任一段含非数字字符（如 1.2.3-rc1、1.2.beta、空串）→ errIncomparable，
//     不支持预发布后缀：发布流水线约定 tag 一律为纯 vX.Y.Z，出现其他形态
//     说明版本不规范，宁可放弃比较也不给出错误方向。
func Compare(a, b string) (int, error) {
	as, err := parseSegments(a)
	if err != nil {
		return 0, fmt.Errorf("版本 %q: %w", a, err)
	}
	bs, err := parseSegments(b)
	if err != nil {
		return 0, fmt.Errorf("版本 %q: %w", b, err)
	}
	// 位数不齐补 0：右侧短的按 0 比较（1.2 vs 1.2.0 → 相等）。
	n := max(len(as), len(bs))
	for i := 0; i < n; i++ {
		av, bv := 0, 0
		if i < len(as) {
			av = as[i]
		}
		if i < len(bs) {
			bv = bs[i]
		}
		switch {
		case av > bv:
			return 1, nil
		case av < bv:
			return -1, nil
		}
	}
	return 0, nil
}

// parseSegments 解析为数字段：去 v/V 前缀 → 按 "." 切分 → 逐段十进制。
// 段值超 int 范围（恶意/异常超长段）同样按解析失败处理，防止溢出误判方向。
func parseSegments(v string) ([]int, error) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V")
	if v == "" {
		return nil, errIncomparable
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			return nil, errIncomparable // "1..2" / 尾点等畸形
		}
		n := 0
		for _, r := range p {
			if r < '0' || r > '9' {
				return nil, errIncomparable
			}
			if n > (1<<31-1-9)/10 { // 溢出防护：约 21 亿段的合法上限足够版本号使用
				return nil, errIncomparable
			}
			n = n*10 + int(r-'0')
		}
		out = append(out, n)
	}
	return out, nil
}
