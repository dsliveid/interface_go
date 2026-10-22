// Package store 负责 workspace.json 的读写、备份与接口数据操作。
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

// Store 是线程安全的数据仓库。
type Store struct {
	mu   sync.RWMutex
	dir  string
	file string
	ws   *model.Workspace
}

// Open 打开（或初始化）数据目录。
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "backup"), 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}
	s := &Store{dir: dir, file: filepath.Join(dir, "workspace.json")}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Dir 返回数据目录。
func (s *Store) Dir() string { return s.dir }

// File 返回数据文件路径。
func (s *Store) File() string { return s.file }

func (s *Store) load() error {
	b, err := os.ReadFile(s.file)
	if errors.Is(err, os.ErrNotExist) {
		s.ws = &model.Workspace{Version: 1, Interfaces: []*model.Interface{}}
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取数据文件失败: %w", err)
	}
	var ws model.Workspace
	if err := json.Unmarshal(b, &ws); err != nil {
		// 数据损坏时保留现场，避免直接覆盖用户数据
		broken := s.file + ".broken-" + time.Now().Format("20060102-150405")
		_ = os.Rename(s.file, broken)
		s.ws = &model.Workspace{Version: 1, Interfaces: []*model.Interface{}}
		return fmt.Errorf("数据文件解析失败，已另存为 %s: %w", filepath.Base(broken), err)
	}
	if ws.Interfaces == nil {
		ws.Interfaces = []*model.Interface{}
	}
	if ws.Version == 0 {
		ws.Version = 1
	}
	for _, it := range ws.Interfaces {
		it.Normalize()
	}
	s.ws = &ws
	return nil
}

// persist 原子写入：先备份旧文件，再写临时文件并重命名。
func (s *Store) persist() error {
	b, err := json.MarshalIndent(s.ws, "", "  ")
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(s.file); statErr == nil {
		_ = s.backup()
	}
	tmp := s.file + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := os.Rename(tmp, s.file); err != nil {
		return fmt.Errorf("替换数据文件失败: %w", err)
	}
	return nil
}

func (s *Store) backup() error {
	data, err := os.ReadFile(s.file)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("workspace-%s.json", time.Now().Format("20060102-150405.000"))
	path := filepath.Join(s.dir, "backup", name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(s.dir, "backup"))
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "workspace-") {
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

// Export 导出整个工作区。
func (s *Store) Export() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return json.MarshalIndent(s.ws, "", "  ")
}

// Import 整体导入（覆盖当前数据）。
func (s *Store) Import(b []byte) error {
	var ws model.Workspace
	if err := json.Unmarshal(b, &ws); err != nil {
		return fmt.Errorf("导入文件不是合法的工作区数据: %w", err)
	}
	if ws.Interfaces == nil {
		return errors.New("导入文件缺少 interfaces 字段")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range ws.Interfaces {
		if it.ID == "" {
			it.ID = NewID()
		}
		it.Normalize()
	}
	if ws.Version == 0 {
		ws.Version = 1
	}
	s.ws = &ws
	return s.persist()
}
