//go:build unix

// disk_free_unix.go 磁盘剩余空间检查的 unix 实现（darwin/linux/bsd 等）。
package doctor

import "syscall"

// diskFreeBytes 返回 path 所在文件系统的剩余可用空间（普通用户可写入的
// Bavail 口径，不含 root 保留块）。
func diskFreeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
