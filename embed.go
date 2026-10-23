package main

import "embed"

// webFS 内嵌前端静态资源，随 exe 一起分发，无需任何外部文件。
//
//go:embed all:web
var webFS embed.FS

// appIconICO 内嵌程序图标（深色底 + 绿色菱形，与页面头部品牌标识一致）。
// 托盘图标直接用它在内存中创建 HICON；exe 文件图标由 rsrc 生成的 .syso 提供同一份图案。
//
//go:embed assets/app.ico
var appIconICO []byte
