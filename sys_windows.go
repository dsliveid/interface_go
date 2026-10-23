//go:build windows

package main

import (
	"encoding/binary"
	"fmt"
	"syscall"
	"unsafe"
)

// 本文件封装程序用到的全部 Windows 原生能力：单实例互斥量、Job Object
// （保证本进程消失时它拉起的浏览器进程树被系统一并回收）、ShellExecute 打开
// 路径/URL、以及从内嵌 ICO 内存创建托盘图标句柄。不依赖任何第三方库。

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")

	procCreateMutexW             = kernel32.NewProc("CreateMutexW")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procOpenProcess              = kernel32.NewProc("OpenProcess")
	procTerminateProcess         = kernel32.NewProc("TerminateProcess")
	procCloseHandle              = kernel32.NewProc("CloseHandle")
	procCreateIconFromResourceEx = user32.NewProc("CreateIconFromResourceEx")
	procShellExecuteW            = shell32.NewProc("ShellExecuteW")
)

const (
	errAlreadyExists = 183 // ERROR_ALREADY_EXISTS

	processSetQuota         = 0x0100
	processTerminate        = 0x0001
	processQueryInformation = 0x0400

	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x2000

	swShowNormal = 1
)

// jobHandle 保持 Job Object 句柄存活到进程结束：
// 句柄一旦关闭（含进程被杀），系统就会终止 Job 内的所有进程。
var jobHandle uintptr

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobExtendedLimitInformation struct {
	BasicLimitInformation jobBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// acquireSingleInstance 用命名互斥量实现单实例。
// 返回 alreadyExists=true 表示已有实例在运行（此时不应再启动服务和托盘）。
func acquireSingleInstance(name string) (alreadyExists bool, release func()) {
	noop := func() {}
	n, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return false, noop
	}
	h, _, callErr := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(n)))
	if h == 0 {
		// 拿不到互斥量（权限等原因）时按「无占用」处理，避免程序无法启动
		return false, noop
	}
	if errno, isErrno := callErr.(syscall.Errno); isErrno && errno == errAlreadyExists {
		procCloseHandle.Call(h)
		return true, noop
	}
	return false, func() { procCloseHandle.Call(h) }
}

// attachJob 把进程绑定到「本进程退出即杀掉」的 Job Object。
// 失败只影响强杀时的兜底清理，不影响正常运行，因此错误由调用方记日志即可。
func attachJob(pid int) error {
	if jobHandle == 0 {
		h, _, callErr := procCreateJobObjectW.Call(0, 0)
		if h == 0 {
			return fmt.Errorf("CreateJobObject 失败：%v", callErr)
		}
		var info jobExtendedLimitInformation
		info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
		ret, _, callErr := procSetInformationJobObject.Call(
			h, jobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
		if ret == 0 {
			procCloseHandle.Call(h)
			return fmt.Errorf("SetInformationJobObject 失败：%v", callErr)
		}
		jobHandle = h
	}

	ph, _, callErr := procOpenProcess.Call(
		processSetQuota|processTerminate|processQueryInformation, 0, uintptr(pid))
	if ph == 0 {
		return fmt.Errorf("OpenProcess(%d) 失败：%v", pid, callErr)
	}
	defer procCloseHandle.Call(ph)

	ret, _, callErr := procAssignProcessToJobObject.Call(jobHandle, ph)
	if ret == 0 {
		return fmt.Errorf("AssignProcessToJobObject 失败：%v", callErr)
	}
	return nil
}

// terminateBrowserTree 结束本程序拉起的整棵浏览器进程树。
//
// 实现说明（为什么不再调用 taskkill）：关闭 Job Object 句柄即可触发
// KILL_ON_JOB_CLOSE，由内核一次性回收 Job 内的全部进程（含渲染、GPU 等子进程），
// 既不依赖外部命令，也不会在二进制里留下「结束进程树」这类高度可疑的字符串特征。
func terminateBrowserTree(pid int) {
	if jobHandle != 0 {
		procCloseHandle.Call(jobHandle)
		jobHandle = 0
		return
	}
	// 兜底：Job 未能建立时（权限不足等），只结束主进程本身
	if pid <= 0 {
		return
	}
	ph, _, _ := procOpenProcess.Call(processTerminate, 0, uintptr(pid))
	if ph == 0 {
		return
	}
	defer procCloseHandle.Call(ph)
	procTerminateProcess.Call(ph, 0)
}

// shellOpen 用系统默认方式打开文件、目录或 URL。
func shellOpen(target string) error {
	verb, err := syscall.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	file, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	ret, _, _ := procShellExecuteW.Call(0,
		uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(file)), 0, 0, swShowNormal)
	// ShellExecuteW 返回值 > 32 才算成功
	if ret <= 32 {
		return fmt.Errorf("ShellExecute 打开 %s 失败（返回 %d）", target, ret)
	}
	return nil
}

// iconFromICO 从内嵌 ICO 数据中挑选最接近 32×32 的 32bpp 条目，创建 HICON。
// 全部在内存中完成，无需把 .ico 落盘，也不依赖 exe 资源段。
func iconFromICO(ico []byte) uintptr {
	best, bestSize := -1, 0
	if len(ico) >= 6 {
		count := int(binary.LittleEndian.Uint16(ico[4:]))
		for i := 0; i < count; i++ {
			off := 6 + 16*i
			if off+16 > len(ico) {
				break
			}
			w := int(ico[off])
			if w == 0 {
				w = 256
			}
			bits := int(binary.LittleEndian.Uint16(ico[off+6:]))
			size := int(binary.LittleEndian.Uint32(ico[off+8:]))
			imgOff := int(binary.LittleEndian.Uint32(ico[off+12:]))
			// 只取 32bpp 位图条目：PNG 压缩条目部分系统 API 支持不佳
			if bits != 32 || imgOff+size > len(ico) || imgOff < 0 || size <= 0 {
				continue
			}
			if best < 0 || absInt(w-32) < absInt(bestSize-32) {
				best, bestSize = imgOff, w
			}
		}
	}
	if best < 0 {
		return 0
	}
	dataLen := icoEntryLength(ico, best)
	if dataLen <= 0 || best+dataLen > len(ico) {
		return 0
	}
	data := ico[best : best+dataLen]
	h, _, _ := procCreateIconFromResourceEx.Call(
		uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)),
		1,        // fIcon = TRUE
		0x00030000, // dwVer = 3.0
		0, 0,     // 使用图像自身尺寸
		0,        // LR_DEFAULTCOLOR
	)
	return h
}

// icoEntryLength 根据图像数据偏移反查该条目的字节长度。
func icoEntryLength(ico []byte, imgOff int) int {
	if len(ico) < 6 {
		return 0
	}
	count := int(binary.LittleEndian.Uint16(ico[4:]))
	for i := 0; i < count; i++ {
		off := 6 + 16*i
		if off+16 > len(ico) {
			return 0
		}
		io := int(binary.LittleEndian.Uint32(ico[off+12:]))
		if io == imgOff {
			return int(binary.LittleEndian.Uint32(ico[off+8:]))
		}
	}
	return 0
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
