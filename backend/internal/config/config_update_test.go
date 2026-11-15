// config_update_test.go E8 更新配置解析：CATMAIL_UPDATE_REPO 校验与两个开关。
package config

import (
	"testing"
)

func TestParseUpdateRepo(t *testing.T) {
	cases := []struct {
		raw    string
		want   string
		wantEr bool
	}{
		{"", "", false},   // 空 = 功能关闭
		{"  ", "", false}, // 空白等同未设置
		{"owner/repo", "owner/repo", false},
		{" catmail-io/catmail ", "catmail-io/catmail", false}, // 两侧空白容忍
		{"user.name/repo.name", "user.name/repo.name", false},
		{"user_x/y-z_1", "user_x/y-z_1", false},
		{"owner/repo/extra", "", true}, // 多段
		{"justname", "", true},         // 缺段
		{"/repo", "", true},            // owner 空
		{"owner/", "", true},           // repo 空
		{".hidden/repo", "", true},     // 以 . 开头（防 "."/".." 拼接穿越）
		{"../repo", "", true},          // 路径穿越形态
		{"own er/repo", "", true},      // 段内空格
		{"owner/re po", "", true},      // 段内空格
		{"owner/repo?", "", true},      // 非法字符
	}
	for _, c := range cases {
		got, err := ParseUpdateRepo(c.raw)
		if c.wantEr {
			if err == nil {
				t.Errorf("ParseUpdateRepo(%q) 应报错, got %q", c.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseUpdateRepo(%q) unexpected err: %v", c.raw, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseUpdateRepo(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}
