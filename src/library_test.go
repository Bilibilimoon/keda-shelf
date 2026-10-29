package main

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"sort"
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

func TestScanGroupsVolumeDirsIntoParent(t *testing.T) {
	root := t.TempDir()
	writeTestCBZ(t, filepath.Join(root, "星海旅人", "第1卷.cbz"), 2)
	os.MkdirAll(filepath.Join(root, "散图"), 0o755)
	os.WriteFile(filepath.Join(root, "散图", "01.jpg"), []byte{0xff, 0xd8}, 0o644)
	// Images directly in the root become the top-level loose series.
	os.WriteFile(filepath.Join(root, "cover.jpg"), []byte{0xff, 0xd8}, 0o644)

	l := newLibrary([]string{root}, "测试库")
	l.scan(nil, nil)
	series, _, _ := l.snapshot()

	loose := findSeriesByRel(series, "/")
	if loose == nil || loose.Name != "测试库" {
		var got []string
		for _, sr := range series {
			got = append(got, `rel="`+sr.Rel+`" name="`+sr.Name+`"`)
		}
		t.Fatalf("loose series should keep the 0.2.x shape (rel /), got %v", got)
	}
	if loose.ID != hashID("series:/") {
		t.Errorf("loose series id changed, breaks saved permissions")
	}
	if len(loose.Books) != 2 {
		t.Errorf("loose series should hold the root image and 散图, got %d books", len(loose.Books))
	}
	if findSeriesByRel(series, "星海旅人") == nil {
		t.Errorf("an archive directory keeps its own series")
	}
}

func TestScanGroupsVolumeTrees(t *testing.T) {
	root := t.TempDir()
	// A work made of image volumes: 第01卷/番外04 are its books, not series.
	for _, v := range []string{"第01卷", "第02卷", "番外04"} {
		for i := 0; i < 3; i++ {
			p := filepath.Join(root, "蜘蛛", v, string(rune('a'+i))+".jpg")
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, []byte{0xff, 0xd8}, 0o644)
		}
	}
	// An archive series whose image directory is one of its volumes.
	writeTestCBZ(t, filepath.Join(root, "合集", "孤单话.cbz"), 1)
	os.MkdirAll(filepath.Join(root, "合集", "番外"), 0o755)
	os.WriteFile(filepath.Join(root, "合集", "番外", "01.jpg"), []byte{0xff, 0xd8}, 0o644)
	// A release-group folder above the volumes must not become a series.
	writeTestCBZ(t, filepath.Join(root, "合集2", "别当欧尼酱了", "KOX", "卷01.cbz"), 1)
	writeTestCBZ(t, filepath.Join(root, "合集2", "别当欧尼酱了", "KOX", "卷02.cbz"), 1)

	l := newLibrary([]string{root}, "库")
	l.scan(nil, nil)
	series, _, _ := l.snapshot()
	byRel := map[string]*Series{}
	for _, sr := range series {
		byRel[sr.Rel] = sr
	}

	for _, sr := range series {
		if strings.HasPrefix(sr.Rel, "蜘蛛/") {
			t.Fatalf("volumes of 蜘蛛 must not be series of their own: %q", sr.Rel)
		}
	}
	spider := byRel["蜘蛛"]
	if spider == nil || len(spider.Books) != 3 {
		t.Fatalf("蜘蛛 should be one series with its three volumes, got %+v", spider)
	}
	if spider.Books[0].Title != "第01卷" || spider.Books[2].Title != "番外04" {
		t.Errorf("volumes should be in reading order, got %q first", spider.Books[0].Title)
	}
	if byRel["合集"] == nil || len(byRel["合集"].Books) != 2 {
		t.Fatalf("合集 should hold its archive and the 番外 volume, got %+v", byRel["合集"])
	}
	if byRel["合集2/别当欧尼酱了"] == nil {
		t.Fatalf("别当欧尼酱了 should be a series, got %v", keysOf(byRel))
	}
	if byRel["合集2/别当欧尼酱了/KOX"] != nil {
		t.Errorf("KOX is a release group, not a work")
	}
}

func keysOf(m map[string]*Series) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func findSeriesByRel(series []*Series, rel string) *Series {
	for _, sr := range series {
		if sr.Rel == rel {
			return sr
		}
	}
	return nil
}

func TestScanContainerDirsAreNotSeries(t *testing.T) {
	root := t.TempDir()
	// A folder that only groups works must not become a series itself.
	writeTestCBZ(t, filepath.Join(root, "合集", "一拳超人", "卷01.cbz"), 1)
	writeTestCBZ(t, filepath.Join(root, "合集", "不死不幸", "卷01.cbz"), 1)
	// A wrapper around a single work takes that work's name; a release group
	// folder above the volumes does not survive as a series.
	writeTestCBZ(t, filepath.Join(root, "别当欧尼酱了", "KOX", "卷01.cbz"), 1)
	writeTestCBZ(t, filepath.Join(root, "别当欧尼酱了", "KOX", "卷02.cbz"), 1)
	// A volume folder around a single file is a book of its series.
	writeTestCBZ(t, filepath.Join(root, "来自深渊", "第01话.cbz"), 1)
	writeTestCBZ(t, filepath.Join(root, "来自深渊", "第61話", "第61话.cbz"), 1)

	l := newLibrary([]string{root}, "库")
	l.scan(nil, nil)
	series, _, _ := l.snapshot()
	byRel := map[string]*Series{}
	for _, sr := range series {
		byRel[sr.Rel] = sr
	}

	if byRel["合集"] != nil || byRel["别当欧尼酱了/KOX"] != nil {
		t.Errorf("grouping folders must not become series, got %v", keysOf(byRel))
	}
	if byRel["合集/一拳超人"] == nil || byRel["合集/不死不幸"] == nil {
		t.Errorf("each work inside a container keeps its series, got %v", keysOf(byRel))
	}
	wrapped := byRel["别当欧尼酱了"]
	if wrapped == nil || len(wrapped.Books) != 2 {
		t.Fatalf("别当欧尼酱了 should be one series with both volumes, got %+v", wrapped)
	}
	if wrapped.Books[0].Rel != "别当欧尼酱了/KOX/卷01.cbz" {
		t.Errorf("the volume keeps its real path, got %q", wrapped.Books[0].Rel)
	}
	if deep := byRel["来自深渊"]; deep == nil || len(deep.Books) != 2 {
		t.Fatalf("来自深渊 should hold both episodes, got %+v", deep)
	}
}

func TestIsVolumeName(t *testing.T) {
	volumes := []string{"第01卷", "第2話", "番外04", "BD特典1", "01卷附页", "第06卷补图与通知", "连载版213", "Vol. 3", "Ch. 12", "12", "[Seaside Scanlations] Vol. 1 Ch. 1"}
	for _, n := range volumes {
		if !isVolumeName(n) {
			t.Errorf("%q should read as a volume", n)
		}
	}
	works := []string{"散图", "漫画A", "别当欧尼酱了", "不过如此", "英雄大全"}
	for _, n := range works {
		if isVolumeName(n) {
			t.Errorf("%q should read as a work name", n)
		}
	}
}

func TestSortBooksPutsSideStoriesLast(t *testing.T) {
	books := []*Book{{Title: "番外04"}, {Title: "第10卷"}, {Title: "第2卷"}, {Title: "特典1"}}
	sortBooks(books)
	var got []string
	for _, b := range books {
		got = append(got, b.Title)
	}
	want := "第2卷|第10卷|特典1|番外04"
	if strings.Join(got, "|") != want {
		t.Errorf("sortBooks = %v, want %s", got, want)
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
		// The loose image directory now belongs to its sub-library, which
		// keeps the unprefixed "/" key so cached grants stay valid.
		"/",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing prefixed series %q in %v", want, rels)
		}
	}
	// Book rels inherit the prefix so permission grants stay per-library.
	for _, sr := range series {
		if sr.Rel == "/" {
			if sr.Name != filepath.Base(r2) {
				t.Errorf("loose series should be named after sub-library %q, got %q", filepath.Base(r2), sr.Name)
			}
			continue
		}
		if !strings.HasPrefix(sr.Rel, filepath.Base(r1)+"/") && !strings.HasPrefix(sr.Rel, filepath.Base(r2)+"/") {
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
