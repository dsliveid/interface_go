package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"interfacego/internal/model"
	"interfacego/internal/store"
)

// newTestServer 构造一个基于临时数据目录的 API 处理器。
func newTestServer(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	h := New(st, fstest.MapFS{"index.html": {Data: []byte("<html></html>")}}, Options{})
	return h, st
}

// call 发送请求并返回解析后的 data 字段与原始响应体。
func call(t *testing.T, h http.Handler, method, path string, body interface{}) (map[string]interface{}, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		switch v := body.(type) {
		case string:
			reader = bytes.NewBufferString(v)
		case []byte:
			reader = bytes.NewReader(v)
		default:
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("marshal body: %v", err)
			}
			reader = bytes.NewReader(b)
		}
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	raw := rec.Body.Bytes()

	var env struct {
		OK   bool                   `json:"ok"`
		Msg  string                 `json:"msg"`
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err == nil {
		if rec.Code >= 400 || !env.OK {
			t.Fatalf("%s %s 失败 [%d]: %s", method, path, rec.Code, env.Msg)
		}
	}
	return env.Data, raw
}

// wsPayload 构造一个可导入的工作区 JSON 字符串。
func wsPayload(names ...string) string {
	ws := model.Workspace{Version: 1}
	for i, n := range names {
		r := model.NewRequest()
		r.URL = "https://example.com/x"
		ws.Interfaces = append(ws.Interfaces, &model.Interface{
			ID:      "iface-" + string(rune('a'+i)),
			Name:    n,
			Group:   "原始组",
			Request: r,
		})
	}
	b, _ := json.Marshal(ws)
	return string(b)
}

// TestWorkspacePayloadHasWorkspaces 全量接口应带 current / workspaces 字段。
func TestWorkspacePayloadHasWorkspaces(t *testing.T) {
	h, _ := newTestServer(t)
	data, _ := call(t, h, "GET", "/api/workspace", nil)
	if data["current"] == nil || data["current"] == "" {
		t.Fatal("缺少 current 字段")
	}
	list, ok := data["workspaces"].([]interface{})
	if !ok || len(list) != 1 {
		t.Fatalf("workspaces 字段异常: %#v", data["workspaces"])
	}
	if _, ok := data["interfaces"]; !ok {
		t.Fatal("缺少 interfaces 字段")
	}
}

// TestWorkspaceCRUDRoutes 新建 / 重命名 / 切换 / 删除空间。
func TestWorkspaceCRUDRoutes(t *testing.T) {
	h, _ := newTestServer(t)

	data, _ := call(t, h, "POST", "/api/workspaces", map[string]string{"name": "空间A"})
	id, _ := data["id"].(string)
	if id == "" {
		t.Fatalf("新建空间未返回 id: %#v", data)
	}

	data, _ = call(t, h, "PUT", "/api/workspaces/"+id, map[string]string{"name": "空间A改名"})
	if data["name"] != "空间A改名" {
		t.Fatalf("重命名失败: %#v", data)
	}

	data, _ = call(t, h, "POST", "/api/workspaces/"+id+"/switch", nil)
	if data["current"] != id {
		t.Fatalf("切换后 current 不符: %v", data["current"])
	}

	if err := func() error {
		req := httptest.NewRequest("DELETE", "/api/workspaces/"+id, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			return io.ErrUnexpectedEOF
		}
		return nil
	}(); err != nil {
		t.Fatal("删除空间失败")
	}

	data, _ = call(t, h, "GET", "/api/workspaces", nil)
	list, _ := data["workspaces"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("删除后应剩 1 个空间，实际 %d", len(list))
	}
}

// TestImportNewWorkspaceRoute 新建空间导入后应成为当前空间。
func TestImportNewWorkspaceRoute(t *testing.T) {
	h, _ := newTestServer(t)
	payload := wsPayload("接口一", "接口二")
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		t.Fatal(err)
	}
	data, _ := call(t, h, "POST", "/api/workspaces/import-new", map[string]interface{}{
		"name": "导入空间",
		"data": parsed,
	})
	newID, _ := data["id"].(string)
	if newID == "" || data["name"] != "导入空间" {
		t.Fatalf("新建导入失败: %#v", data)
	}

	// 当前空间应已切换
	cur, _ := call(t, h, "GET", "/api/workspace", nil)
	if cur["current"] != newID {
		t.Fatalf("新建导入后当前空间未切换")
	}
	ifaces, _ := cur["interfaces"].([]interface{})
	if len(ifaces) != 2 {
		t.Fatalf("导入接口数不符: %d", len(ifaces))
	}
}

// TestImportToWorkspaceRoute 覆盖导入只影响目标空间。
func TestImportToWorkspaceRoute(t *testing.T) {
	h, _ := newTestServer(t)
	// 当前空间放一个接口
	call(t, h, "POST", "/api/interfaces", map[string]interface{}{
		"name":    "原有接口",
		"group":   "原有分组",
		"request": model.NewRequest(),
	})
	// 新建目标空间
	data, _ := call(t, h, "POST", "/api/workspaces", map[string]string{"name": "目标"})
	target, _ := data["id"].(string)

	res, _ := call(t, h, "POST", "/api/workspaces/"+target+"/import", wsPayload("导入A"))
	if res["imported"] != float64(1) {
		t.Fatalf("覆盖导入返回不符: %#v", res)
	}

	// 目标空间内容
	got, _ := call(t, h, "GET", "/api/workspaces/"+target, nil)
	ifaces, _ := got["interfaces"].([]interface{})
	if len(ifaces) != 1 {
		t.Fatalf("目标空间接口数不符: %d", len(ifaces))
	}
	// 当前空间未受影响
	cur, _ := call(t, h, "GET", "/api/workspace", nil)
	curIfaces, _ := cur["interfaces"].([]interface{})
	if len(curIfaces) != 1 {
		t.Fatalf("当前空间被误改: %d", len(curIfaces))
	}
	if cur["current"] == target {
		t.Fatal("覆盖导入不应切换当前空间")
	}
}

// TestCopyInterfacesRoute 批量复制到其他空间的分组。
func TestCopyInterfacesRoute(t *testing.T) {
	h, _ := newTestServer(t)
	created, _ := call(t, h, "POST", "/api/interfaces", map[string]interface{}{
		"name":    "待复制",
		"group":   "源分组",
		"request": model.NewRequest(),
	})
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatal("创建接口失败")
	}
	data, _ := call(t, h, "POST", "/api/workspaces", map[string]string{"name": "目标空间"})
	target, _ := data["id"].(string)

	res, _ := call(t, h, "POST", "/api/interfaces/copy", map[string]interface{}{
		"ids":               []string{id},
		"targetWorkspaceId": target,
		"targetGroup":       "目标分组",
		"regenerateId":      true,
	})
	if res["copied"] != float64(1) {
		t.Fatalf("复制结果不符: %#v", res)
	}

	got, _ := call(t, h, "GET", "/api/workspaces/"+target, nil)
	ifaces, _ := got["interfaces"].([]interface{})
	if len(ifaces) != 1 {
		t.Fatalf("目标空间接口数不符: %d", len(ifaces))
	}
	first, _ := ifaces[0].(map[string]interface{})
	if first["group"] != "目标分组" {
		t.Fatalf("分组未改写: %v", first["group"])
	}
	if first["id"] == id {
		t.Fatal("复制后 ID 应重新生成")
	}
}

// TestExportAllRoute 全部空间导出可下载且结构完整。
func TestExportAllRoute(t *testing.T) {
	h, _ := newTestServer(t)
	// 导出接口是文件下载，返回原始 JSON（非 {ok,data} 包装），故不走 call 辅助函数
	req := httptest.NewRequest("GET", "/api/export/all", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("导出失败: %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal("缺少下载响应头")
	}
	var bundle model.Bundle
	if err := json.Unmarshal(rec.Body.Bytes(), &bundle); err != nil {
		t.Fatalf("导出内容不可解析: %v", err)
	}
	if len(bundle.Workspaces) != 1 {
		t.Fatalf("导出空间数不符: %d", len(bundle.Workspaces))
	}
}

// TestLegacyImportRouteCompatible 旧 /api/import 仍可用（覆盖当前空间）。
func TestLegacyImportRouteCompatible(t *testing.T) {
	h, _ := newTestServer(t)
	call(t, h, "POST", "/api/import", wsPayload("旧接口"))
	cur, _ := call(t, h, "GET", "/api/workspace", nil)
	ifaces, _ := cur["interfaces"].([]interface{})
	if len(ifaces) != 1 {
		t.Fatalf("旧导入路由失效: %d", len(ifaces))
	}
}

// TestCopyInterfacesEmptySelection 未选接口应返回 400。
func TestCopyInterfacesEmptySelection(t *testing.T) {
	h, _ := newTestServer(t)
	body, _ := json.Marshal(map[string]interface{}{"ids": []string{}, "targetWorkspaceId": "x"})
	req := httptest.NewRequest("POST", "/api/interfaces/copy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空选择应返回 400，实际 %d", rec.Code)
	}
}
