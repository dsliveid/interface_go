// 接口工具 —— 轻量本地 HTTP 接口管理与测试工具。
//
// 启动后在 127.0.0.1 上运行本地服务，并用 Edge/Chrome 的 App 模式
// 拉起一个无地址栏的独立窗口；所有数据保存在 exe 同级的 data 目录。
package main

import (
	"context"
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
	"sync"
	"syscall"
	"time"

	"interfacego/internal/api"
	"interfacego/internal/store"
)

// version 由构建脚本注入，默认开发版本。
var version = "1.0.0"

// appName 用于托盘提示、原生弹窗标题与单实例互斥量名称。
const appName = "接口工具"

// singleInstanceMutex 单实例互斥量名称（同一登录会话内唯一）。
const singleInstanceMutex = "InterfaceGo-SingleInstance-{6E4C1F2A-2C6B-4B7E-9C39-2D9A0F2E1B77}"

var (
	shutdownOnce sync.Once
	browserMu    sync.Mutex
	browserPID   int // 由本进程拉起的浏览器主进程 PID（用于退出时清理整棵进程树）
)

func main() {
	port := flag.Int("port", 0, "监听端口，0 表示自动选择空闲端口")
	noBrowser := flag.Bool("no-browser", false, "只启动服务，不自动打开窗口")
	noTray := flag.Bool("no-tray", false, "不创建系统托盘图标（调试用）")
	dataFlag := flag.String("data", "", "数据目录，默认为程序同级的 data 目录")
	showVersion := flag.Bool("version", false, "打印版本号")
	flag.Parse()

	if *showVersion {
		fmt.Println(appName, version)
		return
	}

	dir, err := resolveDataDir(*dataFlag)
	if err != nil {
		fatal("无法初始化数据目录", err)
		return
	}
	initLog(dir)
	log.Printf("版本 %s，数据目录 %s", version, dir)

	// 桌面形态（默认）：系统托盘 + 单实例互斥。
	// -no-tray 退化为纯 HTTP 服务（无托盘、无互斥，便于自动化测试）；
	// -no-browser 只影响是否自动弹出浏览器窗口，托盘与互斥仍然生效。
	desktopMode := !*noTray

	if desktopMode {
		already, release := acquireSingleInstance(singleInstanceMutex)
		if already {
			release()
			handleExistingInstance(dir)
			return
		}
		// 注意：非重复实例时互斥量句柄必须一直保持打开到进程结束（由系统回收），
		// 一旦提前关闭，单实例保护立即失效。这里刻意不调用 release()。
	}

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

	activity := &api.Activity{}
	openWindow := func() { openMainWindow(url) }
	var srv *http.Server
	srv = &http.Server{Handler: api.New(st, sub, api.Options{
		Activity:     activity,
		OnOpenWindow: openWindow,
		OnQuit:       func() { shutdown(srv, dir, "收到退出请求") },
	})}
	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && serveErr != http.ErrServerClosed {
			log.Printf("服务异常退出：%v", serveErr)
		}
	}()

	// 记录端口，供下一个实例找到本实例（下次双击时直接复用，不再起新进程）
	writePortFile(dir, url)

	if !*noBrowser {
		openMainWindow(url)
	}

	if desktopMode {
		if trayErr := startTray(trayOptions{
			tip:     appName + " · 运行中（双击打开主界面）",
			dataDir: dir,
			onOpen:  openWindow,
			onQuit:  func() { shutdown(srv, dir, "托盘退出") },
		}); trayErr != nil {
			log.Printf("托盘初始化失败：%v（程序仍会继续运行，可通过任务管理器结束）", trayErr)
		} else {
			log.Println("托盘图标已就绪：双击打开主界面，右键可退出")
		}
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	shutdown(srv, dir, "收到退出信号")
}

// shutdown 统一收尾：移除托盘图标 → 结束本进程拉起的浏览器进程树 → 关服务 → 清理端口文件。
// 由托盘「退出」、系统信号触发，sync.Once 保证只执行一次。
func shutdown(srv *http.Server, dir, reason string) {
	shutdownOnce.Do(func() {
		log.Printf("正在退出（%s）…", reason)
		removeTray()

		browserMu.Lock()
		pid := browserPID
		browserMu.Unlock()
		if pid > 0 {
			// 关掉 Job Object 句柄 → 内核按 KILL_ON_JOB_CLOSE 回收整棵浏览器进程树；
			// Job 未建立成功时退化为结束浏览器主进程。
			terminateBrowserTree(pid)
			time.Sleep(600 * time.Millisecond)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)

		_ = os.Remove(portFilePath(dir))
		log.Println("已退出")
		os.Exit(0)
	})
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

// findBrowserExecutable 在常见安装路径中查找 Edge / Chrome 主程序。
func findBrowserExecutable() (string, error) {
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
		if _, err := os.Stat(exe); err == nil {
			return exe, nil
		}
	}
	return "", fmt.Errorf("未找到可用的 Edge 或 Chrome")
}

// browserProfileDir 返回本程序独占的浏览器配置目录。
//
// 使用独立的 user-data-dir 有两个关键好处：
//  1. 不会把命令转交给系统里已在运行的浏览器主进程，我们拉起的进程就是真正的
//     浏览器进程，从而能被绑到 Job Object 上、在退出时被完整回收；
//  2. 关掉窗口后该浏览器实例（含全部子进程）会自行退出，不留后台残留。
//
// 目录固定在 %LOCALAPPDATA%\InterfaceGo 下（而非系统临时目录）：
// 「往临时目录释放内容」是恶意程序的典型行为特征，规范路径能避免这类画像。
func browserProfileDir() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "InterfaceGo", "browser-profile")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("创建浏览器配置目录失败（%v），改用临时目录", err)
		return filepath.Join(os.TempDir(), "interface-go-browser")
	}
	return dir
}

// openMainWindow 拉起主界面窗口；失败时退化为系统默认浏览器打开同一地址。
func openMainWindow(url string) {
	browserMu.Lock()
	defer browserMu.Unlock()

	exe, err := findBrowserExecutable()
	if err != nil {
		log.Printf("未能找到 Edge/Chrome（%v），改用默认浏览器", err)
		openDefaultBrowser(url)
		return
	}
	args := []string{
		"--app=" + url,
		"--user-data-dir=" + browserProfileDir(),
		"--no-first-run",
		"--no-default-browser-check",
		"--window-size=1500,960",
	}
	cmd := exec.Command(exe, args...)
	if err := cmd.Start(); err != nil {
		log.Printf("未能拉起浏览器窗口（%v），改用默认浏览器", err)
		openDefaultBrowser(url)
		return
	}
	browserPID = cmd.Process.Pid
	if jobErr := attachJob(browserPID); jobErr != nil {
		// 只影响「强杀本程序」时的兜底清理，不影响正常退出路径
		log.Printf("未能把浏览器进程绑定到 Job Object（%v）", jobErr)
	}
	go func() { _ = cmd.Wait() }()
	log.Println("已打开主界面窗口")
}

// portFilePath 记录当前实例服务地址，供下一个实例复用（避免多开后端进程）。
func portFilePath(dir string) string {
	return filepath.Join(dir, "port.txt")
}

func writePortFile(dir, url string) {
	if err := os.WriteFile(portFilePath(dir), []byte(url), 0o644); err != nil {
		log.Printf("写入端口信息失败：%v", err)
	}
}

func readPortFile(dir string) string {
	b, err := os.ReadFile(portFilePath(dir))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// probeInstance 判断某个地址上是否有存活的程序实例。
func probeInstance(url string) bool {
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	res, err := client.Get(url + "/api/health")
	if err != nil {
		return false
	}
	defer res.Body.Close()
	return res.StatusCode == http.StatusOK
}

// requestOpenWindow 请求已有实例把主界面窗口拉起来。
func requestOpenWindow(url string) bool {
	client := &http.Client{Timeout: 3 * time.Second}
	res, err := client.Post(url+"/api/open-window", "application/json", nil)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	return res.StatusCode == http.StatusOK
}

// requestQuitInstance 请求已有实例干净退出程序。
func requestQuitInstance(url string) bool {
	client := &http.Client{Timeout: 3 * time.Second}
	res, err := client.Post(url+"/api/quit", "application/json", nil)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	return res.StatusCode == http.StatusOK
}

// findExistingInstance 在已有实例刚启动、尚未写入端口文件时做短暂重试，
// 返回可用的服务地址；全部失败返回空串。
func findExistingInstance(dir string, attempts int) string {
	for i := 0; i < attempts; i++ {
		if url := readPortFile(dir); url != "" && probeInstance(url) {
			return url
		}
		time.Sleep(400 * time.Millisecond)
	}
	return ""
}

// handleExistingInstance 在检测到已有实例时接管本次启动的用户意图：
//
//	是(Y) → 打开主界面窗口
//	否(N) → 退出正在运行的那个实例
//
// 这条通道不依赖系统托盘图标，即使托盘在个别系统上不可见，
// 用户也始终能通过「再次双击程序」可靠地结束程序。
func handleExistingInstance(dir string) {
	url := findExistingInstance(dir, 5)
	if url == "" {
		log.Println("已有实例在运行，但暂时无法与之通信，本次启动退出")
		// 原生弹窗会阻塞等待点击；加兜底定时器，确保无人值守时本进程也不残留
		go func() {
			time.Sleep(8 * time.Second)
			os.Exit(0)
		}()
		messageBox(appName, appName+"已在运行，但暂时无法与之通信。\n请稍后重试，或用任务管理器结束残留的 "+appName+" 进程。")
		return
	}
	if confirmBox(appName, appName+"已在后台运行。\n\n是(Y)：打开主界面窗口\n否(N)：退出正在运行的程序") {
		if !requestOpenWindow(url) {
			openDefaultBrowser(url)
		}
		log.Printf("已有实例在运行（%s），已按用户选择打开主界面，本次启动退出", url)
		return
	}
	if requestQuitInstance(url) {
		log.Printf("已有实例在运行（%s），已按用户选择请求其退出", url)
		return
	}
	log.Println("请求退出已有实例失败，本次启动退出")
}

// openDefaultBrowser 用系统默认浏览器打开地址。
// 只走 ShellExecute：额外的 rundll32 兜底既是多余路径，也会在二进制里
// 留下高度可疑的命令行字符串，收益远小于代价。
func openDefaultBrowser(url string) {
	if err := shellOpen(url); err != nil {
		log.Printf("打开默认浏览器失败：%v", err)
	}
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
