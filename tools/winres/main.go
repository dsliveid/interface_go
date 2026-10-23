// winres 生成 Windows 资源对象（.syso），并修正产物的 PE 头。
//
// 为什么需要它：Go 本身不能直接嵌入 Windows 资源。此前用外部工具 rsrc 只嵌了图标，
// 产物 `.rsrc` 里「有图标、却没有任何版本信息与清单」，这个形态与「用换皮工具给样本
// 硬塞图标」高度重合，容易被杀软启发式归入 `VirTool/Obfuscator`（工具/混淆器类）。
// 本工具一次性生成标准四件套（ICON + GROUP_ICON + VERSIONINFO + MANIFEST），
// 让资源节回到「正常构建流程产物」的形态；同时默认跳过 ICO 里的 PNG 压缩条目，
// 避免资源节内出现高熵压缩块（恶意样本藏载荷的常见手法）。
//
// 用法（在项目根目录）：
//
//	go run ./tools/winres                        # 由 assets/app.ico 生成 rsrc_windows_amd64.syso
//	go run ./tools/winres -version 1.2.0         # 指定注入到 VERSIONINFO 的版本号
//	go run ./tools/winres -keep-png              # 保留 ICO 中的 PNG 压缩条目（256×256 大图标更清晰）
//	go run ./tools/winres -fixexe 接口工具.exe    # 修正 PE 时间戳与校验和（须在签名前执行）
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// PE 资源类型 ID。
const (
	resTypeIcon      = 3  // RT_ICON
	resTypeGroupIcon = 14 // RT_GROUP_ICON
	resTypeVersion   = 16 // RT_VERSION
	resTypeManifest  = 24 // RT_MANIFEST

	// 资源语言 ID：0x0409 = en-US，配合 StringTable 的 "040904B0"。
	resLang = 0x0409
)

func main() {
	var (
		icoPath   = flag.String("ico", "assets/app.ico", "输入图标文件路径")
		outPath   = flag.String("out", "rsrc_windows_amd64.syso", "输出资源对象（.syso）路径")
		versionID = flag.String("version", "1.0.0", "注入 VERSIONINFO 的版本号")
		keepPNG   = flag.Bool("keep-png", false, "保留 ICO 中的 PNG 压缩条目（默认跳过）")
		fixexe    = flag.String("fixexe", "", "就地修正指定 exe 的 PE 时间戳与校验和")
		company   = flag.String("company", "接口工具", "VERSIONINFO 的 CompanyName")
		product   = flag.String("product", "接口工具", "VERSIONINFO 的 ProductName")
	)
	flag.Parse()

	if *fixexe != "" {
		if err := fixPEHeader(*fixexe); err != nil {
			log.Fatalf("[winres] 修正 PE 头失败：%v", err)
		}
		log.Printf("[winres] 已修正 PE 头：%s（写入真实时间戳并重算校验和）", *fixexe)
		return
	}

	raw, err := os.ReadFile(*icoPath)
	if err != nil {
		log.Fatalf("[winres] 读取图标失败：%v", err)
	}
	all, err := parseICO(raw)
	if err != nil {
		log.Fatalf("[winres] 解析图标失败：%v", err)
	}

	kept := make([]icoEntry, 0, len(all))
	skipped := 0
	for _, e := range all {
		if !*keepPNG && e.isPNG(raw) {
			skipped++
			continue
		}
		kept = append(kept, e)
	}
	if len(kept) == 0 {
		log.Fatalf("[winres] 图标内没有可用的位图条目（全部是 PNG？可加 -keep-png 重试）")
	}

	// ICON 资源 id 从 1 起连续编号，与 rc.exe 产物形态一致。
	iconType := &resTypeNode{id: resTypeIcon}
	groupEntries := make([]icoEntry, 0, len(kept))
	groupIDs := make([]int, 0, len(kept))
	for i, e := range kept {
		id := i + 1
		iconType.names = append(iconType.names, &resNameNode{
			id:    uint32(id),
			langs: []*resLangEntry{{lang: resLang, data: raw[e.offset : e.offset+e.size]}},
		})
		groupEntries = append(groupEntries, e)
		groupIDs = append(groupIDs, id)
	}

	fields := versionFields{
		company:     *company,
		description: "接口工具 - 本地接口管理与测试工具",
		product:     *product,
		internal:    "interface-go",
		original:    "接口工具.exe",
		copyright:   "Copyright (C) 2026 " + *company,
		version:     *versionID,
	}

	types := []*resTypeNode{
		iconType,
		dataType(resTypeGroupIcon, buildGroupIcon(groupEntries, groupIDs)),
		dataType(resTypeVersion, buildVersionInfo(fields)),
		dataType(resTypeManifest, buildManifest(fields.version)),
	}

	section, entryOffsets := buildResourceSection(types)
	obj := buildCOFF(section, entryOffsets)
	if err := os.WriteFile(*outPath, obj, 0o644); err != nil {
		log.Fatalf("[winres] 写入 %s 失败：%v", *outPath, err)
	}

	sizes := make([]string, 0, len(kept))
	for _, e := range kept {
		sizes = append(sizes, strconv.Itoa(e.dim()))
	}
	log.Printf("[winres] 已生成 %s：%d 字节，图标条目 %d 个（%s），PNG 条目跳过 %d 个",
		*outPath, len(obj), len(kept), strings.Join(sizes, "/"), skipped)
	log.Printf("[winres] 资源：ICON ×%d + GROUP_ICON + VERSIONINFO + MANIFEST（%d 条重定位）",
		len(kept), len(entryOffsets))
}

// dataType 构造「单一资源、单一语言」的资源类型节点。
func dataType(typeID uint32, data []byte) *resTypeNode {
	return &resTypeNode{
		id: typeID,
		names: []*resNameNode{{
			id:    1,
			langs: []*resLangEntry{{lang: resLang, data: data}},
		}},
	}
}

// ---------------- ICO 解析 ----------------

type icoEntry struct {
	width, height int
	colorCount    int
	planes        uint16
	bitCount      uint16
	size          uint32
	offset        uint32
}

var pngSignature = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}

func parseICO(b []byte) ([]icoEntry, error) {
	if len(b) < 6 || binary.LittleEndian.Uint16(b[0:]) != 0 || binary.LittleEndian.Uint16(b[2:]) != 1 {
		return nil, fmt.Errorf("不是有效的 ICO 文件（缺 ICONDIR 头）")
	}
	count := int(binary.LittleEndian.Uint16(b[4:]))
	if count == 0 {
		return nil, fmt.Errorf("ICO 内没有任何图像条目")
	}
	if 6+16*count > len(b) {
		return nil, fmt.Errorf("ICO 目录被截断（声明 %d 个条目）", count)
	}
	entries := make([]icoEntry, 0, count)
	for i := 0; i < count; i++ {
		o := 6 + 16*i
		e := icoEntry{
			width:      int(b[o]),
			height:     int(b[o+1]),
			colorCount: int(b[o+2]),
			planes:     binary.LittleEndian.Uint16(b[o+4:]),
			bitCount:   binary.LittleEndian.Uint16(b[o+6:]),
			size:       binary.LittleEndian.Uint32(b[o+8:]),
			offset:     binary.LittleEndian.Uint32(b[o+12:]),
		}
		if e.size == 0 || uint64(e.offset)+uint64(e.size) > uint64(len(b)) {
			return nil, fmt.Errorf("ICO 第 %d 个条目数据越界", i)
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// isPNG 判断该条目是否为 PNG 压缩格式（ICO 中 256×256 常用 PNG 编码）。
func (e icoEntry) isPNG(b []byte) bool {
	if int(e.offset)+len(pngSignature) > len(b) {
		return false
	}
	return bytes.Equal(b[e.offset:e.offset+uint32(len(pngSignature))], pngSignature)
}

// dim 返回条目的实际边长（ICO 中用 0 表示 256）。
func (e icoEntry) dim() int {
	if e.width == 0 {
		return 256
	}
	return e.width
}

// buildGroupIcon 生成 RT_GROUP_ICON 数据（GRPICONDIR + GRPICONDIRENTRY 列表）。
func buildGroupIcon(entries []icoEntry, ids []int) []byte {
	out := make([]byte, 6+14*len(entries))
	binary.LittleEndian.PutUint16(out[0:], 0) // idReserved
	binary.LittleEndian.PutUint16(out[2:], 1) // idType = 1（图标）
	binary.LittleEndian.PutUint16(out[4:], uint16(len(entries)))
	for i, e := range entries {
		o := 6 + 14*i
		out[o] = byte(e.width)
		out[o+1] = byte(e.height)
		out[o+2] = byte(e.colorCount)
		out[o+3] = 0
		binary.LittleEndian.PutUint16(out[o+4:], e.planes)
		binary.LittleEndian.PutUint16(out[o+6:], e.bitCount)
		binary.LittleEndian.PutUint32(out[o+8:], e.size)
		binary.LittleEndian.PutUint16(out[o+12:], uint16(ids[i]))
	}
	return out
}

// ---------------- 资源目录树 ----------------

type resLangEntry struct {
	lang     uint32
	data     []byte
	dataOff  int // 数据在节内的偏移
	entryOff int // IMAGE_RESOURCE_DATA_ENTRY 在节内的偏移
}

type resNameNode struct {
	id     uint32
	langs  []*resLangEntry
	dirOff int
}

type resTypeNode struct {
	id     uint32
	names  []*resNameNode
	dirOff int
}

type dirEntry struct {
	id    uint32
	isDir bool
	off   int
}

// buildResourceSection 把资源树序列化为 PE 标准的 .rsrc 节数据，
// 同时返回每个 IMAGE_RESOURCE_DATA_ENTRY 的偏移（用于生成重定位）。
//
// 布局顺序：根目录 → 各类型目录 → 各名称目录 → 数据描述项区 → 数据区。
func buildResourceSection(types []*resTypeNode) ([]byte, []int) {
	sort.Slice(types, func(i, j int) bool { return types[i].id < types[j].id })
	for _, t := range types {
		sort.Slice(t.names, func(i, j int) bool { return t.names[i].id < t.names[j].id })
		for _, n := range t.names {
			sort.Slice(n.langs, func(i, j int) bool { return n.langs[i].lang < n.langs[j].lang })
		}
	}

	// 目录区
	cur := 16 + 8*len(types)
	for _, t := range types {
		t.dirOff = cur
		cur += 16 + 8*len(t.names)
	}
	for _, t := range types {
		for _, n := range t.names {
			n.dirOff = cur
			cur += 16 + 8*len(n.langs)
		}
	}

	// 数据描述项区（4 字节对齐）
	cur = align4(cur)
	var entryOffsets []int
	for _, t := range types {
		for _, n := range t.names {
			for _, l := range n.langs {
				l.entryOff = cur
				cur += 16
				entryOffsets = append(entryOffsets, l.entryOff)
			}
		}
	}

	// 数据区
	for _, t := range types {
		for _, n := range t.names {
			for _, l := range n.langs {
				cur = align4(cur)
				l.dataOff = cur
				cur += len(l.data)
			}
		}
	}

	buf := make([]byte, align4(cur))

	root := make([]dirEntry, 0, len(types))
	for _, t := range types {
		root = append(root, dirEntry{id: t.id, isDir: true, off: t.dirOff})
	}
	writeDir(buf, 0, root)

	for _, t := range types {
		entries := make([]dirEntry, 0, len(t.names))
		for _, n := range t.names {
			entries = append(entries, dirEntry{id: n.id, isDir: true, off: n.dirOff})
		}
		writeDir(buf, t.dirOff, entries)
	}

	for _, t := range types {
		for _, n := range t.names {
			entries := make([]dirEntry, 0, len(n.langs))
			for _, l := range n.langs {
				// 语言层直接指向数据描述项，不带「子目录」高位
				entries = append(entries, dirEntry{id: l.lang, isDir: false, off: l.entryOff})
			}
			writeDir(buf, n.dirOff, entries)
		}
	}

	for _, t := range types {
		for _, n := range t.names {
			for _, l := range n.langs {
				o := l.entryOff
				binary.LittleEndian.PutUint32(buf[o:], uint32(l.dataOff)) // OffsetToData：链接期由重定位修正为 RVA
				binary.LittleEndian.PutUint32(buf[o+4:], uint32(len(l.data)))
				binary.LittleEndian.PutUint32(buf[o+8:], 0)  // CodePage
				binary.LittleEndian.PutUint32(buf[o+12:], 0) // Reserved
			}
		}
	}

	for _, t := range types {
		for _, n := range t.names {
			for _, l := range n.langs {
				copy(buf[l.dataOff:], l.data)
			}
		}
	}

	return buf, entryOffsets
}

func writeDir(buf []byte, off int, entries []dirEntry) {
	binary.LittleEndian.PutUint16(buf[off+12:], 0) // NumberOfNamedEntries
	binary.LittleEndian.PutUint16(buf[off+14:], uint16(len(entries)))
	for i, e := range entries {
		o := off + 16 + 8*i
		binary.LittleEndian.PutUint32(buf[o:], e.id)
		v := uint32(e.off)
		if e.isDir {
			v |= 0x80000000
		}
		binary.LittleEndian.PutUint32(buf[o+4:], v)
	}
}

func align4(n int) int { return (n + 3) &^ 3 }

// ---------------- VERSIONINFO ----------------

type versionFields struct {
	company     string
	description string
	product     string
	internal    string
	original    string
	copyright   string
	version     string
}

// fourParts 把 "1.2.3+abc-dirty" 之类的版本串拆成 4 段数字。
func (v versionFields) fourParts() [4]uint16 {
	parts := strings.Split(strings.TrimPrefix(v.version, "v"), ".")
	var out [4]uint16
	for i := 0; i < len(parts) && i < 4; i++ {
		out[i] = uint16(leadingDigits(parts[i]))
	}
	return out
}

func leadingDigits(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			break
		}
		n = n*10 + int(s[i]-'0')
	}
	return n
}

// buildVersionInfo 生成完整的 VS_VERSIONINFO 二进制结构。
func buildVersionInfo(v versionFields) []byte {
	nums := v.fourParts()
	verStr := fmt.Sprintf("%d.%d.%d.%d", nums[0], nums[1], nums[2], nums[3])

	var fixed bytes.Buffer
	put := func(x uint32) {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], x)
		fixed.Write(b[:])
	}
	fileMS := uint32(nums[0])<<16 | uint32(nums[1])
	fileLS := uint32(nums[2])<<16 | uint32(nums[3])
	put(0xFEEF04BD) // dwSignature
	put(0x00010000) // dwStrucVersion
	put(fileMS)     // dwFileVersionMS
	put(fileLS)     // dwFileVersionLS
	put(fileMS)     // dwProductVersionMS
	put(fileLS)     // dwProductVersionLS
	put(0x3F)       // dwFileFlagsMask
	put(0)          // dwFileFlags
	put(0x00040004) // dwFileOS = VOS_NT_WINDOWS32
	put(0x00000001) // dwFileType = VFT_APP
	put(0)          // dwFileSubtype
	put(0)          // dwFileDateMS
	put(0)          // dwFileDateLS

	pairs := []struct{ key, val string }{
		{"CompanyName", v.company},
		{"FileDescription", v.description},
		{"FileVersion", verStr},
		{"InternalName", v.internal},
		{"LegalCopyright", v.copyright},
		{"OriginalFilename", v.original},
		{"ProductName", v.product},
		{"ProductVersion", verStr},
	}

	var table bytes.Buffer
	for _, p := range pairs {
		// 值必须以 UTF-16 的 NUL 结尾，且 wValueLength 计入该 NUL；
		// 少了这个终止符，VerQueryValue 会把下一个块的头字节当成字符串继续读。
		value := append(utf16Bytes(p.val), 0, 0)
		table.Write(versionBlock(p.key, value, uint16(len([]rune(p.val))+1), 1))
	}

	var varValue bytes.Buffer
	var trans [4]byte
	binary.LittleEndian.PutUint32(trans[:], 0x04B00409) // 低 16 位 langID=0x0409，高 16 位 codepage=0x04B0
	varValue.Write(trans[:])

	var root bytes.Buffer
	root.Write(fixed.Bytes())
	padTo4(&root)
	root.Write(versionBlock("StringFileInfo",
		versionBlock("040904B0", table.Bytes(), 0, 1), 0, 1))
	root.Write(versionBlock("VarFileInfo",
		versionBlock("Translation", varValue.Bytes(), 4, 0), 0, 1))

	return versionBlock("VS_VERSION_INFO", root.Bytes(), 52, 0)
}

// versionBlock 生成一个 VS_VERSIONINFO 节点：
// wLength / wValueLength / wType / szKey(UTF-16 + NUL) / 4 字节对齐 / 值 / 4 字节对齐。
func versionBlock(key string, value []byte, valueLen, valueType uint16) []byte {
	var b bytes.Buffer
	b.Write(make([]byte, 6))
	b.Write(utf16Bytes(key))
	b.Write([]byte{0, 0})
	padTo4(&b)
	b.Write(value)
	padTo4(&b)

	out := b.Bytes()
	binary.LittleEndian.PutUint16(out[0:], uint16(len(out)))
	binary.LittleEndian.PutUint16(out[2:], valueLen)
	binary.LittleEndian.PutUint16(out[4:], valueType)
	return out
}

func utf16Bytes(s string) []byte {
	u := utf16.Encode([]rune(s))
	buf := make([]byte, len(u)*2)
	for i, r := range u {
		binary.LittleEndian.PutUint16(buf[i*2:], r)
	}
	return buf
}

func padTo4(b *bytes.Buffer) {
	for b.Len()%4 != 0 {
		b.WriteByte(0)
	}
}

// ---------------- MANIFEST ----------------

// buildManifest 生成应用清单：以普通权限运行 + 声明支持的系统版本 + DPI 感知。
// 注意 XML 必须严格合法，否则程序会因 side-by-side 配置错误而无法启动。
func buildManifest(version string) []byte {
	nums := (versionFields{version: version}).fourParts()
	verStr := fmt.Sprintf("%d.%d.%d.%d", nums[0], nums[1], nums[2], nums[3])
	const tpl = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">
  <assemblyIdentity type="win32" name="InterfaceGo.InterfaceTool" version="%s" processorArchitecture="amd64"/>
  <description>InterfaceGo</description>
  <trustInfo xmlns="urn:schemas-microsoft-com:asm.v3">
    <security>
      <requestedPrivileges>
        <requestedExecutionLevel level="asInvoker" uiAccess="false"/>
      </requestedPrivileges>
    </security>
  </trustInfo>
  <compatibility xmlns="urn:schemas-microsoft-com:compatibility.v1">
    <application>
      <supportedOS Id="{8e0f7a12-bfb3-4fe8-b9a5-48fd50a15a9a}"/>
      <supportedOS Id="{1f676c76-80e1-4239-95bb-83d0f6d0da78}"/>
      <supportedOS Id="{35138b9a-5d96-4fbd-8e2d-a2440225f93a}"/>
    </application>
  </compatibility>
  <application xmlns="urn:schemas-microsoft-com:asm.v3">
    <windowsSettings>
      <dpiAware xmlns="http://schemas.microsoft.com/SMI/2005/WindowsSettings">true</dpiAware>
    </windowsSettings>
  </application>
</assembly>
`
	return []byte(fmt.Sprintf(tpl, verStr))
}
