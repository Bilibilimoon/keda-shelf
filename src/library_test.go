package main

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTestCBZ(t *testing.T, path string, pages int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for i := 0; i < pages; i++ {
		w, _ := zw.Create(string(rune('a'+i)) + ".jpg")
		w.Write([]byte{0xff, 0xd8, 0xff, 0xd9})
	}
	zw.Close()
}

func TestNaturalLess(t *testing.T) {
	cases := []struct{ a, b string }{
		{"第2卷", "第10卷"},
		{"卷2 x", "卷10 x"},
		{"abc2", "abc10"},
	}
	for _, c := range cases {
		if !naturalLess(c.a, c.b) {
			t.Errorf("naturalLess(%q, %q) should be true", c.a, c.b)
		}
		if naturalLess(c.b, c.a) {
			t.Errorf("naturalLess(%q, %q) should be false", c.b, c.a)
		}
	}
}

func TestScanSingleRootKeepsUnprefixedRel(t *testing.T) {
	root := t.TempDir()
	writeTestCBZ(t, filepath.Join(root, "星海旅人", "第1卷.cbz"), 2)
	os.MkdirAll(filepath.Join(root, "散图"), 0o755)
	os.WriteFile(filepath.Join(root, "散图", "01.jpg"), []byte{0xff, 0xd8}, 0o644)
	// Images directly in the root become the top-level loose series.
	os.WriteFile(filepath.Join(root, "cover.jpg"), []byte{0xff, 0xd8}, 0o644)

	l := newLibrary([]string{root}, "测试库")
	l.scan(nil, nil)
	series, _, _ := l.snapshot()
	byRel := map[string]*Series{}
	for _, sr := range series {
		byRel[sr.Rel] = sr
	}
	if byRel["星海旅人"] == nil {
		t.Fatalf("missing series, got %+v", series)
	}
	if byRel["散图"] == nil {
		t.Fatalf("missing subdir series, got %+v", series)
	}
	if byRel["/"] == nil || byRel["/"].Name != "测试库" {
		t.Errorf("loose series should keep the 0.2.x shape (rel /), got %+v", byRel["/"])
	}
	if byRel["/"].ID != hashID("series:/") {
		t.Errorf("loose series id changed, breaks saved permissions")
	}
}

func TestScanMultiRootPrefixesRel(t *testing.T) {
	r1 := t.TempDir()
	r2 := t.TempDir()
	writeTestCBZ(t, filepath.Join(r1, "悬疑", "第一案.cbz"), 1)
	os.MkdirAll(filepath.Join(r2, "日常"), 0o755)
	os.WriteFile(filepath.Join(r2, "日常", "01.jpg"), []byte{0xff, 0xd8}, 0o644)

	l := newLibrary([]string{r1, r2}, "测试库")
	l.scan(nil, nil)
	series, _, _ := l.snapshot()
	var rels []string
	for _, sr := range series {
		rels = append(rels, sr.Rel)
	}
	joined := strings.Join(rels, ",")
	for _, want := range []string{
		filepath.Base(r1) + "/悬疑",
		filepath.Base(r2) + "/日常",
		filepath.Base(r2),
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing prefixed series %q in %v", want, rels)
		}
	}
	// Book rels inherit the prefix so permission grants stay per-library.
	for _, sr := range series {
		if sr.Rel != filepath.Base(r2) && !strings.HasPrefix(sr.Rel, filepath.Base(r1)+"/") &&
			!strings.HasPrefix(sr.Rel, filepath.Base(r2)+"/") {
			t.Errorf("unexpected unprefixed rel %q", sr.Rel)
		}
	}
}

func TestScanCollectsProblems(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "坏包"), 0o755)
	os.WriteFile(filepath.Join(root, "坏包", "broken.cbz"), []byte("not a zip"), 0o644)

	l := newLibrary([]string{root}, "库")
	l.scan(nil, nil)
	probs := l.problemsSnapshot()
	if len(probs) != 1 || !strings.Contains(probs[0].Reason, "压缩包无法读取") {
		t.Fatalf("expected one problem entry, got %+v", probs)
	}
	if !strings.Contains(probs[0].Path, "broken.cbz") {
		t.Errorf("problem path should name the file: %+v", probs[0])
	}
}

func TestScanCacheRoundTrip(t *testing.T) {
	root := t.TempDir()
	writeTestCBZ(t, filepath.Join(root, "系列", "书1.cbz"), 3)
	file := filepath.Join(t.TempDir(), "library.json")

	l := newLibrary([]string{root}, "库")
	l.scan(nil, nil)
	if err := l.saveCache(file); err != nil {
		t.Fatal(err)
	}
	loaded := loadLibraryCache(file, []string{root}, "库")
	if loaded == nil {
		t.Fatal("cache did not reload")
	}
	series, _, _ := loaded.snapshot()
	if len(series) != 1 || len(series[0].Books) != 1 || series[0].Books[0].Pages != 3 {
		t.Fatalf("cache roundtrip mismatch: %+v", series)
	}
	if loadLibraryCache(file, []string{root, "/elsewhere"}, "库") != nil {
		t.Error("cache must not load for a different root set")
	}
}

func TestDirPermMatching(t *testing.T) {
	p := &dirPerm{dirs: []string{"pdf", "epub/异世界"}}
	cases := []struct {
		rel  string
		want bool
	}{
		{"pdf", true},
		{"pdf/悬疑", true},
		{"pdf/悬疑/深/更deep", true},
		{"epub/异世界", true},
		{"epub/悬疑", false},
		{"epubx", false},
		{"pdfx/悬疑", false},
	}
	for _, c := range cases {
		if got := p.ok(c.rel); got != c.want {
			t.Errorf("ok(%q) = %v, want %v", c.rel, got, c.want)
		}
	}
	if (*dirPerm)(nil).ok("anything") != true {
		t.Error("nil perm means unrestricted")
	}
}

func TestCountZipPagesIgnoresMacNoise(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{"01.jpg", "__MACOSX/._01.jpg", ".hidden.jpg", "note.txt"} {
		w, _ := zw.Create(name)
		w.Write([]byte("x"))
	}
	zw.Close()
	file := filepath.Join(t.TempDir(), "t.cbz")
	os.WriteFile(file, buf.Bytes(), 0o644)
	n, err := countZipPages(file)
	if err != nil || n != 1 {
		t.Fatalf("countZipPages = %d, %v; want 1, nil", n, err)
	}
}

func TestScanReusesUnchangedArchive(t *testing.T) {
	root := t.TempDir()
	cbz := filepath.Join(root, "系列", "书1.cbz")
	writeTestCBZ(t, cbz, 3)
	l := newLibrary([]string{root}, "库")
	l.scan(nil, nil)
	// Make the file newer but identical in size: a real content change.
	time.Sleep(1100 * time.Millisecond)
	writeTestCBZ(t, cbz, 3)

	l2 := newLibrary([]string{root}, "库")
	l2.scan(l.booksByPath(), nil)
	series, _, _ := l2.snapshot()
	if series[0].Books[0].Pages != 3 {
		t.Fatalf("prev reuse failed: %+v", series[0].Books[0])
	}
}

func TestHumanSize(t *testing.T) {
	if humanSize(512) != "512 B" {
		t.Errorf("humanSize(512) = %q", humanSize(512))
	}
	if !strings.HasPrefix(humanSize(2048), "2.0 K") {
		t.Errorf("humanSize(2048) = %q", humanSize(2048))
	}
}

var _ = bytes.MinRead
