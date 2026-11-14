package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestFaviconSameSourceAsExeIcon 锁定「网页图标与 exe 图标同源」这条约束。
//
// 历史问题：任务栏按钮上的图标来自 Chrome App 窗口的 favicon，而不是 exe 的 .syso 资源。
// 早期 favicon 用 SVG <text> 画 ◇ 字符（默认锚点在左边缘、基线对齐），任务栏里明显偏离中心。
// 现在统一由 tools/genicon 生成，两份文件必须字节一致，否则说明有人手动改了其中一份。
func TestFaviconSameSourceAsExeIcon(t *testing.T) {
	exeIcon, err := os.ReadFile("assets/app.ico")
	if err != nil {
		t.Fatalf("读取 assets/app.ico 失败：%v", err)
	}
	webIcon, err := os.ReadFile("web/favicon.ico")
	if err != nil {
		t.Fatalf("读取 web/favicon.ico 失败（应运行 go run ./tools/genicon）：%v", err)
	}
	if !bytes.Equal(exeIcon, webIcon) {
		t.Fatalf("web/favicon.ico 与 assets/app.ico 不一致，请重新运行 go run ./tools/genicon")
	}
}

// TestIndexHTMLUsesIconFiles 确保页面不再用字体字符或内联 SVG 充当图标。
func TestIndexHTMLUsesIconFiles(t *testing.T) {
	html, err := os.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("读取 web/index.html 失败：%v", err)
	}
	page := string(html)

	if strings.Contains(page, "data:image/svg") {
		t.Errorf("web/index.html 仍包含内联 SVG 图标：字形/度量依赖字体，会偏离画布中心，请改用 /favicon.ico 与 /icon-*.png")
	}
	if !strings.Contains(page, `href="/favicon.ico"`) {
		t.Errorf("web/index.html 缺少 <link rel=\"icon\" href=\"/favicon.ico\">")
	}
}

// TestWebIconAssetsExist 校验 favicon 各尺寸位图齐备（任务栏 / 高分屏 / 移动端）。
func TestWebIconAssetsExist(t *testing.T) {
	for _, name := range []string{
		"web/favicon.ico",
		"web/icon-16.png",
		"web/icon-32.png",
		"web/icon-48.png",
		"web/icon-180.png",
	} {
		info, err := os.Stat(name)
		if err != nil {
			t.Errorf("缺少 %s（应运行 go run ./tools/genicon）：%v", name, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("%s 是空文件", name)
		}
	}
}
