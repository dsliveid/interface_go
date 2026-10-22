// Package api 提供本地 HTTP 接口，供内嵌页面调用。
package api

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"strings"

	"interfacego/internal/executor"
	"interfacego/internal/model"
	"interfacego/internal/store"
)

// maxRequestBytes 限制请求体大小（保存副本时可能带上很大的响应体）。
const maxRequestBytes = 16 << 20

// Server 承载所有本地接口。
type Server struct {
	st     *store.Store
	static fs.FS
}

// New 构造路由。
func New(st *store.Store, static fs.FS) http.Handler {
	s := &Server{st: st, static: static}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.health)
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
