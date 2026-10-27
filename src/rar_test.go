package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// storeRAR builds a minimal RAR 4.x archive with one stored (uncompressed)
// entry, so the RAR cover path can be tested without shipping a binary sample.
func storeRAR(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	out.Write([]byte{0x52, 0x61, 0x72, 0x21, 0x1a, 0x07, 0x00}) // marker

	main := make([]byte, 13)
	main[2] = 0x73 // MAIN_HEAD
	binary.LittleEndian.PutUint16(main[5:7], 13)
	crc := crc32IEEE(main[2:])
	binary.LittleEndian.PutUint16(main[0:2], uint16(crc))
	out.Write(main)

	nameBytes := []byte(name)
	fh := make([]byte, 32+len(nameBytes))
	fh[2] = 0x74                                   // FILE_HEAD
	binary.LittleEndian.PutUint16(fh[3:5], 0x8000) // LONG_BLOCK: data follows
	binary.LittleEndian.PutUint16(fh[5:7], uint16(len(fh)))
	binary.LittleEndian.PutUint32(fh[7:11], uint32(len(data)))  // PACK_SIZE
	binary.LittleEndian.PutUint32(fh[11:15], uint32(len(data))) // UNP_SIZE
	fh[15] = 3                                                  // HOST_OS windows
	binary.LittleEndian.PutUint32(fh[16:20], crc32IEEE(data))   // FILE_CRC
	fh[24] = 20                                                 // UNP_VER 2.0
	fh[25] = 0x30                                               // METHOD store
	binary.LittleEndian.PutUint16(fh[26:28], uint16(len(nameBytes)))
	binary.LittleEndian.PutUint32(fh[28:32], 0x20)
	copy(fh[32:], nameBytes)
	crc = crc32IEEE(fh[2:])
	binary.LittleEndian.PutUint16(fh[0:2], uint16(crc))
	out.Write(fh)
	out.Write(data)
	return out.Bytes()
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFirstImageInRarDecodesStoredImage(t *testing.T) {
	p := writeTemp(t, "cover.cbr", storeRAR(t, "cover.png", tinyPNG(t)))
	img := firstImageInRar(p)
	if img == nil {
		t.Fatal("no cover decoded from a stored RAR entry")
	}
	if img.Bounds().Dx() != 3 {
		t.Fatalf("unexpected width %d", img.Bounds().Dx())
	}
}

func TestFirstImageInRarSkipsNonImageEntries(t *testing.T) {
	rars := storeRAR(t, "readme.txt", []byte("hello"))
	// Append the second entry's header+data, skipping the repeated marker and
	// main header (7 + 13 bytes) of the one-entry archive.
	p := writeTemp(t, "two.cbr", append(rars, storeRAR(t, "p1.png", tinyPNG(t))[20:]...))
	if img := firstImageInRar(p); img == nil {
		t.Fatal("image after a text entry was not found")
	}
	// A pure image archive must work as well.
	p2 := writeTemp(t, "img.cbr", storeRAR(t, "p1.png", tinyPNG(t)))
	if img := firstImageInRar(p2); img == nil {
		t.Fatal("image-only archive produced no cover")
	}
}

func TestFirstImageInRarRejectsGarbage(t *testing.T) {
	p := writeTemp(t, "bad.cbr", []byte("this is not a rar archive"))
	if img := firstImageInRar(p); img != nil {
		t.Fatal("garbage archive produced a cover")
	}
}

func TestLibraryFingerprintTracksContent(t *testing.T) {
	mod := time.Unix(1700000000, 0)
	build := func(pages int) *library {
		b := &Book{ID: "b1", Title: "第1卷", Rel: "A/第1卷.cbz", Size: 10,
			Pages: pages, ModTime: mod, Meta: &bookMeta{Title: "第1卷"}}
		l := newLibrary([]string{"/lib"}, "漫画库")
		l.series = []*Series{{ID: "s1", Name: "A", Rel: "A", Books: []*Book{b}}}
		l.books = map[string]*Book{"b1": b}
		l.scannedAt = mod
		return l
	}
	a, b := build(5), build(5)
	b.scannedAt = mod.Add(time.Hour) // scan time alone must not change anything
	if a.fingerprint() != b.fingerprint() {
		t.Fatal("identical libraries produced different fingerprints")
	}
	if build(5).fingerprint() == build(6).fingerprint() {
		t.Fatal("page count change was not detected")
	}
}
