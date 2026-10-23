// Package api 提供本地 HTTP 接口，供内嵌页面调用。
package api

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"interfacego/internal/executor"
	"interfacego/internal/model"
	"interfacego/internal/store"
)

// maxRequestBytes 限制请求体大小（保存副本时可能带上很大的响应体）。
const maxRequestBytes = 16 << 20

// windowAliveWindow 判定「浏览器窗口仍然打开」的心跳有效期。
// 前端每 3 秒心跳一次，这里留足余量容忍卡顿与后台节流。
const windowAliveWindow = 8 * time.Second

// Activity 记录前端页面最近一次心跳时间，用于判断浏览器窗口是否仍然打开。
//
// 它只服务于展示与「避免重复开窗」判断，绝不用于触发程序退出：
// 退出统一由系统托盘右键菜单控制（见 tray_windows.go）。
type Activity struct {
	last atomic.Int64 // UnixNano；0 表示从未收到心跳
}

// Touch 记录一次页面心跳。
func (a *Activity) Touch() { a.last.Store(time.Now().UnixNano()) }

// Alive 判断最近 within 时间内是否收到过心跳。
func (a *Activity) Alive(within time.Duration) bool {
	last := a.last.Load()
	if last == 0 {
		return false
	}
	return time.Since(time.Unix(0, last)) <= within
}

// Server 承载所有本地接口。
type Server struct {
	st      *store.Store
	static  fs.FS
	act     *Activity
	onOpen  func() // 由托盘侧注入：重新拉起浏览器窗口
	onQuit  func() // 由主流程注入：干净退出程序
}

// Options 注入桌面环境相关的回调，全部可为 nil（无 GUI 环境）。
type Options struct {
	Activity     *Activity  // 前端心跳记录器
	OnOpenWindow func()     // 打开/聚焦主界面窗口
	OnQuit       func()     // 退出程序（供「重复启动时选择退出已有实例」使用）
}

// New 构造路由。
func New(st *store.Store, static fs.FS, opt Options) http.Handler {
	act := opt.Activity
	if act == nil {
		act = &Activity{}
	}
	s := &Server{
		st:     st,
		static: static,
		act:    act,
		onOpen: opt.OnOpenWindow,
		onQuit: opt.OnQuit,
	}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/ping", s.ping)
	mux.HandleFunc("GET /api/window-state", s.windowState)
	mux.HandleFunc("POST /api/open-window", s.openWindow)
	mux.HandleFunc("POST /api/quit", s.quit)
	mux.HandleFunc("GET /api/workspace", s.workspace)
	mux.HandleFunc("POST /api/interfaces", s.createInterface)
	mux.HandleFunc("PUT /api/interfaces/{id}", s.updateInterface)
	mux.HandleFunc("DELETE /api/interfaces/{id}", s.deleteInterface)
	mux.HandleFunc("POST /api/interfaces/{id}/duplicate", s.duplicateInterface)
	mux.HandleFunc("POST /api/interfaces/{id}/snapshots", s.createSnapshot)
	mux.HandleFunc("GET /api/interfaces/{id}/snapshots/{sid}", s.getSnapshot)
	mux.HandleFunc("PUT /api/interfaces/{id}/snapshots/{sid}", s.renameSnapshot)
	mux.HandleFunc("DELETE /api/interfaces/{id}/snapshots/{sid}", s.deleteSnapshot)
	mux.HandleFunc("POST /api/execute", s.execute)
	mux.HandleFunc("GET /api/export", s.export)
	mux.HandleFunc("POST /api/import", s.importData)

	mux.Handle("/", http.FileServer(http.FS(static)))
	return mux
}

type resp struct {
	OK   bool        `json:"ok"`
	Msg  string      `json:"msg,omitempty"`
	Data interface{} `json:"data,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func ok(w http.ResponseWriter, data interface{}) {
	writeJSON(w, http.StatusOK, resp{OK: true, Data: data})
}

func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, resp{OK: false, Msg: msg})
}

func decodeBody(r *http.Request, v interface{}) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, maxRequestBytes))
	return dec.Decode(v)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	ok(w, map[string]string{"status": "up"})
}

// ping 接收前端心跳，只更新时间戳，不写盘、不加锁。
func (s *Server) ping(w http.ResponseWriter, _ *http.Request) {
	s.act.Touch()
	ok(w, map[string]bool{"alive": true})
}

// windowState 返回浏览器窗口是否仍打开（托盘据此决定是否重复拉窗口）。
func (s *Server) windowState(w http.ResponseWriter, _ *http.Request) {
	ok(w, map[string]bool{"windowAlive": s.act.Alive(windowAliveWindow)})
}

// openWindow 让程序重新拉起（或聚焦）浏览器窗口，供托盘与第二个实例调用。
func (s *Server) openWindow(w http.ResponseWriter, _ *http.Request) {
	if s.onOpen == nil {
		fail(w, http.StatusNotImplemented, "当前运行模式不支持打开窗口")
		return
	}
	s.onOpen()
	ok(w, map[string]bool{"opened": true})
}

// quit 让程序干净退出。用于「再次双击程序时选择退出已有实例」的场景：
// 当系统托盘图标在某些环境不可见时，这是不依赖 UI 的可靠退出通道。
// 服务只监听 127.0.0.1，且先回包再退出，避免调用方拿到连接错误。
func (s *Server) quit(w http.ResponseWriter, _ *http.Request) {
	if s.onQuit == nil {
		fail(w, http.StatusNotImplemented, "当前运行模式不支持主动退出")
		return
	}
	ok(w, map[string]bool{"quitting": true})
	go func() {
		time.Sleep(300 * time.Millisecond)
		s.onQuit()
	}()
}

func (s *Server) workspace(w http.ResponseWriter, _ *http.Request) {
	ws := s.st.Snapshot()
	ok(w, map[string]interface{}{
		"version":    ws.Version,
		"interfaces": ws.Interfaces,
		"methods":    model.Methods,
		"dataDir":    s.st.Dir(),
		"dataFile":   s.st.File(),
	})
}

func (s *Server) createInterface(w http.ResponseWriter, r *http.Request) {
	var in model.Interface
	if err := decodeBody(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "请求数据格式错误")
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		in.Name = "未命名接口"
	}
	it, err := s.st.Create(&in)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	ok(w, it)
}

func (s *Server) updateInterface(w http.ResponseWriter, r *http.Request) {
	var in model.Interface
	if err := decodeBody(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "请求数据格式错误")
		return
	}
	it, err := s.st.Update(r.PathValue("id"), &in)
	if err != nil {
		handleErr(w, err)
		return
	}
	ok(w, it)
}

func (s *Server) deleteInterface(w http.ResponseWriter, r *http.Request) {
	if err := s.st.Delete(r.PathValue("id")); err != nil {
		handleErr(w, err)
		return
	}
	ok(w, nil)
}

func (s *Server) duplicateInterface(w http.ResponseWriter, r *http.Request) {
	it, err := s.st.Duplicate(r.PathValue("id"))
	if err != nil {
		handleErr(w, err)
		return
	}
	ok(w, it)
}

func (s *Server) execute(w http.ResponseWriter, r *http.Request) {
	var in model.Request
	if err := decodeBody(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "请求数据格式错误")
		return
	}
	in.Normalize()
	ok(w, executor.Execute(r.Context(), in))
}

type snapshotIn struct {
	Name     string         `json:"name"`
	Note     string         `json:"note"`
	Request  model.Request  `json:"request"`
	Response model.Response `json:"response"`
}

func (s *Server) createSnapshot(w http.ResponseWriter, r *http.Request) {
	var in snapshotIn
	if err := decodeBody(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "请求数据格式错误")
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		fail(w, http.StatusBadRequest, "副本名称不能为空")
		return
	}
	snap, err := s.st.AddSnapshot(r.PathValue("id"), &model.Snapshot{
		Name:     in.Name,
		Note:     in.Note,
		Request:  in.Request,
		Response: in.Response,
	})
	if err != nil {
		handleErr(w, err)
		return
	}
	ok(w, snap)
}

func (s *Server) getSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, err := s.st.GetSnapshot(r.PathValue("id"), r.PathValue("sid"))
	if err != nil {
		handleErr(w, err)
		return
	}
	ok(w, snap)
}

func (s *Server) renameSnapshot(w http.ResponseWriter, r *http.Request) {
	var in model.Snapshot
	if err := decodeBody(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "请求数据格式错误")
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		fail(w, http.StatusBadRequest, "副本名称不能为空")
		return
	}
	snap, err := s.st.UpdateSnapshot(r.PathValue("id"), r.PathValue("sid"), &in)
	if err != nil {
		handleErr(w, err)
		return
	}
	ok(w, snap)
}

func (s *Server) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteSnapshot(r.PathValue("id"), r.PathValue("sid")); err != nil {
		handleErr(w, err)
		return
	}
	ok(w, nil)
}

func (s *Server) export(w http.ResponseWriter, _ *http.Request) {
	b, err := s.st.Export()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="interfaces-backup.json"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func (s *Server) importData(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
	defer r.Body.Close()
	if err != nil {
		fail(w, http.StatusBadRequest, "读取导入内容失败")
		return
	}
	if err := s.st.Import(b); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	ok(w, nil)
}

func handleErr(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "数据不存在，可能已被删除")
		return
	}
	fail(w, http.StatusInternalServerError, err.Error())
}
