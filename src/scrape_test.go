package main

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanSearchTerm(t *testing.T) {
	cases := []struct{ in, want string }{
		{"[XX汉化组] 进击的巨人 第05卷", "进击的巨人"},
		{"进击的巨人 vol.3", "进击的巨人"},
		{"日常小品 第12话", "日常小品"},
		{"某作品 V2", "某作品"},
		{"试读本.epub", "试读本"},
		{"[Group] Title (2020) Book 3", "Title"},
		{"  多  空格  ", "多 空格"},
		{"第1卷", ""},
		// Real-world rips: kepub suffix, spaced volume numbers, split volumes
		// and the 話NNN-NNN ranges that KMoe/Kox use.
		{"[Kox][一拳超人]話231-235.kepub.epub", "一拳超人"},
		{"[Kmoe][不死不幸]話170-175.epub", "不死不幸"},
		{"一之瀨家的大罪卷 01.epub", "一之瀨家的大罪"},
		{"回復術士的重啟人生卷13_1.epub", "回復術士的重啟人生"},
		{"[Kox][一拳超人]話166-170[至127擊].kepub.epub", "一拳超人"},
		{"[Kox][一拳超人]英雄大全.kepub.epub", "英雄大全"},
		{"[Kox][別當歐尼醬了！]卷01.kepub.epub", "別當歐尼醬了！"},
	}
	for _, c := range cases {
		if got := cleanSearchTerm(c.in); got != c.want {
			t.Errorf("cleanSearchTerm(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormTitleAndConfidence(t *testing.T) {
	if normTitle("Shingeki no Kyojin!") != "shingekinokyojin" {
		t.Errorf("normTitle latin: %q", normTitle("Shingeki no Kyojin!"))
	}
	cases := []struct {
		term  string
		cand  scrapeCandidate
		want  bool
		label string
	}{
		{"よつばと！", scrapeCandidate{Title: "よつばと！"}, true, "exact after norm"},
		{"Yotsuba-to", scrapeCandidate{Title: "Yotsuba&To!"}, true, "romanised variant"},
		{"进击的巨人", scrapeCandidate{Title: "進撃の巨人"}, false, "simplified vs japanese stays manual"},
		{"少女", scrapeCandidate{Title: "少女終末旅行"}, false, "prefix must not auto-match"},
		{"", scrapeCandidate{Title: "x"}, false, "empty term"},
	}
	for _, c := range cases {
		if got := scrapeConfident(c.term, c.cand); got != c.want {
			t.Errorf("%s: scrapeConfident(%q, %q) = %v, want %v", c.label, c.term, c.cand.Title, got, c.want)
		}
	}
}

func TestRuneOverlap(t *testing.T) {
	if runeOverlap("abc", "abc") != 1 {
		t.Error("identical sets should overlap 1")
	}
	if runeOverlap("", "abc") != 0 {
		t.Error("empty set should overlap 0")
	}
	if runeOverlap("abcd", "ab") != 0.5 {
		t.Errorf("partial overlap: got %v", runeOverlap("abcd", "ab"))
	}
}

func TestNormalizePermDirs(t *testing.T) {
	valid := map[string]bool{"a": true, "a/b": true, "a/b/c": true, "x": true}
	got := normalizePermDirs([]string{"a/b/c", "a", "missing", "x", "a/b", "x"}, valid)
	want := []string{"a", "x"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("normalizePermDirs = %v, want %v", got, want)
	}
}

func TestGrantedDirs(t *testing.T) {
	s := &server{lib: &library{series: []*Series{
		{ID: "1", Rel: "漫画/pdf/悬疑"},
		{ID: "2", Rel: "少女"},
		{ID: "3", Rel: "/"},
	}}}
	got := s.grantedDirs()
	for _, want := range []string{"漫画", "漫画/pdf", "漫画/pdf/悬疑", "少女"} {
		if !got[want] {
			t.Errorf("grantedDirs missing %q (have %v)", want, got)
		}
	}
	if got["/"] || got[""] {
		t.Errorf("root loose series must not be grantable: %v", got)
	}
}

func TestMetaStoreSuggestLifecycle(t *testing.T) {
	dir := t.TempDir()
	m := loadMetaStore(filepath.Join(dir, "meta.json"), dir)
	if err := m.setSuggest("s1", &scrapeCandidate{ID: 7, Title: "x"}); err != nil {
		t.Fatal(err)
	}
	if len(m.suggestions()) != 1 {
		t.Fatal("suggestion not stored")
	}
	if err := m.setIgnored("s1"); err != nil {
		t.Fatal(err)
	}
	if len(m.suggestions()) != 0 || !m.isIgnored("s1") {
		t.Fatal("ignore must clear the suggestion")
	}
}

func TestEpubMetadata(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	mw, _ := zw.CreateHeader(&zip.FileHeader{Name: "mimetype", Method: zip.Store})
	mw.Write([]byte("application/epub+zip"))
	cw, _ := zw.Create("META-INF/container.xml")
	cw.Write([]byte(`<container><rootfiles><rootfile full-path="OEBPS/book.opf"/></rootfiles></container>`))
	img, _ := zw.Create("OEBPS/images/p 1.jpg")
	img.Write([]byte{0xff, 0xd8, 0xff, 0xd9})
	opf, _ := zw.Create("OEBPS/book.opf")
	opf.Write([]byte(`<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" xmlns:dc="http://purl.org/dc/elements/1.1/" version="3.0">
 <metadata>
  <dc:title>测试书名</dc:title>
  <dc:creator>作者甲</dc:creator><dc:creator>作者乙</dc:creator>
  <dc:description>&lt;p&gt;这是&lt;b&gt;简介&lt;/b&gt;。&lt;/p&gt;</dc:description>
  <dc:subject>标签1</dc:subject>
 </metadata>
 <manifest><item id="c" href="images/p%201.jpg" media-type="image/jpeg" properties="cover-image"/></manifest>
</package>`))
	zw.Close()
	file := filepath.Join(t.TempDir(), "t.epub")
	os.WriteFile(file, buf.Bytes(), 0o644)

	m := epubMetadata(file)
	if m == nil {
		t.Fatal("epubMetadata returned nil")
	}
	if m.Title != "测试书名" {
		t.Errorf("title = %q", m.Title)
	}
	if len(m.Authors) != 2 || m.Authors[0] != "作者甲" {
		t.Errorf("authors = %v", m.Authors)
	}
	if m.Summary != "这是简介。" {
		t.Errorf("summary = %q", m.Summary)
	}
	if m.CoverEntry != "OEBPS/images/p 1.jpg" {
		t.Errorf("cover entry (must be URL-unescaped) = %q", m.CoverEntry)
	}
}

func TestArchiveMetadataEmptyMarker(t *testing.T) {
	// A zip without any metadata yields a non-nil empty marker so scan caching
	// does not reopen it forever.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("001.jpg")
	w.Write([]byte{0xff, 0xd8, 0xff, 0xd9})
	zw.Close()
	file := filepath.Join(t.TempDir(), "t.zip")
	os.WriteFile(file, buf.Bytes(), 0o644)

	m := archiveMetadata(file, "zip")
	if m == nil {
		t.Fatal("archiveMetadata must return the empty marker, not nil")
	}
	if m.Title != "" || m.CoverEntry != "" {
		t.Errorf("unexpected metadata: %+v", m)
	}
}
