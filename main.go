// 接口工具 —— 轻量本地 HTTP 接口管理与测试工具。
//
// 启动后在 127.0.0.1 上运行本地服务，并用 Edge/Chrome 的 App 模式
// 拉起一个无地址栏的独立窗口；所有数据保存在 exe 同级的 data 目录。
package main

import (
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"interfacego/internal/api"
	"interfacego/internal/store"
)

// version 由构建脚本注入，默认开发版本。
var version = "1.0.0"

func main() {
	port := flag.Int("port", 0, "监听端口，0 表示自动选择空闲端口")
	noBrowser := flag.Bool("no-browser", false, "只启动服务，不自动打开窗口")
	dataFlag := flag.String("data", "", "数据目录，默认为程序同级的 data 目录")
	showVersion := flag.Bool("version", false, "打印版本号")
	flag.Parse()

	if *showVersion {
		fmt.Println("接口工具", version)
		return
	}

	dir, err := resolveDataDir(*dataFlag)
	if err != nil {
		fatal("无法初始化数据目录", err)
		return
	}
	initLog(dir)
	log.Printf("版本 %s，数据目录 %s", version, dir)

	st, err := store.Open(dir)
	if err != nil {
		fatal("数据加载失败", err)
		return
	}

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		fatal("前端资源加载失败", err)
		return
	}

	addr := "127.0.0.1:" + strconv.Itoa(*port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// 端口被占用时退回随机端口
		log.Printf("端口 %d 不可用（%v），改用随机端口", *port, err)
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fatal("本地服务启动失败", err)
			return
		}
	}
	url := "http://127.0.0.1:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	log.Printf("本地服务已启动：%s", url)

	srv := &http.Server{Handler: api.New(st, sub)}
	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && serveErr != http.ErrServerClosed {
			log.Printf("服务异常退出：%v", serveErr)
		}
	}()

	if !*noBrowser {
		if openErr := openAppWindow(url); openErr != nil {
			log.Printf("未能拉起 Edge/Chrome 独立窗口（%v），改用默认浏览器", openErr)
			openDefaultBrowser(url)
		}
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	log.Println("正在退出…")
}

// resolveDataDir 定位数据目录：优先程序同级 data，不可写时回退到用户目录。
func resolveDataDir(explicit string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return "", err
		}
		return abs, ensureWritable(abs)
	}

	base := ""
	if exe, err := os.Executable(); err == nil {
		if resolved, linkErr := filepath.EvalSymlinks(exe); linkErr == nil {
			exe = resolved
		}
		base = filepath.Dir(exe)
	}
	// go run / 测试环境下 exe 位于临时目录，此时以当前工作目录为准
	if isTempPath(base) {
		if wd, err := os.Getwd(); err == nil {
			base = wd
		}
	}
	if base == "" {
		if wd, err := os.Getwd(); err == nil {
			base = wd
		}
	}

	preferred := filepath.Join(base, "data")
	if err := ensureWritable(preferred); err == nil {
		return preferred, nil
	}
	fallback := filepath.Join(os.Getenv("LOCALAPPDATA"), "InterfaceGo", "data")
	if os.Getenv("LOCALAPPDATA") == "" {
		return "", fmt.Errorf("目录 %s 不可写，且无法确定用户数据目录", preferred)
	}
	if err := ensureWritable(fallback); err != nil {
		return "", err
	}
	log.Printf("程序所在目录不可写，数据改存放于 %s", fallback)
	return fallback, nil
}

func isTempPath(p string) bool {
	lower := strings.ToLower(p)
	return strings.Contains(lower, "go-build") ||
		strings.Contains(lower, `\temp`) ||
		strings.Contains(lower, "/tmp/") ||
		strings.HasSuffix(lower, "/tmp")
}

func ensureWritable(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "backup"), 0o755); err != nil {
		return err
	}
	probe := filepath.Join(dir, ".write-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		return err
	}
	return os.Remove(probe)
}

func initLog(dir string) {
	logFile := filepath.Join(dir, "app.log")
	if fi, err := os.Stat(logFile); err == nil && fi.Size() > 5<<20 {
		_ = os.Remove(logFile)
	}
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.SetFlags(log.LstdFlags)
		return
	}
	log.SetOutput(io.MultiWriter(os.Stdout, f))
	log.SetFlags(log.LstdFlags)
	log.SetPrefix("")
}

// openAppWindow 用 Edge/Chrome 的 App 模式打开，得到无地址栏的独立窗口。
func openAppWindow(url string) error {
	var candidates []string
	add := func(parts ...string) {
		if p := filepath.Join(parts...); p != "" {
			candidates = append(candidates, p)
		}
	}
	pf := os.Getenv("ProgramFiles")
	pf86 := os.Getenv("ProgramFiles(x86)")
	lad := os.Getenv("LocalAppData")
	for _, root := range []string{pf86, pf, lad} {
		if root == "" {
			continue
		}
		add(root, "Microsoft", "Edge", "Application", "msedge.exe")
	}
	for _, root := range []string{pf, pf86, lad} {
		if root == "" {
			continue
		}
		add(root, "Google", "Chrome", "Application", "chrome.exe")
	}
	for _, exe := range candidates {
		if _, err := os.Stat(exe); err != nil {
			continue
		}
		cmd := exec.Command(exe, "--app="+url, "--window-size=1500,960")
		if err := cmd.Start(); err == nil {
			return nil
		}
	}
	return fmt.Errorf("未找到可用的 Edge 或 Chrome")
}

func openDefaultBrowser(url string) {
	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

func fatal(title string, err error) {
	msg := title
	if err != nil {
		msg += "：" + err.Error()
	}
	log.Println(msg)
	messageBox("接口工具", msg)
	os.Exit(1)
}
