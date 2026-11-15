// Package version 提供程序版本号的单一事实来源（E8 自动更新）。
//
// 默认 "dev"；正式构建经 -ldflags 注入覆盖：
//
//	go build -ldflags "-X catmail/internal/version.Version=vX.Y.Z" ./cmd/catmail
//
// main（doctor 子命令/启动日志）、doctor、internal/update 的更新检查均从这里
// 读取，保证三处口径一致（此前 main.version 仅 doctor 可见，自动更新无法复用）。
package version

// Version 程序版本号：构建时经 -ldflags "-X catmail/internal/version.Version=vX.Y.Z"
// 注入（Dockerfile ARG VERSION / CI tag 构建），未注入显示 dev。
var Version = "dev"
