//go:build !unix

// disk_free_other.go 不支持磁盘剩余空间检查的平台（Windows 等）：
// 预检跳过（见 diskFreeCheck），不阻断更新。
package update

import "errors"

var errDiskUnsupported = errors.New("当前平台不支持磁盘空间检查")

func diskFreeBytes(path string) (uint64, error) {
	return 0, errDiskUnsupported
}
