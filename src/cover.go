package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/nwaples/rardecode/v2"
	"golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const coverMaxSide = 640

// coverCacheVer is part of generated cache file names; bumping it makes
// clients lazily regenerate at the new rendering rule while the files under
// the previous name are removed on that same pass.
const coverCacheVer = "t6"

// maxRarEntries bounds how far a cover lookup walks a RAR archive, so a
// pathological archive with thousands of non-image entries cannot stall the
// renderer.
const maxRarEntries = 2000

// Limits for decoding images out of untrusted archives: a crafted file must
// not be able to exhaust memory (decompression bomb) via the cover renderer.
const (
	decodeMaxBytes  = 32 << 20   // 32 MB of encoded image data
	decodeMaxPixels = 40_000_000 // larger than 8K; any real comic page fits
)

// manualCoverMaxBytes bounds an admin-supplied cover, whether uploaded or
// fetched from a URL, before it is decoded.
const manualCoverMaxBytes = 16 << 20

// Per-book locks plus a small global pool let several covers render at once
// without letting image decoding saturate every core.
var (
	coverLocks sync.Map // cache key -> *sync.Mutex
	coverSem   = make(chan struct{}, 4)
)

func coverDir(dataDir string) string { return filepath.Join(dataDir, "covers") }

// thumbName is the derived thumbnail file name for a stored cover image. The
// separator keeps it from colliding with the book cache names, which embed the
// version suffix in the middle.
func thumbName(file string) string { return file + "." + coverCacheVer + ".jpg" }

// bookCacheKeys lists the cache key variants one book may have used: the plain
// ID, the RAR suffix from before RAR covers were supported, and the declared
// cover suffix. Switching format or adding a declared cover leaves the
// previous slot behind, so cleanup must be able to reach all of them.
func bookCacheKeys(b *Book) []string {
	if b == nil {
		return nil
	}
	keys := []string{b.ID}
	if b.Format == "cbr" || b.Format == "rar" {
		keys = append(keys, b.ID+"-rar1")
	}
	if b.Meta != nil && b.Meta.CoverEntry != "" {
		keys = append(keys, b.ID+"-m")
	}
	return keys
}

// bookCacheNames lists the cache files the current renderer creates for a book
// (thumbnail and original). The valid set for automatic cleanup is built from
// these, so pre-t6 legacy files are deliberately absent and get swept.
func bookCacheNames(b *Book) []string {
	keys := bookCacheKeys(b)
	names := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		names = append(names, k+"."+coverCacheVer+".jpg", k+".orig."+coverCacheVer+".jpg")
	}
	return names
}

// bookCacheLegacyNames lists cache names written by versions before the
// version suffix existed; they are safe to delete on a cleanup pass.
func bookCacheLegacyNames(b *Book) []string {
	keys := bookCacheKeys(b)
	names := make([]string, 0, len(keys))
	for _, k := range keys {
		names = append(names, k+".jpg")
	}
	return names
}

// encodeOriginalJPEG re-encodes a full-size cover for storage and serving.
func encodeOriginalJPEG(img image.Image) []byte {
	var buf bytes.Buffer
	if jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}) != nil {
		return nil
	}
	return buf.Bytes()
}

// writeFileAtomic writes via a temporary name so a crash or a concurrent
// reader never sees a half-written cover.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// saveManualCover validates admin-supplied image bytes, stores them re-encoded
// as a full-size JPEG under covers/ and writes the serving thumbnail. Encoding
// instead of storing the upload verbatim keeps the image cache uniform and
// rejects anything that is not a decodable image.
func saveManualCover(dataDir, level, id string, raw []byte) (string, error) {
	img, err := decodeLimited(bytes.NewReader(raw))
	if err != nil {
		return "", errors.New("无法识别该图片")
	}
	out := encodeOriginalJPEG(img)
	if len(out) == 0 {
		return "", errors.New("图片编码失败")
	}
	dir := coverDir(dataDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := "manual-" + level + "-" + id + ".jpg"
	if err := writeFileAtomic(filepath.Join(dir, name), out); err != nil {
		return "", err
	}
	if thumb := encodeCover(img); len(thumb) > 0 {
		_ = os.WriteFile(filepath.Join(dir, thumbName(name)), thumb, 0o644)
	}
	return name, nil
}

// removeStoredCover deletes a stored cover image and its thumbnail, returning
// the number of files and bytes removed.
func removeStoredCover(dataDir, file string) (int, int64) {
	if file == "" {
		return 0, 0
	}
	dir := coverDir(dataDir)
	files, varBytes := 0, int64(0)
	for _, name := range []string{filepath.Base(file), thumbName(filepath.Base(file))} {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil {
			if os.Remove(p) == nil {
				files++
				varBytes += st.Size()
			}
		}
	}
	return files, varBytes
}

// coverJPEG returns a cached cover for the book, generating it if needed.
func coverJPEG(b *Book, dataDir string) []byte {
	return cachedCoverJPEG(b, dataDir, "")
}

// coverOriginalJPEG returns the full-size cover extracted from the book,
// re-encoded to a high-quality JPEG and cached; it does not apply
// coverMaxSide. The extra key component separates its cache slot from the
// thumbnail one.
func coverOriginalJPEG(b *Book, dataDir string) []byte {
	return cachedCoverJPEG(b, dataDir, ".orig")
}

func cachedCoverJPEG(b *Book, dataDir, slot string) []byte {
	if b == nil {
		return placeholderJPEG("")
	}
	key := b.ID
	if b.Format == "cbr" || b.Format == "rar" {
		// RAR covers used to be placeholder-only; a suffix invalidates those
		// cached placeholders once without touching every other cover.
		key += "-rar1"
	}
	if b.Meta != nil && b.Meta.CoverEntry != "" {
		// Declared covers get their own cache slot so upgrading from a
		// first-image cover to the declared one invalidates the old cache.
		key = b.ID + "-m"
	}
	dir := coverDir(dataDir)
	cache := filepath.Join(dir, key+slot+"."+coverCacheVer+".jpg")
	anyMu, _ := coverLocks.LoadOrStore(key+slot, &sync.Mutex{})
	mu := anyMu.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	if st, err := os.Stat(cache); err == nil && !st.ModTime().Before(b.ModTime) {
		if raw, err := os.ReadFile(cache); err == nil && len(raw) > 0 {
			return raw
		}
	}

	var img image.Image
	if slot == ".orig" {
		// Full-size output does not go through the downscaler.
		if raw := extractCoverBytes(b); raw != nil {
			return raw
		}
		coverSem <- struct{}{}
		img = extractCoverImage(b)
		<-coverSem
	} else {
		coverSem <- struct{}{}
		img = extractCoverImage(b)
		<-coverSem
	}
	var out []byte
	if img != nil {
		if slot == ".orig" {
			out = encodeOriginalJPEG(img)
		} else {
			out = encodeCover(img)
		}
	}
	if len(out) == 0 && slot != ".orig" {
		out = placeholderJPEG(b.Title)
	}
	if len(out) > 0 {
		if err := os.MkdirAll(dir, 0o755); err == nil {
			_ = os.WriteFile(cache, out, 0o644)
			if slot == "" {
				// Drop the pre-t6 cache file once the new one exists.
				_ = os.Remove(filepath.Join(dir, key+".jpg"))
			}
		}
	}
	return out
}

func extractCoverImage(b *Book) image.Image {
	// A cover declared in the archive's metadata wins over guessing.
	if b.Meta != nil && b.Meta.CoverEntry != "" {
		if img := imageFromZipEntry(b.Path, b.Meta.CoverEntry); img != nil {
			return img
		}
	}
	if b.Format == "dir" {
		return firstImageInDir(b.Path)
	}
	if b.Format == "cbz" || b.Format == "zip" || b.Format == "cb7" || b.Format == "epub" {
		return firstImageInZip(b.Path)
	}
	if b.Format == "cbr" || b.Format == "rar" {
		return firstImageInRar(b.Path)
	}
	return nil
}

// decodeLimited decodes one image from r, capping the encoded size and the
// pixel dimensions first so hostile archives cannot OOM the process. The
// entry stream of a zip file cannot be rewound, so the bytes are buffered.
func decodeLimited(r io.Reader) (image.Image, error) {
	raw, err := io.ReadAll(io.LimitReader(r, decodeMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > decodeMaxBytes {
		return nil, errors.New("image data exceeds size limit")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > decodeMaxPixels {
		return nil, errors.New("image dimensions exceed pixel limit")
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	return img, err
}

// imageFromZipEntry decodes the image stored under one archive entry name.
func imageFromZipEntry(file, entry string) image.Image {
	zr, err := zip.OpenReader(file)
	if err != nil {
		return nil
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name != entry {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil
		}
		img, err := decodeLimited(rc)
		rc.Close()
		if err != nil {
			return nil
		}
		return img
	}
	return nil
}

func firstImageInDir(dir string) image.Image {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || ignoreEntry(e.Name()) || !isImageName(e.Name()) {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Slice(names, func(i, j int) bool { return naturalLess(names[i], names[j]) })
	for _, n := range names {
		if img, err := decodeImageFile(filepath.Join(dir, n)); err == nil {
			return img
		}
	}
	return nil
}

// firstImageDirFile returns the path of the first renderable image in a
// directory book, using the same ordering as firstImageInDir.
func firstImageDirFile(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || ignoreEntry(e.Name()) || !isImageName(e.Name()) {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Slice(names, func(i, j int) bool { return naturalLess(names[i], names[j]) })
	for _, n := range names {
		p := filepath.Join(dir, n)
		if _, err := decodeImageFile(p); err == nil {
			return p
		}
	}
	return ""
}

// extractCoverBytes returns the raw bytes of a directory book's cover file so
// the original endpoint can serve it untouched; archive books have no single
// raw file to point at and get nil.
func extractCoverBytes(b *Book) []byte {
	if b == nil || b.Format != "dir" {
		return nil
	}
	p := firstImageDirFile(b.Path)
	if p == "" {
		return nil
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	return raw
}

func firstImageInZip(file string) image.Image {
	zr, err := zip.OpenReader(file)
	if err != nil {
		return nil
	}
	defer zr.Close()
	var files []*zip.File
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || strings.HasPrefix(f.Name, "__MACOSX/") {
			continue
		}
		name := zipEntryName(f)
		if strings.HasPrefix(filepath.Base(name), ".") || !isImageName(name) {
			continue
		}
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool {
		return naturalLess(zipEntryName(files[i]), zipEntryName(files[j]))
	})
	for _, f := range files {
		rc, err := f.Open()
		if err != nil {
			continue
		}
		img, err := decodeLimited(rc)
		rc.Close()
		if err == nil {
			return img
		}
	}
	return nil
}

// firstImageInRar decodes the first image entry of a RAR/CBR archive. rardecode
// is pure Go, so RAR covers no longer need the placeholder. Solid archives are
// read sequentially, which is why the archive itself is the entry reader.
func firstImageInRar(file string) image.Image {
	rc, err := rardecode.OpenReader(file)
	if err != nil {
		return nil
	}
	defer rc.Close()
	for n := 0; n < maxRarEntries; n++ {
		h, err := rc.Next()
		if err != nil {
			return nil
		}
		if h.IsDir || h.Encrypted || h.LinkType != rardecode.LinkTypeNone {
			continue
		}
		name := displayName(h.Name)
		if !isImageName(name) || strings.HasPrefix(filepath.Base(name), ".") {
			continue
		}
		img, err := decodeLimited(rc)
		if err != nil {
			return nil
		}
		return img
	}
	return nil
}

func decodeImageFile(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	switch strings.ToLower(filepath.Ext(path)) {
	case ".bmp":
		b, err := io.ReadAll(io.LimitReader(f, decodeMaxBytes+1))
		if err != nil {
			return nil, err
		}
		if len(b) > decodeMaxBytes {
			return nil, errors.New("image data exceeds size limit")
		}
		return bmp.Decode(bytes.NewReader(b))
	}
	return decodeLimited(f)
}

func encodeCover(src image.Image) []byte {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == 0 || h == 0 {
		return nil
	}
	scale := float64(coverMaxSide) / float64(maxInt(w, h))
	if scale > 1 {
		scale = 1
	}
	nw, nh := int(float64(w)*scale), int(float64(h)*scale)
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 82}); err != nil {
		return nil
	}
	return buf.Bytes()
}

// placeholderJPEG draws a neutral card so clients always get an image.
func placeholderJPEG(title string) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 320, 448))
	base := color.RGBA{0x3a, 0x3f, 0x4a, 0xff}
	accent := color.RGBA{0x5a, 0x64, 0x78, 0xff}
	for y := 0; y < 448; y++ {
		t := float64(y) / 448.0
		c := color.RGBA{
			uint8(float64(base.R) + t*float64(accent.R-base.R)),
			uint8(float64(base.G) + t*float64(accent.G-base.G)),
			uint8(float64(base.B) + t*float64(accent.B-base.B)),
			0xff,
		}
		for x := 0; x < 320; x++ {
			img.Set(x, y, c)
		}
	}
	draw.Draw(img, image.Rect(70, 150, 250, 300), image.NewUniform(color.RGBA{0xf0, 0xf2, 0xf5, 0xff}), image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(90, 170, 230, 280), image.NewUniform(base), image.Point{}, draw.Src)
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80})
	return buf.Bytes()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
