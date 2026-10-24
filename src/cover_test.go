package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 3, 3))
	for y := 0; y < 3; y++ {
		for x := 0; x < 3; x++ {
			img.Set(x, y, color.RGBA{0x20, 0x40, 0x80, 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDecodeLimitedAcceptsNormalImage(t *testing.T) {
	img, err := decodeLimited(bytes.NewReader(tinyPNG(t)))
	if err != nil {
		t.Fatalf("normal image rejected: %v", err)
	}
	if img.Bounds().Dx() != 3 {
		t.Fatalf("unexpected width %d", img.Bounds().Dx())
	}
}

func TestDecodeLimitedRejectsOversizedBytes(t *testing.T) {
	big := make([]byte, decodeMaxBytes+1)
	copy(big, tinyPNG(t))
	if _, err := decodeLimited(bytes.NewReader(big)); err == nil {
		t.Fatal("oversized byte stream accepted")
	}
}

// hugePNGHeader builds a valid PNG whose IHDR declares decodeMaxPixels+1
// pixels; decoding it must be refused before any memory is allocated.
func hugePNGHeader(t *testing.T, w, h uint32) []byte {
	t.Helper()
	var chunk func(typ string, data []byte) []byte
	chunk = func(typ string, data []byte) []byte {
		body := append([]byte(typ), data...)
		n := uint32(len(data))
		var out bytes.Buffer
		out.WriteByte(byte(n >> 24))
		out.WriteByte(byte(n >> 16))
		out.WriteByte(byte(n >> 8))
		out.WriteByte(byte(n))
		out.Write(body)
		crc := crc32IEEE(body)
		out.WriteByte(byte(crc >> 24))
		out.WriteByte(byte(crc >> 16))
		out.WriteByte(byte(crc >> 8))
		out.WriteByte(byte(crc))
		return out.Bytes()
	}
	sig := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	ihdr := []byte{
		byte(w >> 24), byte(w >> 16), byte(w >> 8), byte(w),
		byte(h >> 24), byte(h >> 16), byte(h >> 8), byte(h),
		8, 6, 0, 0, 0, // 8-bit RGBA
	}
	return append(append(sig[:8:8], chunk("IHDR", ihdr)...), chunk("IEND", nil)...)
}

func TestDecodeLimitedRejectsHugeDimensions(t *testing.T) {
	raw := hugePNGHeader(t, 8000, 6000) // 48 MP > decodeMaxPixels
	if _, err := decodeLimited(bytes.NewReader(raw)); err == nil {
		t.Fatal("decompression bomb accepted")
	}
}

func TestValidUsername(t *testing.T) {
	ok := []string{"admin", "user_01", "a", "Moonly-2.x"}
	for _, n := range ok {
		if !validUsername(n) {
			t.Errorf("%q should be valid", n)
		}
	}
	bad := []string{
		"", "a b", "a'b", `a"b`, "a<b>", "名字", "a;b", "a/b",
		"0123456789012345678901234567890123", // 33 chars
	}
	for _, n := range bad {
		if validUsername(n) {
			t.Errorf("%q should be rejected", n)
		}
	}
}

func TestValidScrapeID(t *testing.T) {
	if !validScrapeID("abc123XYZ-_") {
		t.Error("hash-like id rejected")
	}
	for _, id := range []string{"", "../evil", "a b", "a/b"} {
		if validScrapeID(id) {
			t.Errorf("%q should be rejected", id)
		}
	}
}

func crc32IEEE(p []byte) uint32 {
	const poly = 0xedb88320
	crc := uint32(0xffffffff)
	for _, b := range p {
		crc ^= uint32(b)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = crc>>1 ^ poly
			} else {
				crc >>= 1
			}
		}
	}
	return ^crc
}
