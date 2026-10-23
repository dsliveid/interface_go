package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"time"
)

// COFF 目标文件布局常量。
//
// 这套布局是从 rsrc 工具生成的 .syso 逐字节逆向出来的，保持完全一致，
// 以保证 Go 链接器（cmd/link 的 COFF 读取逻辑）能正常接受产物。
const (
	coffHeaderSize   = 20
	coffSectionSize  = 40
	coffSymbolSize   = 18
	coffRelocSize    = 10
	machineAMD64     = 0x8664
	sectionCharRsrc  = 0x40000040 // IMAGE_SCN_CNT_INITIALIZED_DATA | IMAGE_SCN_MEM_READ
	fileCharRsrc     = 0x0104
	relTypeAMD64NB   = 0x0003 // IMAGE_REL_AMD64_ADDR32NB
	symbolClassStatc = 3      // IMAGE_SYM_CLASS_STATIC
)

// buildCOFF 把资源节数据打包成「只含一个 .rsrc 节」的 COFF 目标文件。
//
// dataEntryOffsets 是每个 IMAGE_RESOURCE_DATA_ENTRY 在节内的偏移，每一条都会
// 生成一个 ADDR32NB 重定位。这一步是必须的，理由有二：
//  1. Go 链接器会把「没有任何重定位引用」的数据段当作死数据丢弃，资源节会消失；
//  2. 重定位让链接器把 OffsetToData 从「节内偏移」修正为「PE 中的真实 RVA」。
func buildCOFF(rsrc []byte, dataEntryOffsets []int) []byte {
	data := make([]byte, len(rsrc))
	copy(data, rsrc)
	for len(data)%4 != 0 {
		data = append(data, 0)
	}

	dataPtr := int64(coffHeaderSize + coffSectionSize)
	relocPtr := dataPtr + int64(len(data))
	symPtr := relocPtr + int64(len(dataEntryOffsets)*coffRelocSize)
	total := symPtr + coffSymbolSize + 4 // 符号表 1 项 + 4 字节空字符串表

	out := make([]byte, total)

	// ---- COFF 文件头 ----
	binary.LittleEndian.PutUint16(out[0:], machineAMD64)
	binary.LittleEndian.PutUint16(out[2:], 1) // NumberOfSections
	binary.LittleEndian.PutUint32(out[4:], 0) // TimeDateStamp：保持 0，交给 -fixexe 在 PE 阶段修正
	binary.LittleEndian.PutUint32(out[8:], uint32(symPtr))
	binary.LittleEndian.PutUint32(out[12:], 1) // NumberOfSymbols
	binary.LittleEndian.PutUint16(out[16:], 0) // SizeOfOptionalHeader
	binary.LittleEndian.PutUint16(out[18:], fileCharRsrc)

	// ---- 节头 ----
	sec := out[coffHeaderSize:]
	copy(sec[0:8], ".rsrc\x00\x00\x00")
	binary.LittleEndian.PutUint32(sec[8:], 0) // VirtualSize
	binary.LittleEndian.PutUint32(sec[12:], 0)
	binary.LittleEndian.PutUint32(sec[16:], uint32(len(data))) // SizeOfRawData
	binary.LittleEndian.PutUint32(sec[20:], uint32(dataPtr))   // PointerToRawData
	binary.LittleEndian.PutUint32(sec[24:], uint32(relocPtr))  // PointerToRelocations
	binary.LittleEndian.PutUint32(sec[28:], 0)                 // PointerToLinenumbers
	binary.LittleEndian.PutUint16(sec[32:], uint16(len(dataEntryOffsets)))
	binary.LittleEndian.PutUint16(sec[34:], 0)
	binary.LittleEndian.PutUint32(sec[36:], sectionCharRsrc)

	// ---- 节数据 ----
	copy(out[dataPtr:], data)

	// ---- 重定位项 ----
	for i, off := range dataEntryOffsets {
		o := relocPtr + int64(i*coffRelocSize)
		binary.LittleEndian.PutUint32(out[o:], uint32(off)) // VirtualAddress
		binary.LittleEndian.PutUint32(out[o+4:], 0)         // SymbolTableIndex：指向 .rsrc 节符号
		binary.LittleEndian.PutUint16(out[o+8:], relTypeAMD64NB)
	}

	// ---- 符号表（1 个节符号 + 4 字节空字符串表）----
	sym := out[symPtr:]
	copy(sym[0:8], ".rsrc\x00\x00\x00")
	binary.LittleEndian.PutUint32(sym[8:], 0)  // Value
	binary.LittleEndian.PutUint16(sym[12:], 1) // SectionNumber
	binary.LittleEndian.PutUint16(sym[14:], 0) // Type
	sym[16] = symbolClassStatc
	sym[17] = 0 // NumberOfAuxSymbols

	return out
}

// fixPEHeader 就地修正产物的 PE 头：写入真实构建时间戳并按规范重算校验和。
//
// Go 1.24+ 为了可重现构建会把 TimeDateStamp 置 0、CheckSum 留 0，
// 这让产物看起来像「手工拼装/加壳的 PE」，是杀软启发式的常见减分项。
// 必须在代码签名之前执行，否则签名会失效。
func fixPEHeader(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(b) < 0x40 || b[0] != 'M' || b[1] != 'Z' {
		return fmt.Errorf("%s 不是有效的 MZ 可执行文件", path)
	}
	peOff := int(binary.LittleEndian.Uint32(b[0x3C:]))
	if peOff+24 > len(b) || string(b[peOff:peOff+4]) != "PE\x00\x00" {
		return fmt.Errorf("%s 不是有效的 PE 文件", path)
	}
	coff := peOff + 4
	opt := coff + 20
	if opt+70 > len(b) {
		return fmt.Errorf("%s 的 PE 可选头被截断", path)
	}

	binary.LittleEndian.PutUint32(b[coff+4:], uint32(time.Now().Unix()))
	checksumOff := opt + 64
	binary.LittleEndian.PutUint32(b[checksumOff:], 0)
	binary.LittleEndian.PutUint32(b[checksumOff:], peChecksum(b, checksumOff))

	return os.WriteFile(path, b, 0o644)
}

// peChecksum 按 Windows imagehlp 的 CheckSumMappedFile 算法计算校验和。
// 计算时校验和字段自身视为 0（调用方已先清零）。
func peChecksum(b []byte, checksumOff int) uint32 {
	var sum uint32
	n := len(b)
	for i := 0; i+1 < n; i += 2 {
		if i == checksumOff || i == checksumOff+2 {
			continue
		}
		sum += uint32(binary.LittleEndian.Uint16(b[i:]))
		sum = (sum & 0xffff) + (sum >> 16)
	}
	if n%2 == 1 {
		sum += uint32(b[n-1])
		sum = (sum & 0xffff) + (sum >> 16)
	}
	sum = (sum & 0xffff) + (sum >> 16)
	return sum + uint32(n)
}
