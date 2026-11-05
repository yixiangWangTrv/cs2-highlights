// genicon 生成 cs2hi 应用图标：
//   - cmd/cs2hi/app.ico            多尺寸 Windows 图标（供 go-winres 嵌入 exe）
//   - internal/gui/assets/favicon.svg / favicon.png  网页 favicon
//
// 设计语言与应用一致（iOS 26 液态玻璃）：
// 蓝→靛→紫对角渐变圆角方形底座、顶部玻璃高光、白色播放三角、星芒点缀。
// 纯标准库逐像素渲染（SDF + 平滑边缘），不依赖图像库。
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

// ---------- 颜色 ----------
var (
	cStart  = color.NRGBA{R: 0x0a, G: 0x84, B: 0xff, A: 255} // #0a84ff
	cMid    = color.NRGBA{R: 0x5e, G: 0x5c, B: 0xe6, A: 255} // #5e5ce6
	cEnd    = color.NRGBA{R: 0xbf, G: 0x5a, B: 0xf2, A: 255} // #bf5af2
	cShade  = color.NRGBA{R: 0x18, G: 0x0a, B: 0x3c, A: 255} // 底部玻璃内阴影
	white   = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
)

func mix(a, b color.NRGBA, t float64) color.NRGBA {
	return color.NRGBA{
		R: uint8(float64(a.R)*(1-t) + float64(b.R)*t + 0.5),
		G: uint8(float64(a.G)*(1-t) + float64(b.G)*t + 0.5),
		B: uint8(float64(a.B)*(1-t) + float64(b.B)*t + 0.5),
		A: 255,
	}
}

// clamp01 截断到 [0,1]
func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// render 按指定边长渲染图标（几何参数以 256 为基准等比缩放）。
func render(sz int) *image.NRGBA {
	s := float64(sz) / 256.0
	cx, cy := float64(sz-1)/2, float64(sz-1)/2
	radius := 57.0 * s
	// 播放三角（视觉重心略右于几何中心，符合图标惯例）
	triA := [2]float64{102 * s, 76 * s}
	triB := [2]float64{102 * s, 180 * s}
	triC := [2]float64{188 * s, 128 * s}
	// 星芒中心与半边长
	sp := [2]float64{194 * s, 64 * s}
	sr := 30.0 * s

	img := image.NewNRGBA(image.Rect(0, 0, sz, sz))
	for y := 0; y < sz; y++ {
		for x := 0; x < sz; x++ {
			px, py := float64(x), float64(y)

			// 圆角矩形覆盖（1px 平滑边缘）
			shapeA := roundedRectAlpha(px, py, cx, cy, radius)
			if shapeA == 0 {
				continue
			}

			// 135° 对角渐变：#0a84ff → #5e5ce6 → #bf5af2
			t := (px + py) / (2 * float64(sz-1))
			var base color.NRGBA
			if t < 0.5 {
				base = mix(cStart, cMid, t*2)
			} else {
				base = mix(cMid, cEnd, (t-0.5)*2)
			}

			// 底部玻璃内阴影（深色自下而上淡出，增强玻璃厚度感）
			shadeA := clamp01((py - 170*s) / (86 * s))
			if shadeA > 0 {
				base = mix(base, cShade, shadeA*0.28)
			}

			// 顶部玻璃高光：大椭圆柔和光斑 + 左上小亮点
			e1 := (px-cx)*(px-cx)/(140*s*140*s) + (py-80*s)*(py-80*s)/(58*s*58*s)
			if e1 < 1 {
				base = mix(base, white, 0.20*(1-e1))
			}
			e2 := (px-86*s)*(px-86*s)/(64*s*64*s) + (py-44*s)*(py-44*s)/(26*s*26*s)
			if e2 < 1 {
				base = mix(base, white, 0.13*(1-e2))
			}

			// 星芒（四角星：|dx|^½+|dy|^½ ≤ √r 的凹菱形），白色 90%
			spark := math.Pow(math.Sqrt(math.Abs(px-sp[0]))+math.Sqrt(math.Abs(py-sp[1])), 2) - sr
			if spark < 0.5 {
				base = mix(base, white, 0.90*clamp01(0.5-spark))
			}

			// 白色播放三角（SDF + 1px 平滑边缘）
			tri := triSDF(px, py, triA, triB, triC)
			if tri < 0.5 {
				base = mix(base, white, clamp01(0.5-tri))
			}

			base.A = uint8(shapeA * 255)
			img.SetNRGBA(x, y, base)
		}
	}
	return img
}

// roundedRectAlpha 圆角矩形覆盖率：边缘 1px 平滑过渡（SDF）。
func roundedRectAlpha(px, py, cx, cy, r float64) float64 {
	bx, by := cx-r+0.5, cy-r+0.5 // 内矩形半宽（不含圆角）
	qx, qy := math.Abs(px-cx)-bx, math.Abs(py-cy)-by
	d := math.Hypot(math.Max(qx, 0), math.Max(qy, 0)) + math.Min(math.Max(qx, qy), 0) - r
	return clamp01(0.5 - d)
}

// triSDF 点到三角形的有符号距离（内部为负）。
func triSDF(px, py float64, a, b, c [2]float64) float64 {
	d := math.Inf(1)
	segs := [][2][2]float64{{a, b}, {b, c}, {c, a}}
	for _, s := range segs {
		dx, dy := s[1][0]-s[0][0], s[1][1]-s[0][1]
		l2 := dx*dx + dy*dy
		tt := ((px-s[0][0])*dx + (py-s[0][1])*dy) / l2
		tt = clamp01(tt)
		qx, qy := s[0][0]+tt*dx, s[0][1]+tt*dy
		d = math.Min(d, math.Hypot(px-qx, py-qy))
	}
	if pointInTri(px, py, a, b, c) {
		d = -d
	}
	return d
}

// pointInTri 叉积同号判断点是否在三角形内。
func pointInTri(px, py float64, a, b, c [2]float64) bool {
	sign := func(p, q, r [2]float64) float64 {
		return (q[0]-p[0])*(r[1]-p[1]) - (q[1]-p[1])*(r[0]-p[0])
	}
	d1, d2, d3 := sign(a, b, [2]float64{px, py}), sign(b, c, [2]float64{px, py}), sign(c, a, [2]float64{px, py})
	hasNeg := d1 < 0 || d2 < 0 || d3 < 0
	hasPos := d1 > 0 || d2 > 0 || d3 > 0
	return !(hasNeg && hasPos)
}

// pngBytes 编码 PNG。
func pngBytes(img *image.NRGBA) []byte {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// writeICO 把各尺寸 PNG 打包为 ICO（现代格式：目录 + PNG 数据，256 尺寸字段记 0）。
func writeICO(path string, sizes []int) error {
	var blobs [][]byte
	for _, sz := range sizes {
		blobs = append(blobs, pngBytes(render(sz)))
	}
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, uint16(0))  // reserved
	binary.Write(&buf, binary.LittleEndian, uint16(1))  // type: icon
	binary.Write(&buf, binary.LittleEndian, uint16(len(sizes)))
	offset := 6 + 16*len(sizes)
	for i, sz := range sizes {
		w := uint8(sz)
		if sz >= 256 {
			w = 0
		}
		buf.WriteByte(w)          // width
		buf.WriteByte(w)          // height
		buf.WriteByte(0)          // colors
		buf.WriteByte(0)          // reserved
		binary.Write(&buf, binary.LittleEndian, uint16(1))  // planes
		binary.Write(&buf, binary.LittleEndian, uint16(32)) // bpp
		binary.Write(&buf, binary.LittleEndian, uint32(len(blobs[i])))
		binary.Write(&buf, binary.LittleEndian, uint32(offset))
		offset += len(blobs[i])
	}
	for _, b := range blobs {
		buf.Write(b)
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// faviconSVG 与渲染器同一设计的手写 SVG（浏览器 favicon，随窗口缩放无损）。
const faviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 256 256">
  <defs>
    <linearGradient id="g" x1="0" y1="0" x2="1" y2="1">
      <stop offset="0" stop-color="#0a84ff"/>
      <stop offset="0.5" stop-color="#5e5ce6"/>
      <stop offset="1" stop-color="#bf5af2"/>
    </linearGradient>
    <linearGradient id="shade" x1="0" y1="0" x2="0" y2="1">
      <stop offset="0.7" stop-color="#180a3c" stop-opacity="0"/>
      <stop offset="1" stop-color="#180a3c" stop-opacity="0.28"/>
    </linearGradient>
  </defs>
  <rect width="256" height="256" rx="57" fill="url(#g)"/>
  <rect width="256" height="256" rx="57" fill="url(#shade)"/>
  <ellipse cx="128" cy="80" rx="140" ry="58" fill="#fff" opacity="0.20"/>
  <ellipse cx="86" cy="44" rx="64" ry="26" fill="#fff" opacity="0.13"/>
  <path d="M194 34 Q194 64 224 64 Q194 64 194 94 Q194 64 164 64 Q194 64 194 34 Z" fill="#fff" opacity="0.9"/>
  <path d="M102 76 L102 180 L188 128 Z" fill="#fff"/>
</svg>
`

func main() {
	root := filepath.Join("..", "..") // 从 tools/genicon 回到仓库根
	ico := filepath.Join(root, "cmd", "cs2hi", "app.ico")
	if err := writeICO(ico, []int{256, 128, 64, 48, 32, 24, 16}); err != nil {
		fmt.Println("写 app.ico 失败:", err)
		os.Exit(1)
	}
	fmt.Println("已生成", ico)

	fav := filepath.Join(root, "internal", "gui", "assets", "favicon.svg")
	if err := os.WriteFile(fav, []byte(faviconSVG), 0o644); err != nil {
		fmt.Println("写 favicon.svg 失败:", err)
		os.Exit(1)
	}
	fmt.Println("已生成", fav)

	favpng := filepath.Join(root, "internal", "gui", "assets", "favicon.png")
	if err := os.WriteFile(favpng, pngBytes(render(32)), 0o644); err != nil {
		fmt.Println("写 favicon.png 失败:", err)
		os.Exit(1)
	}
	fmt.Println("已生成", favpng)
}
