//go:build windows

package main

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// 系统托盘：创建一个隐藏的消息窗口，用 Shell_NotifyIcon 在通知区放置图标，
// 双击图标打开主界面，右键弹出菜单（打开主界面 / 打开数据目录 / 退出）。
// 全部通过 stdlib syscall 调用 Win32，没有任何第三方 GUI 依赖。
//
// 生命周期约定：托盘图标从程序启动即常驻，关闭浏览器窗口不会让它消失；
// 只有菜单里的「退出」才真正结束程序。

const (
	wmNull          = 0x0000
	wmDestroy       = 0x0002
	wmClose         = 0x0010
	wmRButtonUp     = 0x0205
	wmLButtonDblClk = 0x0203
	wmApp           = 0x8000

	trayCallbackMessage = wmApp + 1 // 托盘事件回调消息

	nimAdd    = 0x00000000
	nimModify = 0x00000001
	nimDelete = 0x00000002

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004
	nifInfo    = 0x00000010

	niifInfo = 0x00000001 // NIIF_INFO：普通信息气泡

	mfString    = 0x00000000
	mfSeparator = 0x00000800

	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100

	menuOpenWindow  = 1
	menuOpenDataDir = 2
	menuQuit        = 3
)

var (
	procRegisterClassExW       = user32.NewProc("RegisterClassExW")
	procCreateWindowExW        = user32.NewProc("CreateWindowExW")
	procDefWindowProcW         = user32.NewProc("DefWindowProcW")
	procGetMessageW            = user32.NewProc("GetMessageW")
	procTranslateMessage       = user32.NewProc("TranslateMessage")
	procDispatchMessageW       = user32.NewProc("DispatchMessageW")
	procPostQuitMessage        = user32.NewProc("PostQuitMessage")
	procCreatePopupMenu        = user32.NewProc("CreatePopupMenu")
	procAppendMenuW            = user32.NewProc("AppendMenuW")
	procTrackPopupMenu         = user32.NewProc("TrackPopupMenu")
	procDestroyMenu            = user32.NewProc("DestroyMenu")
	procSetForegroundWindow    = user32.NewProc("SetForegroundWindow")
	procGetCursorPos           = user32.NewProc("GetCursorPos")
	procPostMessageW           = user32.NewProc("PostMessageW")
	procRegisterWindowMessageW = user32.NewProc("RegisterWindowMessageW")
	procGetModuleHandleW       = kernel32.NewProc("GetModuleHandleW")

	procShellNotifyIconW = shell32.NewProc("Shell_NotifyIconW")
)

type wndClassExW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type trayPoint struct{ X, Y int32 }

type trayMsg struct {
	HWnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      trayPoint
}

// notifyIconDataW 对应 NOTIFYICONDATAW；cbSize 按 v2 结构上报（到 uVersion 为止）。
type notifyIconDataW struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         [16]byte
	HBalloonIcon     uintptr
}

// trayOptions 托盘回调与展示信息。
type trayOptions struct {
	tip     string
	dataDir string
	onOpen  func() // 打开/聚焦主界面
	onQuit  func() // 退出程序
}

var trayState struct {
	hwnd           uintptr
	icon           uintptr
	tip            string
	dataDir        string
	onOpen         func()
	onQuit         func()
	taskbarCreated uint32
}

var trayWndProcCallback = syscall.NewCallback(trayWndProc)

// startTray 启动托盘并等待初始化完成；返回后图标已出现在通知区。
func startTray(opt trayOptions) error {
	ready := make(chan error, 1)
	go func() {
		// 消息循环必须固定在同一个 OS 线程上
		runtime.LockOSThread()
		trayRun(opt, ready)
	}()
	return <-ready
}

// removeTray 从通知区移除图标（跨线程调用安全），供退出流程使用。
func removeTray() {
	if trayState.hwnd == 0 {
		return
	}
	nid := notifyIconDataW{HWnd: trayState.hwnd, UID: 1}
	nid.CbSize = notifyIconSize()
	procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
	trayState.hwnd = 0
}

func trayRun(opt trayOptions, ready chan<- error) {
	trayState.onOpen = opt.onOpen
	trayState.onQuit = opt.onQuit
	trayState.dataDir = opt.dataDir
	trayState.tip = opt.tip

	hInstance, _, _ := procGetModuleHandleW.Call(0)
	clsName, err := syscall.UTF16PtrFromString("InterfaceGoTrayWindow")
	if err != nil {
		ready <- err
		return
	}
	wc := wndClassExW{
		LpfnWndProc:   trayWndProcCallback,
		HInstance:     hInstance,
		LpszClassName: clsName,
	}
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	if atom, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		ready <- fmt.Errorf("RegisterClassEx 失败：%v", callErr)
		return
	}

	title, err := syscall.UTF16PtrFromString("接口工具")
	if err != nil {
		ready <- err
		return
	}
	// 隐藏窗口：仅用于接收托盘消息，不显示
	hwnd, _, callErr := procCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(clsName)), uintptr(unsafe.Pointer(title)), 0,
		0, 0, 0, 0, 0, 0, hInstance, 0)
	if hwnd == 0 {
		ready <- fmt.Errorf("CreateWindowEx 失败：%v", callErr)
		return
	}
	trayState.hwnd = hwnd

	// explorer.exe 重启后系统广播该消息，收到后需要重新添加图标
	if name, e := syscall.UTF16PtrFromString("TaskbarCreated"); e == nil {
		id, _, _ := procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(name)))
		trayState.taskbarCreated = uint32(id)
	}

	if err := trayAdd(opt.tip); err != nil {
		ready <- err
		return
	}
	trayBalloon(appName, "程序已在后台运行：双击托盘图标可打开界面，右键菜单可退出程序。\n若通知区看不到图标，请点击任务栏右下角的 ^ 展开。")
	ready <- nil

	var m trayMsg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) == -1 || r == 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	// 消息循环结束（窗口被销毁 / explorer 异常）意味着托盘入口已丢失。
	// 此时必须主动退出程序，否则用户将失去唯一可控的退出入口。
	if trayState.onQuit != nil {
		trayState.onQuit()
	}
}

func trayAdd(tip string) error {
	if trayState.icon == 0 {
		trayState.icon = iconFromICO(appIconICO)
	}
	nid := notifyIconDataW{
		HWnd:             trayState.hwnd,
		UID:              1,
		UFlags:           nifMessage | nifIcon | nifTip,
		UCallbackMessage: trayCallbackMessage,
		HIcon:            trayState.icon,
	}
	copyUTF16Fixed(nid.SzTip[:], tip)
	nid.CbSize = notifyIconSize()
	if ret, _, callErr := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&nid))); ret == 0 {
		return fmt.Errorf("Shell_NotifyIcon(NIM_ADD) 失败：%v", callErr)
	}
	return nil
}

// trayBalloon 在通知区弹一次信息气泡。
// 说明：Windows 11 会把新注册的托盘图标折叠进「^ 溢出区」，气泡能让用户
// 立刻意识到程序已在后台运行，并知道去哪里找图标。
func trayBalloon(title, text string) {
	if trayState.hwnd == 0 {
		return
	}
	nid := notifyIconDataW{
		HWnd:        trayState.hwnd,
		UID:         1,
		UFlags:      nifInfo,
		DwInfoFlags: niifInfo,
	}
	copyUTF16Fixed(nid.SzInfoTitle[:], title)
	copyUTF16Fixed(nid.SzInfo[:], text)
	nid.CbSize = notifyIconSize()
	procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
}

// notifyIconSize 返回 NOTIFYICONDATAW v3 的结构长度（到 dwInfoFlags 为止），
// 既支持图标/提示，也支持信息气泡。
func notifyIconSize() uint32 {
	var nid notifyIconDataW
	return uint32(unsafe.Offsetof(nid.DwInfoFlags) + unsafe.Sizeof(nid.DwInfoFlags))
}

func trayWndProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case trayCallbackMessage:
		switch uint32(lParam) & 0xFFFF {
		case wmLButtonDblClk:
			runAsync(trayState.onOpen)
		case wmRButtonUp:
			showTrayMenu(hwnd)
		}
		return 0
	case wmClose:
		removeTray()
		procPostQuitMessage.Call(0)
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	if trayState.taskbarCreated != 0 && msg == trayState.taskbarCreated {
		_ = trayAdd(trayState.tip)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
	return r
}

func showTrayMenu(hwnd uintptr) {
	hmenu, _, _ := procCreatePopupMenu.Call()
	if hmenu == 0 {
		return
	}
	appendMenu(hmenu, menuOpenWindow, "打开主界面")
	appendMenu(hmenu, menuOpenDataDir, "打开数据目录")
	procAppendMenuW.Call(hmenu, mfSeparator, 0, 0)
	appendMenu(hmenu, menuQuit, "退出")

	var pt trayPoint
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// 先把窗口提到前台，否则点击菜单外部时菜单不会消失
	procSetForegroundWindow.Call(hwnd)
	cmd, _, _ := procTrackPopupMenu.Call(hmenu,
		tpmReturnCmd|tpmRightButton, uintptr(pt.X), uintptr(pt.Y), 0, hwnd, 0)
	procDestroyMenu.Call(hmenu)
	procPostMessageW.Call(hwnd, wmNull, 0, 0)

	switch cmd {
	case menuOpenWindow:
		runAsync(trayState.onOpen)
	case menuOpenDataDir:
		if trayState.dataDir != "" {
			_ = shellOpen(trayState.dataDir)
		}
	case menuQuit:
		runAsync(trayState.onQuit)
	}
}

func appendMenu(hmenu, id uintptr, text string) {
	if label, err := syscall.UTF16PtrFromString(text); err == nil {
		procAppendMenuW.Call(hmenu, mfString, id, uintptr(unsafe.Pointer(label)))
	}
}

// runAsync 让回调在独立 goroutine 中执行，避免阻塞消息循环。
func runAsync(fn func()) {
	if fn == nil {
		return
	}
	go fn()
}

// copyUTF16Fixed 把字符串编码进定长 UTF-16 缓冲区，超长自动截断并保证结尾 NUL。
func copyUTF16Fixed(dst []uint16, s string) {
	u := syscall.StringToUTF16(s)
	if len(u) > len(dst) {
		u = u[:len(dst)]
		u[len(u)-1] = 0
	}
	copy(dst, u)
}
