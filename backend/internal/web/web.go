// Package web 通过 go:embed 打包前端 SPA 构建产物（dist）。
package web

import (
	"embed"
	"io/fs"
)

//go:embed dist
var distFS embed.FS

// FS 返回 dist 目录根（SPA 静态资源，含 index.html）。
func FS() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
