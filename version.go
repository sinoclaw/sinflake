package sinflake

// Version 是库的语义化版本，便于 SDK 使用方在日志/健康检查里打印。
//
// 兼容承诺：v0.x 期间不保证 API 完全稳定，但破坏性改动一定写进 CHANGELOG 并在同一条
// 记录里给出迁移方式；v1.0 之后遵守 semver。
const Version = "0.2.0"

// GoVersion 是本库要求的最低 Go 版本（与 go.mod 一致）。
const GoVersion = "1.21"
