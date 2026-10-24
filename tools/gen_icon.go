//go:build ignore

// gen_icon produces the app icons. When a custom source image exists
// (tools/icon.png) it is scaled to every target size; otherwise a default
// book-and-gateway mark is drawn.
package main

import (
	"flag"
	"image"
	"image/color"
	_ "image/png"
	"image/png"
	"math"
	"os"
	"path/filepath"

	"golang.org/x/image/draw"
)

func main() {
	out := flag.String("out", "pkg/mangagate", "package root")
	src := flag.String("src", "tools/icon.png", "custom icon source, used when present")
	flag.Parse()
	targets := []struct {
		path string
		size int
	}{
		{filepath.Join(*out, "ICON.PNG"), 64},
		{filepath.Join(*out, "ICON_256.PNG"), 256},
		{filepath.Join(*out, "app/ui/images/icon_64.png"), 64},
		{filepath.Join(*out, "app/ui/images/icon_256.png"), 256},
	}
	var base image.Image
	if f, err := os.Open(*src); err == nil {
		if img, _, err := image.Decode(f); err == nil {
			base = img
			println("using custom icon:", *src)
		} else {
			println("custom icon unreadable, drawing default:", err.Error())
		}
		f.Close()
	}
	for _, t := range targets {
		if err := os.MkdirAll(filepath.Dir(t.path), 0o755); err != nil {
			panic(err)
		}
		f, err := os.Create(t.path)
		if err != nil {
			panic(err)
		}
		var img image.Image
		if base != nil {
			img = scaleTo(base, t.size)
		} else {
			img = drawIcon(t.size)
		}
		if err := png.Encode(f, img); err != nil {
			panic(err)
		}
		f.Close()
	}
}

// scaleTo resizes the custom icon to the square target size.
func scaleTo(src image.Image, size int) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	return dst
}

func drawIcon(size int) image.Image {
	s := float64(size)
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	bg := color.RGBA{0x1f, 0x3a, 0x6b, 0xff}
	bg2 := color.RGBA{0x2f, 0x6f, 0xb8, 0xff}
	white := color.RGBA{0xf7, 0xf9, 0xfc, 0xff}
	accent := color.RGBA{0x7d, 0xd3, 0xf0, 0xff}

	radius := s * 0.22
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			if !inRounded(fx, fy, 0, 0, s, radius) {
				continue
			}
			t := fy / s
			c := color.RGBA{
				uint8(float64(bg.R) + t*float64(bg2.R-bg.R)),
				uint8(float64(bg.G) + t*float64(bg2.G-bg.G)),
				uint8(float64(bg.B) + t*float64(bg2.B-bg.B)),
				0xff,
			}
			img.Set(x, y, c)
		}
	}

	// Open book: two leaves meeting at the spine.
	left := [][2]float64{{0.18, 0.31}, {0.48, 0.27}, {0.48, 0.72}, {0.18, 0.66}}
	right := [][2]float64{{0.52, 0.27}, {0.82, 0.31}, {0.82, 0.66}, {0.52, 0.72}}
	fillPoly(img, left, white)
	fillPoly(img, right, white)

	// Page lines.
	for i := 0; i < 3; i++ {
		y := 0.38 + float64(i)*0.08
		fillRect(img, 0.23*s, y*s, 0.44*s, y*s+s*0.018, bg2)
		fillRect(img, 0.56*s, y*s, 0.77*s, y*s+s*0.018, bg2)
	}

	// Gateway arcs at the lower right.
	cx, cy := 0.72*s, 0.74*s
	for _, r := range []float64{0.10, 0.16, 0.22} {
		strokeArc(img, cx, cy, r*s, accent)
	}
	fillCircle(img, cx, cy, 0.035*s, accent)

	return img
}

func inRounded(x, y, ox, oy, size, r float64) bool {
	minX, minY := ox, oy
	maxX, maxY := ox+size, oy+size
	if x < minX || x > maxX || y < minY || y > maxY {
		return false
	}
	cx := math.Min(math.Max(x, minX+r), maxX-r)
	cy := math.Min(math.Max(y, minY+r), maxY-r)
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= r*r
}

func fillRect(img *image.RGBA, x0, y0, x1, y1 float64, c color.RGBA) {
	for y := int(y0); y < int(y1); y++ {
		for x := int(x0); x < int(x1); x++ {
			if image.Pt(x, y).In(img.Bounds()) {
				img.Set(x, y, c)
			}
		}
	}
}

func fillCircle(img *image.RGBA, cx, cy, r float64, c color.RGBA) {
	for y := int(cy - r - 1); y <= int(cy+r+1); y++ {
		for x := int(cx - r - 1); x <= int(cx+r+1); x++ {
			dx, dy := float64(x)-cx, float64(y)-cy
			if dx*dx+dy*dy <= r*r && image.Pt(x, y).In(img.Bounds()) {
				img.Set(x, y, c)
			}
		}
	}
}

func strokeArc(img *image.RGBA, cx, cy, r float64, c color.RGBA) {
	thick := math.Max(1.2, r*0.10)
	for y := int(cy - r - thick); y <= int(cy+r+thick); y++ {
		for x := int(cx - r - thick); x <= int(cx+r+thick); x++ {
			dx, dy := float64(x)+0.5-cx, float64(y)+0.5-cy
			d := math.Hypot(dx, dy)
			if math.Abs(d-r) > thick/2 {
				continue
			}
			// Only the upper half of each arc, like a Wi-Fi symbol turned up.
			if dy > -r*0.15 {
				continue
			}
			if image.Pt(x, y).In(img.Bounds()) {
				img.Set(x, y, c)
			}
		}
	}
}

func fillPoly(img *image.RGBA, pts [][2]float64, c color.RGBA) {
	minY, maxY := pts[0][1], pts[0][1]
	minX, maxX := pts[0][0], pts[0][0]
	for _, p := range pts {
		if p[1] < minY { minY = p[1] }
		if p[1] > maxY { maxY = p[1] }
		if p[0] < minX { minX = p[0] }
		if p[0] > maxX { maxX = p[0] }
	}
	b := img.Bounds()
	for y := int(minY); y <= int(maxY); y++ {
		for x := int(minX); x <= int(maxX); x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			if pointInPoly(fx, fy, pts) && image.Pt(x, y).In(b) {
				img.Set(x, y, c)
			}
		}
	}
}

func pointInPoly(x, y float64, pts [][2]float64) bool {
	inside := false
	j := len(pts) - 1
	for i := 0; i < len(pts); i++ {
		xi, yi := pts[i][0], pts[i][1]
		xj, yj := pts[j][0], pts[j][1]
		if (yi > y) != (yj > y) && x < (xj-xi)*(y-yi)/(yj-yi)+xi {
			inside = !inside
		}
		j = i
	}
	return inside
}
