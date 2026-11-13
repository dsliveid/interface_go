// Package store 负责多工作空间数据的读写、备份与接口数据操作。
//
// 磁盘布局：
//
//	data/index.json            空间清单（含当前选中空间）
//	data/workspaces/<wsid>.json 单个空间的数据文件
//	data/backup/               历史备份（按前缀分空间保留最近 N 份）
//
// 兼容性：旧的单文件 data/workspace.json 在首次启动时自动迁移为「默认工作空间」，
// 原文件重命名为 workspace.json.migrated 保留现场，不删除用户数据。
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"interfacego/internal/model"
)

// 备份保留份数。
const backupKeep = 30

// ErrNotFound 表示数据不存在。
var ErrNotFound = errors.New("数据不存在")

// ErrLastWorkspace 表示不允许删除最后一个工作空间。
var ErrLastWorkspace = errors.New("不能删除最后一个工作空间")

// ErrBadWorkspace 表示工作空间数据格式非法。
var ErrBadWorkspace = errors.New("导入内容不是合法的工作空间数据")

// CopyRequest 是批量复制接口到其他空间的请求。
type CopyRequest struct {
	IDs               []string `json:"ids"`
	TargetWorkspaceID string   `json:"targetWorkspaceId"`
	TargetGroup       string   `json:"targetGroup"`
	RegenerateID      bool     `json:"regenerateId"`
}

// CopyResult 是批量复制的结果。
type CopyResult struct {
	Copied            int    `json:"copied"`
	Skipped           int    `json:"skipped"`
	TargetWorkspaceID string `json:"targetWorkspaceId"`
	TargetGroup       string `json:"targetGroup"`
}

// 目标分组特殊值：保留每个接口原来的分组。
const keepGroup = "__keep__"

// Store 是线程安全的数据仓库。ws 始终指向当前工作空间。
type Store struct {
	mu     sync.RWMutex
	dir    string
	file   string // 当前空间数据文件路径
	ws     *model.Workspace
	idx    model.WorkspaceIndex
	spaces map[string]*model.Workspace // 已加载的空间缓存
}

// Open 打开（或初始化）数据目录。
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "workspaces"), 0o755); err != nil {
		return nil, fmt.Errorf("创建工作空间目录失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "backup"), 0o755); err != nil {
		return nil, fmt.Errorf("创建备份目录失败: %w", err)
	}
	s := &Store{dir: dir, spaces: map[string]*model.Workspace{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Dir 返回数据目录。
func (s *Store) Dir() string { return s.dir }

// File 返回当前空间数据文件路径（前端「数据文件」提示用）。
func (s *Store) File() string { return s.file }

func (s *Store) indexPath() string { return filepath.Join(s.dir, "index.json") }

func (s *Store) spaceFile(id string) string {
	return filepath.Join(s.dir, "workspaces", id+".json")
}

// load 读取空间清单；清单缺失时执行首次初始化或旧数据迁移。
func (s *Store) load() error {
	b, err := os.ReadFile(s.indexPath())
	if errors.Is(err, os.ErrNotExist) {
		return s.bootstrap()
	}
	if err != nil {
		return fmt.Errorf("读取空间清单失败: %w", err)
	}
	var idx model.WorkspaceIndex
	if err := json.Unmarshal(b, &idx); err != nil {
		broken := s.indexPath() + ".broken-" + time.Now().Format("20060102-150405")
		_ = os.Rename(s.indexPath(), broken)
		return fmt.Errorf("空间清单解析失败，已另存为 %s: %w", filepath.Base(broken), err)
	}
	// 剔除文件已丢失的条目，避免出现点不开的空间
	valid := make([]model.WorkspaceMeta, 0, len(idx.Workspaces))
	for _, m := range idx.Workspaces {
		if m.ID == "" {
			continue
		}
		if _, statErr := os.Stat(s.spaceFile(m.ID)); statErr != nil {
			continue
		}
		valid = append(valid, m)
	}
	fixed := len(valid) != len(idx.Workspaces)
	idx.Workspaces = valid
	if len(idx.Workspaces) == 0 {
		return s.bootstrap()
	}
	if idx.Version == 0 {
		idx.Version = 1
	}
	if !hasWorkspace(idx, idx.Current) {
		idx.Current = idx.Workspaces[0].ID
		fixed = true
	}
	s.idx = idx
	if err := s.activate(idx.Current); err != nil {
		return err
	}
	if fixed {
		if err := s.persistIndex(); err != nil {
			return err
		}
	}
	return nil
}

// hasWorkspace 判断清单里是否含指定空间。
func hasWorkspace(i model.WorkspaceIndex, id string) bool {
	for _, m := range i.Workspaces {
		if m.ID == id {
			return true
		}
	}
	return false
}

// bootstrap 首次初始化：优先迁移旧单文件数据，否则建空默认空间。
func (s *Store) bootstrap() error {
	legacy := filepath.Join(s.dir, "workspace.json")
	now := time.Now().UnixMilli()
	meta := model.WorkspaceMeta{
		ID:        "ws_" + NewID(),
		Name:      model.DefaultWorkspaceName,
		CreatedAt: now,
		UpdatedAt: now,
	}
	var ws *model.Workspace
	if b, err := os.ReadFile(legacy); err == nil {
		// 迁移前 dry-run 校验：必须能解析出 interfaces 才搬迁，避免搬坏数据
		var parsed model.Workspace
		if jsonErr := json.Unmarshal(b, &parsed); jsonErr != nil {
			broken := legacy + ".broken-" + time.Now().Format("20060102-150405")
			_ = os.Rename(legacy, broken)
			return fmt.Errorf("旧数据文件解析失败，已另存为 %s: %w", filepath.Base(broken), jsonErr)
		}
		if parsed.Interfaces == nil {
			parsed.Interfaces = []*model.Interface{}
		}
		if parsed.Version == 0 {
			parsed.Version = 1
		}
		for _, it := range parsed.Interfaces {
			if it.ID == "" {
				it.ID = NewID()
			}
			it.Normalize()
			dedupeSnapshots(it)
		}
		ws = &parsed
		meta.Name = model.DefaultWorkspaceName
	} else {
		ws = &model.Workspace{Version: 1, Interfaces: []*model.Interface{}}
	}
	s.spaces[meta.ID] = ws
	s.idx = model.WorkspaceIndex{Version: 1, Current: meta.ID, Workspaces: []model.WorkspaceMeta{meta}}
	s.ws = ws
	s.file = s.spaceFile(meta.ID)
	if err := s.persistSpace(meta.ID); err != nil {
		return err
	}
	if err := s.persistIndex(); err != nil {
		return err
	}
	if _, err := os.Stat(legacy); err == nil {
		// 旧文件不删除，改名留证，便于用户核对迁移结果
		_ = os.Rename(legacy, legacy+".migrated")
	}
	return nil
}

// activate 将指定空间载入内存并设为当前空间。
func (s *Store) activate(id string) error {
	ws, err := s.loadSpace(id)
	if err != nil {
		return err
	}
	s.spaces[id] = ws
	s.ws = ws
	s.idx.Current = id
	s.file = s.spaceFile(id)
	return nil
}

// loadSpace 读取并规范化指定空间的数据。文件不存在时返回空空间。
func (s *Store) loadSpace(id string) (*model.Workspace, error) {
	if ws, ok := s.spaces[id]; ok {
		return ws, nil
	}
	path := s.spaceFile(id)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &model.Workspace{Version: 1, Interfaces: []*model.Interface{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取工作空间失败: %w", err)
	}
	var ws model.Workspace
	if err := json.Unmarshal(b, &ws); err != nil {
		broken := path + ".broken-" + time.Now().Format("20060102-150405")
		_ = os.Rename(path, broken)
		return nil, fmt.Errorf("工作空间数据解析失败，已另存为 %s: %w", filepath.Base(broken), err)
	}
	if ws.Interfaces == nil {
		ws.Interfaces = []*model.Interface{}
	}
	if ws.Version == 0 {
		ws.Version = 1
	}
	for _, it := range ws.Interfaces {
		it.Normalize()
		dedupeSnapshots(it)
	}
	return &ws, nil
}

// dedupeSnapshots 按副本 ID 去重后写回接口。
// 历史版本前端在「保存副本」时会把刚创建、且已被服务器返回列表包含的副本再 push 一次，
// 落盘后同一副本出现两份；表现就是接口下多出一个同名副本，且删掉一个后另一个还在。
// 这里在加载阶段做一次自愈，防止既有脏数据继续作恶。
func dedupeSnapshots(it *model.Interface) {
	if len(it.Snapshots) < 2 {
		return
	}
	seen := make(map[string]struct{}, len(it.Snapshots))
	kept := make([]model.Snapshot, 0, len(it.Snapshots))
	for _, sn := range it.Snapshots {
		if sn.ID != "" {
			if _, dup := seen[sn.ID]; dup {
				continue
			}
			seen[sn.ID] = struct{}{}
		}
		kept = append(kept, sn)
	}
	it.Snapshots = kept
}

// persist 原子写入当前空间：先备份旧文件，再写临时文件并重命名。
func (s *Store) persist() error {
	return s.persistSpace(s.idx.Current)
}

// persistSpace 原子写入指定空间（内存中必须已加载）。
func (s *Store) persistSpace(id string) error {
	ws, ok := s.spaces[id]
	if !ok {
		return fmt.Errorf("工作空间 %s 未加载", id)
	}
	b, err := json.MarshalIndent(ws, "", "  ")
	if err != nil {
		return err
	}
	path := s.spaceFile(id)
	if _, statErr := os.Stat(path); statErr == nil {
		_ = s.backupFile(path, "space-"+id)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("替换数据文件失败: %w", err)
	}
	// 同步清单里的计数与更新时间
	for i := range s.idx.Workspaces {
		if s.idx.Workspaces[i].ID == id {
			s.idx.Workspaces[i].Count = len(ws.Interfaces)
			s.idx.Workspaces[i].UpdatedAt = time.Now().UnixMilli()
			return s.persistIndex()
		}
	}
	return s.persistIndex()
}

// persistIndex 原子写入空间清单。
func (s *Store) persistIndex() error {
	b, err := json.MarshalIndent(s.idx, "", "  ")
	if err != nil {
		return err
	}
	path := s.indexPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("写入空间清单失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("替换空间清单失败: %w", err)
	}
	return nil
}

// backupFile 把指定文件复制进 backup 目录，并按前缀保留最近 backupKeep 份。
func (s *Store) backupFile(path, prefix string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%s-%s.json", prefix, time.Now().Format("20060102-150405.000"))
	if err := os.WriteFile(filepath.Join(s.dir, "backup", name), data, 0o644); err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(s.dir, "backup"))
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), prefix+"-") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > backupKeep {
		_ = os.Remove(filepath.Join(s.dir, "backup", names[0]))
		names = names[1:]
	}
	return nil
}

// NewID 生成短随机 ID。
func NewID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// Snapshot 返回整个工作区数据的深拷贝。
func (s *Store) Snapshot() model.Workspace {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := model.Workspace{Version: s.ws.Version, Interfaces: make([]*model.Interface, 0, len(s.ws.Interfaces))}
	for _, it := range s.ws.Interfaces {
		out.Interfaces = append(out.Interfaces, it)
	}
	return out
}

func (s *Store) index(id string) int {
	for i, it := range s.ws.Interfaces {
		if it.ID == id {
			return i
		}
	}
	return -1
}

// Get 按 ID 取接口。
func (s *Store) Get(id string) (*model.Interface, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if i := s.index(id); i >= 0 {
		return s.ws.Interfaces[i], nil
	}
	return nil, ErrNotFound
}

// Create 新建接口。
func (s *Store) Create(it *model.Interface) (*model.Interface, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UnixMilli()
	if it.ID == "" {
		it.ID = NewID()
	}
	it.CreatedAt = now
	it.UpdatedAt = now
	it.Snapshots = []model.Snapshot{}
	it.Request.Normalize()
	s.ws.Interfaces = append(s.ws.Interfaces, it)
	if err := s.persist(); err != nil {
		return nil, err
	}
	return it, nil
}

// Update 覆盖保存接口配置（保留原有副本列表）。
func (s *Store) Update(id string, in *model.Interface) (*model.Interface, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(id)
	if i < 0 {
		return nil, ErrNotFound
	}
	cur := s.ws.Interfaces[i]
	cur.Name = in.Name
	cur.Group = in.Group
	cur.Doc = in.Doc
	cur.Request = in.Request
	cur.Request.Normalize()
	// 最近一次出参随接口一起落库
	cur.Response = in.Response
	cur.Response.Normalize()
	cur.UpdatedAt = time.Now().UnixMilli()
	if err := s.persist(); err != nil {
		return nil, err
	}
	return cur, nil
}

// Delete 删除接口（连同其下副本）。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(id)
	if i < 0 {
		return ErrNotFound
	}
	s.ws.Interfaces = append(s.ws.Interfaces[:i], s.ws.Interfaces[i+1:]...)
	return s.persist()
}

// Duplicate 复制接口为新的独立接口（不带副本）。
func (s *Store) Duplicate(id string) (*model.Interface, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(id)
	if i < 0 {
		return nil, ErrNotFound
	}
	src := s.ws.Interfaces[i]
	now := time.Now().UnixMilli()
	cp := *src
	cp.ID = NewID()
	cp.Name = src.Name + "_副本"
	cp.CreatedAt = now
	cp.UpdatedAt = now
	cp.Snapshots = []model.Snapshot{}
	s.ws.Interfaces = append(s.ws.Interfaces, &cp)
	if err := s.persist(); err != nil {
		return nil, err
	}
	return &cp, nil
}

// AddSnapshot 给接口挂一个副本。
func (s *Store) AddSnapshot(id string, snap *model.Snapshot) (*model.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(id)
	if i < 0 {
		return nil, ErrNotFound
	}
	if snap.ID == "" {
		snap.ID = NewID()
	}
	if snap.CreatedAt == 0 {
		snap.CreatedAt = time.Now().UnixMilli()
	}
	snap.Request.Normalize()
	snap.Response.Normalize()
	cur := s.ws.Interfaces[i]
	cur.Snapshots = append(cur.Snapshots, *snap)
	cur.UpdatedAt = time.Now().UnixMilli()
	if err := s.persist(); err != nil {
		return nil, err
	}
	return snap, nil
}

// GetSnapshot 取接口下某个副本。
func (s *Store) GetSnapshot(id, sid string) (*model.Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	i := s.index(id)
	if i < 0 {
		return nil, ErrNotFound
	}
	for _, sn := range s.ws.Interfaces[i].Snapshots {
		if sn.ID == sid {
			cp := sn
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

// UpdateSnapshot 全量更新副本（名称/备注/文档/请求配置/出参），返回更新后的副本。
func (s *Store) UpdateSnapshot(id, sid string, in *model.Snapshot) (*model.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(id)
	if i < 0 {
		return nil, ErrNotFound
	}
	cur := s.ws.Interfaces[i]
	for idx := range cur.Snapshots {
		if cur.Snapshots[idx].ID != sid {
			continue
		}
		in.ID = sid
		in.CreatedAt = cur.Snapshots[idx].CreatedAt
		in.Request.Normalize()
		in.Response.Normalize()
		cur.Snapshots[idx] = *in
		cur.UpdatedAt = time.Now().UnixMilli()
		if err := s.persist(); err != nil {
			return nil, err
		}
		updated := cur.Snapshots[idx]
		return &updated, nil
	}
	return nil, ErrNotFound
}

// DeleteSnapshot 删除副本。
func (s *Store) DeleteSnapshot(id, sid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(id)
	if i < 0 {
		return ErrNotFound
	}
	cur := s.ws.Interfaces[i]
	for idx := range cur.Snapshots {
		if cur.Snapshots[idx].ID == sid {
			cur.Snapshots = append(cur.Snapshots[:idx], cur.Snapshots[idx+1:]...)
			return s.persist()
		}
	}
	return ErrNotFound
}

// Export 导出当前工作空间。
func (s *Store) Export() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return json.MarshalIndent(s.ws, "", "  ")
}

// ExportWorkspace 导出指定工作空间。
func (s *Store) ExportWorkspace(id string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ws, err := s.loadSpace(id)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(ws, "", "  ")
}

// ExportAll 导出全部工作空间（打包结构）。
func (s *Store) ExportAll() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := model.Bundle{Version: 1, ExportedAt: time.Now().UnixMilli()}
	for _, m := range s.idx.Workspaces {
		ws, err := s.loadSpace(m.ID)
		if err != nil {
			return nil, err
		}
		m.Count = len(ws.Interfaces)
		out.Workspaces = append(out.Workspaces, model.WorkspaceBundle{Meta: m, Data: *ws})
	}
	return json.MarshalIndent(out, "", "  ")
}

// Workspaces 返回空间清单与当前空间 ID。
func (s *Store) Workspaces() ([]model.WorkspaceMeta, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.WorkspaceMeta, len(s.idx.Workspaces))
	copy(out, s.idx.Workspaces)
	return out, s.idx.Current
}

// Current 返回当前空间 ID。
func (s *Store) Current() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.idx.Current
}

// CreateWorkspace 新建一个空工作空间（不切换当前空间）。
func (s *Store) CreateWorkspace(name string) (*model.WorkspaceMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name = strings.TrimSpace(name)
	if name == "" {
		name = "新工作空间"
	}
	now := time.Now().UnixMilli()
	meta := model.WorkspaceMeta{ID: "ws_" + NewID(), Name: name, CreatedAt: now, UpdatedAt: now}
	s.spaces[meta.ID] = &model.Workspace{Version: 1, Interfaces: []*model.Interface{}}
	s.idx.Workspaces = append(s.idx.Workspaces, meta)
	if err := s.persistSpace(meta.ID); err != nil {
		return nil, err
	}
	return &meta, nil
}

// RenameWorkspace 重命名工作空间。
func (s *Store) RenameWorkspace(id, name string) (*model.WorkspaceMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("空间名称不能为空")
	}
	for i := range s.idx.Workspaces {
		if s.idx.Workspaces[i].ID != id {
			continue
		}
		s.idx.Workspaces[i].Name = name
		s.idx.Workspaces[i].UpdatedAt = time.Now().UnixMilli()
		if err := s.persistIndex(); err != nil {
			return nil, err
		}
		meta := s.idx.Workspaces[i]
		return &meta, nil
	}
	return nil, ErrNotFound
}

// DeleteWorkspace 删除工作空间（不允许删除最后一个）。
func (s *Store) DeleteWorkspace(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.idx.Workspaces) <= 1 {
		return ErrLastWorkspace
	}
	pos := -1
	for i := range s.idx.Workspaces {
		if s.idx.Workspaces[i].ID == id {
			pos = i
			break
		}
	}
	if pos < 0 {
		return ErrNotFound
	}
	path := s.spaceFile(id)
	if _, err := os.Stat(path); err == nil {
		_ = s.backupFile(path, "space-"+id)
	}
	_ = os.Remove(path)
	delete(s.spaces, id)
	s.idx.Workspaces = append(s.idx.Workspaces[:pos], s.idx.Workspaces[pos+1:]...)
	if s.idx.Current == id {
		next := s.idx.Workspaces[0].ID
		if err := s.activate(next); err != nil {
			return err
		}
	}
	return s.persistIndex()
}

// SetCurrent 切换当前工作空间。
func (s *Store) SetCurrent(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !hasWorkspace(s.idx, id) {
		return ErrNotFound
	}
	if err := s.activate(id); err != nil {
		return err
	}
	return s.persistIndex()
}

// SnapshotOf 返回指定空间的接口列表快照。
func (s *Store) SnapshotOf(id string) (model.Workspace, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ws, err := s.loadSpace(id)
	if err != nil {
		return model.Workspace{}, err
	}
	out := model.Workspace{Version: ws.Version, Interfaces: make([]*model.Interface, 0, len(ws.Interfaces))}
	out.Interfaces = append(out.Interfaces, ws.Interfaces...)
	return out, nil
}

// GroupsOf 返回指定空间已有的分组名（去重、保持出现顺序）。
func (s *Store) GroupsOf(id string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ws, err := s.loadSpace(id)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := []string{}
	for _, it := range ws.Interfaces {
		g := strings.TrimSpace(it.Group)
		if g == "" || seen[g] {
			continue
		}
		seen[g] = true
		out = append(out, g)
	}
	return out, nil
}

// parseWorkspace 解析导入内容为工作空间（自动补 ID / 规范化）。
func parseWorkspace(b []byte) (*model.Workspace, error) {
	var ws model.Workspace
	if err := json.Unmarshal(b, &ws); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadWorkspace, err)
	}
	if ws.Interfaces == nil {
		return nil, fmt.Errorf("%w: 缺少 interfaces 字段", ErrBadWorkspace)
	}
	if ws.Version == 0 {
		ws.Version = 1
	}
	for _, it := range ws.Interfaces {
		if it.ID == "" {
			it.ID = NewID()
		}
		it.Normalize()
		dedupeSnapshots(it)
	}
	return &ws, nil
}

// Import 覆盖导入到当前工作空间。
func (s *Store) Import(b []byte) error {
	return s.ImportTo(s.Current(), b)
}

// ImportTo 覆盖导入到指定工作空间（其他空间不受影响）。
func (s *Store) ImportTo(id string, b []byte) error {
	ws, err := parseWorkspace(b)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !hasWorkspace(s.idx, id) {
		return ErrNotFound
	}
	s.spaces[id] = ws
	if s.idx.Current == id {
		s.ws = ws
		s.file = s.spaceFile(id)
	}
	return s.persistSpace(id)
}

// ImportNewWorkspace 新建工作空间并导入数据，随后把它设为当前空间。
func (s *Store) ImportNewWorkspace(name string, b []byte) (*model.WorkspaceMeta, error) {
	ws, err := parseWorkspace(b)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	name = strings.TrimSpace(name)
	if name == "" {
		name = "导入-" + time.Now().Format("20060102-150405")
	}
	now := time.Now().UnixMilli()
	meta := model.WorkspaceMeta{ID: "ws_" + NewID(), Name: name, CreatedAt: now, UpdatedAt: now, Count: len(ws.Interfaces)}
	s.spaces[meta.ID] = ws
	s.idx.Workspaces = append(s.idx.Workspaces, meta)
	if err := s.persistSpace(meta.ID); err != nil {
		return nil, err
	}
	if err := s.activate(meta.ID); err != nil {
		return nil, err
	}
	if err := s.persistIndex(); err != nil {
		return nil, err
	}
	return &meta, nil
}

// copyInterface 深拷贝单个接口并按需重生成 ID / 改写分组。
func copyInterface(src *model.Interface, group string, regenerate bool, now int64) *model.Interface {
	b, err := json.Marshal(src)
	if err != nil {
		// 结构简单，正常不会失败；兜底退回浅拷贝
		cp := *src
		return &cp
	}
	var cp model.Interface
	if err := json.Unmarshal(b, &cp); err != nil {
		shallow := *src
		return &shallow
	}
	if regenerate {
		cp.ID = NewID()
		for i := range cp.Snapshots {
			cp.Snapshots[i].ID = NewID()
		}
	}
	if group != keepGroup {
		cp.Group = group
	}
	cp.CreatedAt = now
	cp.UpdatedAt = now
	cp.Normalize()
	return &cp
}

// CopyInterfaces 把当前空间的若干接口批量复制到目标空间的目标分组。
func (s *Store) CopyInterfaces(req CopyRequest) (*CopyResult, error) {
	target := strings.TrimSpace(req.TargetWorkspaceID)
	if target == "" {
		return nil, errors.New("请选择目标工作空间")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !hasWorkspace(s.idx, target) {
		return nil, ErrNotFound
	}
	dst, err := s.loadSpace(target)
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	group := req.TargetGroup
	if strings.TrimSpace(group) == "" {
		group = keepGroup
	}
	res := &CopyResult{TargetWorkspaceID: target, TargetGroup: group}
	if group == keepGroup {
		res.TargetGroup = ""
	}
	for _, id := range req.IDs {
		i := s.index(id)
		if i < 0 {
			res.Skipped++
			continue
		}
		cp := copyInterface(s.ws.Interfaces[i], group, req.RegenerateID, now)
		dst.Interfaces = append(dst.Interfaces, cp)
		res.Copied++
	}
	if res.Copied == 0 {
		return res, errors.New("没有可复制的接口")
	}
	s.spaces[target] = dst
	if err := s.persistSpace(target); err != nil {
		return nil, err
	}
	return res, nil
}
