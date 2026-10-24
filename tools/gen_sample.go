//go:build ignore

// gen_sample builds a small comic library for local testing.
package main

import (
	"archive/zip"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"log"
	"os"
	"path/filepath"
)

type pageSpec struct {
	name  string
	pages int
	hue   color.RGBA
}

func main() {
	out := flag.String("out", "testdata/library", "output library directory")
	flag.Parse()

	series := []struct {
		dir   string
		books []pageSpec
	}{
		{"星海旅人", []pageSpec{{"第1卷", 8, color.RGBA{38, 70, 120, 255}}, {"第2卷", 7, color.RGBA{30, 96, 110, 255}}}},
		{"日常小品", []pageSpec{{"第1话", 6, color.RGBA{120, 60, 90, 255}}, {"第2话", 5, color.RGBA{110, 84, 50, 255}}}},
		{"单本测试", []pageSpec{{"独立的一本", 9, color.RGBA{60, 100, 70, 255}}}},
	}

	for _, s := range series {
		dir := filepath.Join(*out, s.dir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatal(err)
		}
		for _, b := range s.books {
			path := filepath.Join(dir, b.name+".cbz")
			if err := writeCBZ(path, b); err != nil {
				log.Fatalf("%s: %v", path, err)
			}
			fmt.Println("created", path)
		}
	}

	// An image-only book exercises the folder/loose-image path.
	imgDir := filepath.Join(*out, "图片样式", "散图漫画")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		log.Fatal(err)
	}
	for i := 1; i <= 6; i++ {
		path := filepath.Join(imgDir, fmt.Sprintf("%02d.jpg", i))
		if err := writeJPEG(path, i, color.RGBA{70, 50, 110, 255}); err != nil {
			log.Fatal(err)
		}
	}
	fmt.Println("created image folder", imgDir)

	// A top-level loose image folder.
	loose := filepath.Join(*out, "零散图集")
	if err := os.MkdirAll(loose, 0o755); err != nil {
		log.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		if err := writeJPEG(filepath.Join(loose, fmt.Sprintf("%02d.jpg", i)), i, color.RGBA{100, 90, 40, 255}); err != nil {
			log.Fatal(err)
		}
	}
	fmt.Println("created loose folder", loose)

	// Nested categories exercise directory-subtree permission grants.
	nested := []struct {
		dir   string
		books []pageSpec
	}{
		{"混合集/悬疑", []pageSpec{{"第一案", 5, color.RGBA{90, 40, 60, 255}}}},
		{"混合集/异世界", []pageSpec{{"冒险记", 4, color.RGBA{40, 80, 120, 255}}}},
	}
	for _, s := range nested {
		dir := filepath.Join(*out, s.dir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatal(err)
		}
		for _, b := range s.books {
			path := filepath.Join(dir, b.name+".cbz")
			if err := writeCBZ(path, b); err != nil {
				log.Fatalf("%s: %v", path, err)
			}
			fmt.Println("created", path)
		}
	}

	// An epub archive exercises the epub format path.
	epubDir := filepath.Join(*out, "电子书")
	if err := os.MkdirAll(epubDir, 0o755); err != nil {
		log.Fatal(err)
	}
	epubPath := filepath.Join(epubDir, "试读本.epub")
	if err := writeEPUB(epubPath, 4, color.RGBA{50, 110, 90, 255}); err != nil {
		log.Fatalf("%s: %v", epubPath, err)
	}
	fmt.Println("created", epubPath)
}

func writeEPUB(path string, pages int, hue color.RGBA) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	// The mimetype entry must be first and stored uncompressed.
	mw, err := zw.CreateHeader(&zip.FileHeader{Name: "mimetype", Method: zip.Store})
	if err != nil {
		return err
	}
	if _, err := mw.Write([]byte("application/epub+zip")); err != nil {
		return err
	}
	cw, err := zw.Create("META-INF/container.xml")
	if err != nil {
		return err
	}
	if _, err := cw.Write([]byte(`<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`)); err != nil {
		return err
	}
	// Entry names carry a space so the URL-escaped href in the manifest is
	// exercised too.
	for i := 1; i <= pages; i++ {
		w, err := zw.Create(fmt.Sprintf("OEBPS/images/p %d.jpg", i))
		if err != nil {
			return err
		}
		if err := jpeg.Encode(w, drawPage(i, pages, hue), &jpeg.Options{Quality: 85}); err != nil {
			return err
		}
	}
	opf, err := zw.Create("OEBPS/content.opf")
	if err != nil {
		return err
	}
	_, err = opf.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" xmlns:dc="http://purl.org/dc/elements/1.1/" version="3.0" unique-identifier="id">
  <metadata>
    <dc:title>试读本 第一卷</dc:title>
    <dc:creator>作者·测试</dc:creator>
    <dc:description>&lt;p&gt;一本用于测试内嵌元数据的电子书。&lt;/p&gt;</dc:description>
    <dc:subject>测试</dc:subject>
    <dc:language>zh</dc:language>
    <dc:identifier id="id">mangagate-sample-001</dc:identifier>
  </metadata>
  <manifest>
    <item id="cover" href="images/p%201.jpg" media-type="image/jpeg" properties="cover-image"/>
  </manifest>
  <spine/>
</package>`))
	if err != nil {
		return err
	}
	return zw.Close()
}

func writeCBZ(path string, spec pageSpec) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for i := 1; i <= spec.pages; i++ {
		w, err := zw.Create(fmt.Sprintf("%02d.jpg", i))
		if err != nil {
			return err
		}
		img := drawPage(i, spec.pages, spec.hue)
		if err := jpeg.Encode(w, img, &jpeg.Options{Quality: 85}); err != nil {
			return err
		}
	}
	return zw.Close()
}

func writeJPEG(path string, index int, hue color.RGBA) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return jpeg.Encode(f, drawPage(index, 6, hue), &jpeg.Options{Quality: 85})
}

func drawPage(index, total int, hue color.RGBA) image.Image {
	const W, H = 720, 1080
	img := image.NewRGBA(image.Rect(0, 0, W, H))
	for y := 0; y < H; y++ {
		t := float64(y) / float64(H)
		c := color.RGBA{
			uint8(float64(hue.R) * (0.45 + 0.55*t)),
			uint8(float64(hue.G) * (0.45 + 0.55*t)),
			uint8(float64(hue.B) * (0.45 + 0.55*t)),
			255,
		}
		for x := 0; x < W; x++ {
			img.Set(x, y, c)
		}
	}
	// Paper-like inset
	paper := image.Rect(60, 70, W-60, H-70)
	draw.Draw(img, paper, image.NewUniform(color.RGBA{245, 243, 238, 255}), image.Point{}, draw.Src)
	// Simple page indicator: one block per page number.
	block := 46
	for n := 0; n < index && n < 12; n++ {
		x := 110 + (n%6)*(block+18)
		y := 180 + (n/6)*(block+18)
		draw.Draw(img, image.Rect(x, y, x+block, y+block), image.NewUniform(hue), image.Point{}, draw.Src)
	}
	// Progress bar
	draw.Draw(img, image.Rect(80, H-110, W-80, H-92), image.NewUniform(color.RGBA{210, 208, 202, 255}), image.Point{}, draw.Src)
	filled := int(float64(W-160) * float64(index) / float64(total))
	draw.Draw(img, image.Rect(80, H-110, 80+filled, H-92), image.NewUniform(hue), image.Point{}, draw.Src)
	return img
}
