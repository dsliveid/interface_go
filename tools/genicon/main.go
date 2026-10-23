// genicon 生成程序图标 assets/app.ico。
//
// 图标与页面头部品牌标识保持一致：深色面板底 + 绿色菱形（◇，--green #4ec9b0）。
// 输出为多尺寸 ICO：16/24/32/48/64 用 32bpp BMP 条目（兼容所有 Windows 图标 API，
// 托盘图标即取自其中的 32×32 条目），256 用 PNG 条目（Vista+ 支持，减小体积）。
//
// 用法（在项目根目录）：
//
//	go run ./tools/genicon
//
// 随后用 tools/winres 生成 exe 资源对象（图标 + 版本信息 + 应用清单，可重复执行）：
//
//	go run ./tools/winres
//
// 注意：winres 默认跳过 ICO 里的 PNG 压缩条目，避免资源节内出现高熵压缩块
// （256×256 大图标想保留 PNG 条目时加 -keep-png）。
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"os"
	"path/filepath"
)

// 与 web/style.css 中的变量保持一致。
var (
	colorBG    = color.NRGBA{R: 0x25, G: 0x25, B: 0x26, A: 0xff} // --bg-panel
	colorEdge  = color.NRGBA{R: 0x3f, G: 0x3f, B: 0x46, A: 0xff} // --border
	colorGreen = color.NRGBA{R: 0x4e, G: 0xc9, B: 0xb0, A: 0xff} // --green
)

// bmpSizes 使用 32bpp BMP 条目（托盘图标取 32）。
var bmpSizes = []int{16, 24, 32, 48, 64}

// pngSizes 使用 PNG 条目（Windows Vista+ 支持，用于资源管理器大图标）。
var pngSizes = []int{256}

const supersample = 4 // 超采样倍数，用于抗锯齿

func main() {
	out := "assets/app.ico"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}

	var entries []entry
	for _, s := range bmpSizes {
		img := render(s)
		entries = append(entries, entry{size: s, data: encodeBMP(img)})
	}
	for _, s := range pngSizes {
		img := render(s)
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			log.Fatalf("PNG 编码失败：%v", err)
		}
		entries = append(entries, entry{size: s, data: buf.Bytes()})
	}

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		log.Fatalf("创建目录失败：%v", err)
	}
	if err := os.WriteFile(out, buildICO(entries), 0o644); err != nil {
		log.Fatalf("写入 %s 失败：%v", out, err)
	}
	fmt.Printf("已生成 %s（%d 个尺寸：%v BMP + %v PNG）\n", out, len(entries), bmpSizes, pngSizes)
}

type entry struct {
	size int
	data []byte
}

// render 绘制单张图标：深色圆角方块 + 绿色空心菱形。
func render(size int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	ss := size * supersample

	// 以超采样分辨率计算覆盖率后降采样，得到平滑边缘。
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var bgHit, edgeHit, diaHit int
			for sy := 0; sy < supersample; sy++ {
				for sx := 0; sx < supersample; sx++ {
					// 采样点取子像素中心，归一化到 [0,1]
					px := (float64(x*supersample+sx) + 0.5) / float64(ss)
					py := (float64(y*supersample+sy) + 0.5) / float64(ss)
					if insideRoundedRect(px, py, 0.18) {
						bgHit++
					} else if insideRoundedRect(px, py, 0.18, -0.02) {
						// 外圈描边（略微外扩，形成 1px 边框感）
						edgeHit++
					}
					if insideDiamondRing(px, py, 0.34, 0.10) {
						diaHit++
					}
				}
			}
			total := float64(supersample * supersample)
			var c color.NRGBA
			switch {
			case diaHit > 0:
				c = blend(colorGreen, blend(colorBG, transparent(), float64(edgeHit)/total), float64(diaHit)/total)
			case bgHit > 0:
				c = colorBG
			case edgeHit > 0:
				c = colorEdge
			default:
				c = transparent()
			}
			if bgHit == 0 && edgeHit == 0 && diaHit == 0 {
				c = transparent()
			}
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

func transparent() color.NRGBA { return color.NRGBA{} }

// blend 以 t 的比例把 top 叠加到 base 上（仅处理 alpha 覆盖，够用即可）。
func blend(top, base color.NRGBA, t float64) color.NRGBA {
	if t <= 0 {
		return base
	}
	if t >= 1 {
		return top
	}
	mix := func(a, b uint8) uint8 { return uint8(float64(a)*t + float64(b)*(1-t) + 0.5) }
	return color.NRGBA{R: mix(top.R, base.R), G: mix(top.G, base.G), B: mix(top.B, base.B), A: mix(top.A, base.A)}
}

// insideRoundedRect 判断归一化坐标是否落在圆角方块内；inset 为负表示向外扩张。
func insideRoundedRect(x, y, radius float64, inset ...float64) bool {
	pad := 0.0
	if len(inset) > 0 {
		pad = inset[0]
	}
	x0, y0 := pad, pad
	x1, y1 := 1-pad, 1-pad
	r := radius
	if x < x0 || x > x1 || y < y0 || y > y1 {
		return false
	}
	cx := clamp(x, x0+r, x1-r)
	cy := clamp(y, y0+r, y1-r)
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= r*r
}

// insideDiamondRing 判断是否落在菱形描边环内：|dx|/r + |dy|/r 距 1 不超过 halfWidth。
func insideDiamondRing(x, y, radius, halfWidth float64) bool {
	dx := (x - 0.5) / radius
	dy := (y - 0.5) / radius
	d := abs(dx) + abs(dy)
	return abs(d-1) <= halfWidth
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// encodeBMP 把图像编码为 ICO 内嵌的 32bpp DIB：BITMAPINFOHEADER + BGRA 像素（自下而上）+ AND 掩码。
func encodeBMP(img *image.NRGBA) []byte {
	w := img.Bounds().Dx()
	h := img.Bounds().Dy()
	maskStride := ((w + 31) / 32) * 4 // 1bpp 行按 4 字节对齐

	var buf bytes.Buffer
	// BITMAPINFOHEADER
	header := make([]byte, 40)
	binary.LittleEndian.PutUint32(header[0:], 40)              // biSize
	binary.LittleEndian.PutUint32(header[4:], uint32(w))       // biWidth
	binary.LittleEndian.PutUint32(header[8:], uint32(h*2))     // biHeight（XOR + AND 两张图）
	binary.LittleEndian.PutUint16(header[12:], 1)              // biPlanes
	binary.LittleEndian.PutUint16(header[14:], 32)             // biBitCount
	binary.LittleEndian.PutUint32(header[16:], 0)              // biCompression = BI_RGB
	binary.LittleEndian.PutUint32(header[20:], uint32(w*h*4))  // biSizeImage
	buf.Write(header)

	// XOR 位图：BGRA，自下而上
	for y := h - 1; y >= 0; y-- {
		for x := 0; x < w; x++ {
			c := img.NRGBAAt(x, y)
			buf.Write([]byte{c.B, c.G, c.R, c.A})
		}
	}

	// AND 掩码：32bpp 下由 alpha 决定透明度，这里全部置 0
	buf.Write(make([]byte, maskStride*h))
	return buf.Bytes()
}

// buildICO 组装 ICO 文件（ICONDIR + ICONDIRENTRY 列表 + 各图像数据）。
func buildICO(entries []entry) []byte {
	var buf bytes.Buffer
	// ICONDIR
	binary.Write(&buf, binary.LittleEndian, uint16(0)) // reserved
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // type = icon
	binary.Write(&buf, binary.LittleEndian, uint16(len(entries)))

	offset := 6 + 16*len(entries)
	for _, e := range entries {
		dim := byte(e.size)
		if e.size >= 256 {
			dim = 0 // 256 在 ICO 目录中用 0 表示
		}
		buf.WriteByte(dim)              // bWidth
		buf.WriteByte(dim)              // bHeight
		buf.WriteByte(0)                // bColorCount
		buf.WriteByte(0)                // bReserved
		binary.Write(&buf, binary.LittleEndian, uint16(1))            // wPlanes
		binary.Write(&buf, binary.LittleEndian, uint16(32))           // wBitCount
		binary.Write(&buf, binary.LittleEndian, uint32(len(e.data)))  // dwBytesInRes
		binary.Write(&buf, binary.LittleEndian, uint32(offset))       // dwImageOffset
		offset += len(e.data)
	}
	for _, e := range entries {
		buf.Write(e.data)
	}
	return buf.Bytes()
}
