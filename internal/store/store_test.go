package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"interfacego/internal/model"
)

// mkIface 构造一个测试用接口。
func mkIface(id, name, group string) *model.Interface {
	r := model.NewRequest()
	r.URL = "https://example.com/" + id
	return &model.Interface{
		ID:      id,
		Name:    name,
		Group:   group,
		Request: r,
		Snapshots: []model.Snapshot{
			{ID: id + "-snap", Name: "副本", Request: r},
		},
	}
}

// TestBootstrapEmpty 空目录应自动创建默认工作空间。
func TestBootstrapEmpty(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	metas, cur := s.Workspaces()
	if len(metas) != 1 {
		t.Fatalf("期望 1 个空间，实际 %d", len(metas))
	}
	if metas[0].Name != model.DefaultWorkspaceName {
		t.Fatalf("默认空间名不符: %s", metas[0].Name)
	}
	if cur != metas[0].ID {
		t.Fatalf("当前空间不匹配")
	}
	if _, err := os.Stat(filepath.Join(dir, "index.json")); err != nil {
		t.Fatalf("index.json 未生成: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "workspaces", cur+".json")); err != nil {
		t.Fatalf("空间数据文件未生成: %v", err)
	}
}

// TestMigrateLegacy 旧单文件数据应自动迁移且接口数量一致、旧文件改名保留。
func TestMigrateLegacy(t *testing.T) {
	dir := t.TempDir()
	legacy := model.Workspace{
		Version: 1,
		Interfaces: []*model.Interface{
			mkIface("a1", "接口一", "用户"),
			mkIface("a2", "接口二", "订单"),
		},
	}
	b, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "workspace.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ws := s.Snapshot()
	if len(ws.Interfaces) != 2 {
		t.Fatalf("迁移后接口数应为 2，实际 %d", len(ws.Interfaces))
	}
	if ws.Interfaces[0].Group != "用户" {
		t.Fatalf("分组丢失: %+v", ws.Interfaces[0])
	}
	if _, err := os.Stat(filepath.Join(dir, "workspace.json")); !os.IsNotExist(err) {
		t.Fatalf("旧文件应已改名，实际仍存在")
	}
	if _, err := os.Stat(filepath.Join(dir, "workspace.json.migrated")); err != nil {
		t.Fatalf("旧文件未被保留为 .migrated: %v", err)
	}
}

// TestWorkspaceCRUD 空间新建/重命名/删除与最后空间保护。
func TestWorkspaceCRUD(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, _ := s.Workspaces()
	if err := s.DeleteWorkspace(first[0].ID); err != ErrLastWorkspace {
		t.Fatalf("删除最后一个空间应被拒绝，实际 %v", err)
	}
	meta, err := s.CreateWorkspace("第二空间")
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if meta.Name != "第二空间" {
		t.Fatalf("空间名不符: %s", meta.Name)
	}
	if _, err := s.RenameWorkspace(meta.ID, "改名后"); err != nil {
		t.Fatalf("RenameWorkspace: %v", err)
	}
	metas, _ := s.Workspaces()
	if metas[1].Name != "改名后" {
		t.Fatalf("重命名未生效")
	}
	if err := s.SetCurrent(meta.ID); err != nil {
		t.Fatalf("SetCurrent: %v", err)
	}
	if s.Current() != meta.ID {
		t.Fatalf("当前空间未切换")
	}
	if err := s.DeleteWorkspace(meta.ID); err != nil {
		t.Fatalf("DeleteWorkspace: %v", err)
	}
	metas, cur := s.Workspaces()
	if len(metas) != 1 || cur != metas[0].ID {
		t.Fatalf("删除后应回退到剩余空间，实际 %d 个，cur=%s", len(metas), cur)
	}
}

// TestImportToOnlyTarget 覆盖导入只影响目标空间。
func TestImportToOnlyTarget(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateWorkspace("目标空间")
	if err != nil {
		t.Fatal(err)
	}
	cur := s.Current()
	// 给当前空间放一个接口，确认覆盖目标空间后它仍然存在
	if _, err := s.Create(mkIface("", "原接口", "原始")); err != nil {
		t.Fatal(err)
	}

	payload, _ := json.Marshal(model.Workspace{
		Version:    1,
		Interfaces: []*model.Interface{mkIface("i1", "导入接口", "导入组")},
	})
	if err := s.ImportTo(other.ID, payload); err != nil {
		t.Fatalf("ImportTo: %v", err)
	}

	target, err := s.SnapshotOf(other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(target.Interfaces) != 1 || target.Interfaces[0].Name != "导入接口" {
		t.Fatalf("目标空间导入结果不符: %d", len(target.Interfaces))
	}
	if s.Current() != cur {
		t.Fatalf("覆盖导入不应切换当前空间")
	}
	curWS := s.Snapshot()
	if len(curWS.Interfaces) != 1 || curWS.Interfaces[0].Name != "原接口" {
		t.Fatalf("其他空间数据被误改: %+v", curWS.Interfaces)
	}
}

// TestImportNewWorkspace 新建空间导入后应成为当前空间。
func TestImportNewWorkspace(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(model.Workspace{
		Version:    1,
		Interfaces: []*model.Interface{mkIface("n1", "新导入", "组A")},
	})
	meta, err := s.ImportNewWorkspace("导入空间", payload)
	if err != nil {
		t.Fatalf("ImportNewWorkspace: %v", err)
	}
	if meta.Name != "导入空间" || meta.Count != 1 {
		t.Fatalf("新建空间 meta 不符: %+v", meta)
	}
	if s.Current() != meta.ID {
		t.Fatalf("新建导入后应切换到新空间")
	}
	if got := s.Snapshot(); len(got.Interfaces) != 1 {
		t.Fatalf("新空间接口数不符: %d", len(got.Interfaces))
	}
	metas, _ := s.Workspaces()
	if len(metas) != 2 {
		t.Fatalf("空间总数应为 2，实际 %d", len(metas))
	}
}

// TestImportBadPayload 非法导入内容应报 ErrBadWorkspace。
func TestImportBadPayload(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Import([]byte("not json")); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
	if err := s.Import([]byte(`{"version":1}`)); err == nil {
		t.Fatal("缺少 interfaces 应报错")
	}
}

// TestCopyInterfaces 跨空间复制应重生成 ID、改写分组并保留副本。
func TestCopyInterfaces(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := mkIface("", "源接口", "源分组")
	created, err := s.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	// Create 会重置副本列表，这里通过 AddSnapshot 单独挂一个副本
	snap, err := s.AddSnapshot(created.ID, &model.Snapshot{Name: "副本", Request: created.Request})
	if err != nil {
		t.Fatal(err)
	}
	dst, err := s.CreateWorkspace("目标空间")
	if err != nil {
		t.Fatal(err)
	}

	res, err := s.CopyInterfaces(CopyRequest{
		IDs:               []string{created.ID, "missing-id"},
		TargetWorkspaceID: dst.ID,
		TargetGroup:       "目标分组",
		RegenerateID:      true,
	})
	if err != nil {
		t.Fatalf("CopyInterfaces: %v", err)
	}
	if res.Copied != 1 || res.Skipped != 1 {
		t.Fatalf("复制结果不符: %+v", res)
	}

	target, err := s.SnapshotOf(dst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(target.Interfaces) != 1 {
		t.Fatalf("目标空间接口数不符: %d", len(target.Interfaces))
	}
	cp := target.Interfaces[0]
	if cp.ID == created.ID {
		t.Fatal("复制后 ID 应重新生成")
	}
	if cp.Group != "目标分组" {
		t.Fatalf("分组未改写: %s", cp.Group)
	}
	if len(cp.Snapshots) != 1 {
		t.Fatalf("副本未随复制保留: %d", len(cp.Snapshots))
	}
	if cp.Snapshots[0].ID == snap.ID {
		t.Fatal("副本 ID 应重新生成")
	}
	// 源空间不受影响
	if got := s.Snapshot(); len(got.Interfaces) != 1 || got.Interfaces[0].Group != "源分组" {
		t.Fatalf("源空间被误改: %+v", got.Interfaces)
	}
}

// TestCopyKeepGroup 目标分组为空时应保留原分组。
func TestCopyKeepGroup(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.Create(mkIface("", "接口", "原分组"))
	if err != nil {
		t.Fatal(err)
	}
	dst, _ := s.CreateWorkspace("目标")
	if _, err := s.CopyInterfaces(CopyRequest{
		IDs:               []string{created.ID},
		TargetWorkspaceID: dst.ID,
		RegenerateID:      true,
	}); err != nil {
		t.Fatalf("CopyInterfaces: %v", err)
	}
	target, _ := s.SnapshotOf(dst.ID)
	if target.Interfaces[0].Group != "原分组" {
		t.Fatalf("应保留原分组，实际 %s", target.Interfaces[0].Group)
	}
}

// TestPersistence 重启后数据应保持一致（多空间独立落盘）。
func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(mkIface("", "空间一接口", "A")); err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateWorkspace("空间二")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(model.Workspace{
		Version:    1,
		Interfaces: []*model.Interface{mkIface("x1", "空间二接口", "B")},
	})
	if err := s.ImportTo(second.ID, payload); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCurrent(second.ID); err != nil {
		t.Fatal(err)
	}

	// 重新打开
	again, err := Open(dir)
	if err != nil {
		t.Fatalf("重新 Open: %v", err)
	}
	metas, cur := again.Workspaces()
	if len(metas) != 2 {
		t.Fatalf("重启后空间数应为 2，实际 %d", len(metas))
	}
	if cur != second.ID {
		t.Fatalf("当前空间未持久化")
	}
	if got := again.Snapshot(); len(got.Interfaces) != 1 || got.Interfaces[0].Name != "空间二接口" {
		t.Fatalf("当前空间数据不符: %+v", got.Interfaces)
	}
	// 另一个空间数据仍在
	for _, m := range metas {
		if m.ID == second.ID {
			continue
		}
		ws, err := again.SnapshotOf(m.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(ws.Interfaces) != 1 || ws.Interfaces[0].Name != "空间一接口" {
			t.Fatalf("空间一数据丢失: %+v", ws.Interfaces)
		}
	}
}

// TestExportAll 全部空间导出结构完整。
func TestExportAll(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(mkIface("", "接口", "组")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateWorkspace("第二"); err != nil {
		t.Fatal(err)
	}
	b, err := s.ExportAll()
	if err != nil {
		t.Fatalf("ExportAll: %v", err)
	}
	var bundle model.Bundle
	if err := json.Unmarshal(b, &bundle); err != nil {
		t.Fatalf("导出内容不可解析: %v", err)
	}
	if len(bundle.Workspaces) != 2 {
		t.Fatalf("导出空间数不符: %d", len(bundle.Workspaces))
	}
}

// TestSnapshotDuplicateSelfHeal 历史脏数据（同一副本被重复挂载）在加载时自动去重，
// 且删除该副本后不会残留第二份。
func TestSnapshotDuplicateSelfHeal(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.Create(mkIface("", "接口", "A"))
	if err != nil {
		t.Fatal(err)
	}
	r := model.NewRequest()
	r.URL = "https://example.com/dup"
	snap := &model.Snapshot{ID: "dup-snap", Name: "副本-1", Request: r}
	// 模拟旧版前端把同一副本 push 两次
	if _, err := s.AddSnapshot(it.ID, snap); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddSnapshot(it.ID, snap); err != nil {
		t.Fatal(err)
	}
	cur, err := s.Get(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cur.Snapshots) != 2 {
		t.Fatalf("构造脏数据失败，副本数=%d", len(cur.Snapshots))
	}

	again, err := Open(dir)
	if err != nil {
		t.Fatalf("重新 Open: %v", err)
	}
	reloaded, err := again.Get(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Snapshots) != 1 || reloaded.Snapshots[0].ID != "dup-snap" {
		t.Fatalf("加载阶段未按副本 ID 去重: %+v", reloaded.Snapshots)
	}
	if err := again.DeleteSnapshot(it.ID, "dup-snap"); err != nil {
		t.Fatal(err)
	}
	after, err := again.Get(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Snapshots) != 0 {
		t.Fatalf("删除副本后仍残留 %d 份", len(after.Snapshots))
	}
}

// TestGroupsOf 分组名去重且保持顺序。
func TestGroupsOf(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []string{"甲", "乙", "甲", ""} {
		if _, err := s.Create(mkIface("", "接口", g)); err != nil {
			t.Fatal(err)
		}
	}
	groups, err := s.GroupsOf(s.Current())
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || groups[0] != "甲" || groups[1] != "乙" {
		t.Fatalf("分组结果不符: %v", groups)
	}
}
