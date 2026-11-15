//go:build !unix

// disk_free_other.go 非 unix 平台（Windows 等）的磁盘剩余空间检查：不支持，
// 统一返回 errDiskUnsupported，doctor 以 SKIP 呈现并注明平台。
package doctor

// diskFreeBytes 非 unix 平台不支持（Windows 请用资源管理器/wmic 查看）。
func diskFreeBytes(path string) (uint64, error) {
	return 0, errDiskUnsupported
}
