package main

import (
	"archive/zip"
	"testing"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

func gbkBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatalf("encode GBK: %v", err)
	}
	return b
}

func TestDisplayNameDecodesGBK(t *testing.T) {
	const want = "漫画 第1卷"
	raw := string(gbkBytes(t, want))
	if utf8.ValidString(raw) {
		t.Skip("GBK sample happens to be valid UTF-8; path not exercised")
	}
	if got := displayName(raw); got != want {
		t.Fatalf("displayName(%q) = %q, want %q", raw, got, want)
	}
}

func TestDisplayNameKeepsValidNames(t *testing.T) {
	for _, s := range []string{"", "第1卷.cbz", "Vol.01.zip", "normal-name"} {
		if got := displayName(s); got != s {
			t.Errorf("displayName(%q) changed to %q", s, got)
		}
	}
}

func TestDisplayNameKeepsUndecodableBytes(t *testing.T) {
	raw := string([]byte{0xff, 0xfe, 0x80, '.', 'j', 'p', 'g'})
	if got := displayName(raw); got != raw {
		t.Errorf("undecodable name changed to %q", got)
	}
}

func TestZipEntryNameHonoursUTF8Flag(t *testing.T) {
	const want = "第二卷"
	raw := string(gbkBytes(t, want))
	legacy := &zip.File{FileHeader: zip.FileHeader{Name: raw, NonUTF8: true}}
	if got := zipEntryName(legacy); got != want {
		t.Errorf("legacy entry name = %q, want %q", got, want)
	}
	modern := &zip.File{FileHeader: zip.FileHeader{Name: want}}
	if got := zipEntryName(modern); got != want {
		t.Errorf("utf-8 entry name = %q, want %q", got, want)
	}
}

func TestXMLDecodeGBKDeclaration(t *testing.T) {
	const want = "进击的巨人"
	raw := []byte(`<?xml version="1.0" encoding="gbk"?><ComicInfo><Title>`)
	raw = append(raw, gbkBytes(t, want)...)
	raw = append(raw, []byte(`</Title></ComicInfo>`)...)
	var ci struct {
		Title string `xml:"Title"`
	}
	if err := xmlDecode(raw, &ci); err != nil {
		t.Fatalf("xmlDecode: %v", err)
	}
	if ci.Title != want {
		t.Fatalf("title = %q, want %q", ci.Title, want)
	}
}

func TestXMLDecodeUTF8(t *testing.T) {
	raw := []byte(`<?xml version="1.0" encoding="UTF-8"?><ComicInfo><Title>海贼王</Title></ComicInfo>`)
	var ci struct {
		Title string `xml:"Title"`
	}
	if err := xmlDecode(raw, &ci); err != nil {
		t.Fatalf("xmlDecode: %v", err)
	}
	if ci.Title != "海贼王" {
		t.Fatalf("title = %q", ci.Title)
	}
}
