// Package version 提供构建期注入的版本元数据。
//
// 构建时通过 -ldflags 注入，例如：
//
//	go build -ldflags "-X upanel/internal/version.Version=0.1.0 \
//	  -X upanel/internal/version.Commit=$(git rev-parse --short HEAD) \
//	  -X upanel/internal/version.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
//	  -o bin/upanel-web ./cmd/upanel-web
//
// 未注入时保留明确的开发占位值，不伪装成正式发布版本。
package version

import (
	"fmt"
	"runtime"
)

var (
	// Version 为语义化版本号；未注入时为开发占位值。
	Version = "0.0.0-dev"
	// Commit 为构建所用的提交短哈希；未知时为 "unknown"。
	Commit = "unknown"
	// BuildDate 为 UTC RFC3339 构建时间；未知时为 "unknown"。
	BuildDate = "unknown"
)

// Info 是版本信息的只读视图，字段名为 snake_case 以直接进入 JSON 响应。
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	GoVersion string `json:"go_version"`
}

// Get 返回当前二进制的版本信息。
func Get() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		BuildDate: BuildDate,
		GoVersion: runtime.Version(),
	}
}

// String 返回适合日志与 CLI 输出的一行描述。
func (i Info) String() string {
	return fmt.Sprintf("%s (commit %s, built %s, %s)", i.Version, i.Commit, i.BuildDate, i.GoVersion)
}
