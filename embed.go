package main

import "embed"

// webFS 内嵌前端静态资源，随 exe 一起分发，无需任何外部文件。
//
//go:embed all:web
var webFS embed.FS
