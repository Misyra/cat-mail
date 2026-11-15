// semver_test.go 受限 semver 比较的表驱动测试：v 前缀、位数不齐补 0、
// 非数字段不可比、溢出防护。
package update

import (
	"errors"
	"testing"
)

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int  // 期望比较方向；wantErr=true 时忽略
		err  bool // 期望 errIncomparable
	}{
		// 基本方向
		{"1.0.0", "1.0.0", 0, false},
		{"1.0.0", "1.0.1", -1, false},
		{"2.0.0", "1.9.9", 1, false},
		{"1.10.0", "1.9.9", 1, false}, // 数值比较而非字典序
		// v 前缀（大小写、与无前缀互通）
		{"v1.2.3", "1.2.3", 0, false},
		{"V1.2.3", "v1.2.4", -1, false},
		// 位数不齐补 0
		{"1.2", "1.2.0", 0, false},
		{"1.2", "1.2.1", -1, false},
		{"1", "1.0.0", 0, false},
		{"1.0", "0.9.9", 1, false},
		// 多段（约定外但纯数字段也可比）
		{"1.2.3.4", "1.2.3", 1, false},
		// 非数字段 → 不可比
		{"", "1.0.0", 0, true},
		{"1.2.3-rc1", "1.2.3", 0, true},
		{"1.2.beta", "1.2.3", 0, true},
		{"1..2", "1.0.0", 0, true},
		{"1.2.", "1.2.0", 0, true},
		{"latest", "1.0.0", 0, true},
		{"dev", "1.0.0", 0, true},
		{" 1.2.3 ", "1.2.3", 0, false}, // 两侧空白容忍
	}
	for _, c := range cases {
		got, err := Compare(c.a, c.b)
		if c.err {
			if !errors.Is(err, errIncomparable) {
				t.Errorf("Compare(%q,%q) err=%v, want errIncomparable", c.a, c.b, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("Compare(%q,%q) unexpected err: %v", c.a, c.b, err)
			continue
		}
		// 归一化符号
		dir := 0
		switch {
		case got > 0:
			dir = 1
		case got < 0:
			dir = -1
		}
		if dir != c.want {
			t.Errorf("Compare(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// TestCompareOverflow 段值超 int 上限按解析失败处理（防溢出误判方向）。
func TestCompareOverflow(t *testing.T) {
	if _, err := Compare("99999999999999999999.0.0", "1.0.0"); !errors.Is(err, errIncomparable) {
		t.Errorf("超长数字段应判为不可比, got err=%v", err)
	}
}
