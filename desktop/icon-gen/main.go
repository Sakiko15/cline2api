// 生成 Cline Go Proxy 桌面端图标：渐变背景 + 白色 "C" 形。
// 输出 resource_windows_amd64.syso，放在项目根目录后 go build 会自动链接。
package main

import (
	"image"
	"image/color"
	"math"
	"os"

	"github.com/tc-hib/winres"
)

func main() {
	const size = 256
	img := drawIcon(size)

	// 从单张图生成多尺寸图标（256/48/32/16）
	icon, err := winres.NewIconFromResizedImage(img, []int{256, 48, 32, 16})
	if err != nil {
		panic(err)
	}

	rs := &winres.ResourceSet{}
	// 资源 ID = 3，匹配 Wails 的 winc.AppIconID
	if err := rs.SetIcon(winres.ID(3), icon); err != nil {
		panic(err)
	}

	out := "resource_windows_amd64.syso"
	f, err := os.Create(out)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if err := rs.WriteObject(f, winres.ArchAMD64); err != nil {
		panic(err)
	}
	println("wrote", out)
}

// drawIcon 绘制 256×256 图标：圆角方形渐变背景 + 白色 "C" 环。
func drawIcon(size int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	cx, cy := float64(size)/2, float64(size)/2

	// 渐变色：左上 indigo → 右下 purple
	top := [3]float64{79, 70, 229}   // #4F46E5
	bot := [3]float64{124, 58, 237}  // #7C3AED
	white := [4]uint8{255, 255, 255, 255}

	// "C" 环参数
	outerR := float64(size) * 0.36
	innerR := float64(size) * 0.22
	gap := 1.3 // 弧度（约 75°），缺口朝右

	// 圆角半径
	corner := float64(size) * 0.18

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			px, py := float64(x), float64(y)

			// 渐变背景
			t := (px + py) / float64(2*size)
			r := uint8(top[0]*(1-t) + bot[0]*t)
			g := uint8(top[1]*(1-t) + bot[1]*t)
			b := uint8(top[2]*(1-t) + bot[2]*t)

			// 圆角判断：四个角透明
			if !inRoundedRect(px, py, float64(size), corner) {
				img.Set(x, y, color.RGBA{0, 0, 0, 0})
				continue
			}

			// "C" 环判断
			dx := px - cx
			dy := py - cy
			dist := math.Sqrt(dx*dx + dy*dy)
			angle := math.Atan2(dy, dx)

			inRing := dist >= innerR && dist <= outerR
			inGap := angle > -gap/2 && angle < gap/2

			if inRing && !inGap {
				img.Set(x, y, color.RGBA{white[0], white[1], white[2], white[3]})
			} else {
				img.Set(x, y, color.RGBA{r, g, b, 255})
			}
		}
	}
	return img
}

// inRoundedRect 判断点是否在圆角矩形内。
func inRoundedRect(x, y, size, r float64) bool {
	if x < r && y < r {
		return dist2(x, y, r, r) <= r*r
	}
	if x > size-r && y < r {
		return dist2(x, y, size-r, r) <= r*r
	}
	if x < r && y > size-r {
		return dist2(x, y, r, size-r) <= r*r
	}
	if x > size-r && y > size-r {
		return dist2(x, y, size-r, size-r) <= r*r
	}
	return true
}

func dist2(x1, y1, x2, y2 float64) float64 {
	dx := x1 - x2
	dy := y1 - y2
	return dx*dx + dy*dy
}
