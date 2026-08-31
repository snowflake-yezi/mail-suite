// Package buildinfo 保存构建阶段注入的制品身份。
package buildinfo

var (
	// Version 是面向运维日志和 OCI 元数据的发布版本。
	Version = "dev"
	// Commit 是生成当前制品的源码提交，未注入时保持 unknown。
	Commit = "unknown"
)
