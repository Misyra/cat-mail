//go:build unix

// disk_free_unix.go 磁盘剩余空间（unix 系列：linux/darwin/bsd），
// 与 internal/doctor 的同名检查同口径（Statfs 取 f_bavail）。
package update

import (
	"errors"
	"syscall"
)

// errDiskUnsupported 占位统一错误（unix 平台恒支持，仅对齐 other 文件签名）。
var errDiskUnsupported = errors.New("当前平台不支持磁盘空间检查")

// diskFreeBytes 返回 path 所在文件系统的剩余可用空间（普通用户可写入的字节数）。
func diskFreeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}
