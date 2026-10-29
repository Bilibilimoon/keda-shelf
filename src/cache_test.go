package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func cacheTestServer(t *testing.T) *server {
	t.Helper()
	dir := t.TempDir()
	b := &Book{
		ID: "b1", Title: "Book One", Format: "cbz",
		Path: filepath.Join(dir, "book.cbz"), SeriesID: "s1", ModTime: time.Now(),
	}
	sr := &Series{ID: "s1", Name: "Series One", Books: []*Book{b}}
	lib := newLibrary(nil, "test")
	lib.series = []*Series{sr}
	lib.books = map[string]*Book{b.ID: b}
	return &server{dataDir: dir, lib: lib, meta: loadMetaStore(filepath.Join(dir, "meta.json"), dir)}
}

func writeTestFile(t *testing.T, dir, name string, data []byte, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
}

func TestManualCoverRoundTrip(t *testing.T) {
	s := cacheTestServer(t)
	name, err := saveManualCover(s.dataDir, "book", "b1", tinyPNG(t))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if name != "manual-book-b1.jpg" {
		t.Fatalf("unexpected name %q", name)
	}
	for _, f := range []string{name, thumbName(name)} {
		if _, err := os.Stat(filepath.Join(coverDir(s.dataDir), f)); err != nil {
			t.Fatalf("%s missing: %v", f, err)
		}
	}
	if err := s.meta.setManual("book", "b1", name); err != nil {
		t.Fatal(err)
	}
	b := s.lib.findBook("b1")
	if got := s.storedCoverFile(b); got != name {
		t.Fatalf("storedCoverFile = %q, want %q", got, name)
	}
	if raw, ctype := s.originalCover(b); len(raw) == 0 || ctype != "image/jpeg" {
		t.Fatalf("original cover: %d bytes, type %q", len(raw), ctype)
	}
	if thumb := s.storedThumbnail(b); len(thumb) == 0 {
		t.Fatal("empty thumbnail")
	}

	file, err := s.meta.clearManual("book", "b1")
	if err != nil || file != name {
		t.Fatalf("clearManual = %q, %v", file, err)
	}
	files, _ := removeStoredCover(s.dataDir, file)
	if files != 2 {
		t.Fatalf("removeStoredCover removed %d files, want 2", files)
	}
	if s.storedCoverFile(b) != "" {
		t.Fatal("override still present after clear")
	}
}

func TestStoredCoverPriority(t *testing.T) {
	s := cacheTestServer(t)
	b := s.lib.findBook("b1")

	scraped := "scrape-series-s1.jpg"
	writeTestFile(t, coverDir(s.dataDir), scraped, tinyPNG(t), time.Time{})
	if err := s.meta.setSeries("s1", &scrapedInfo{Title: "S", Cover: scraped}); err != nil {
		t.Fatal(err)
	}
	if got := s.storedCoverFile(b); got != scraped {
		t.Fatalf("series scraped cover: got %q", got)
	}

	manual := "manual-book-b1.jpg"
	writeTestFile(t, coverDir(s.dataDir), manual, tinyPNG(t), time.Time{})
	if err := s.meta.setManual("book", "b1", manual); err != nil {
		t.Fatal(err)
	}
	if got := s.storedCoverFile(b); got != manual {
		t.Fatalf("manual book cover should win: got %q", got)
	}

	if _, err := s.meta.clearManual("book", "b1"); err != nil {
		t.Fatal(err)
	}
	if got := s.storedCoverFile(b); got != scraped {
		t.Fatalf("after clear, scraped cover should return: got %q", got)
	}
}

func TestCleanCoverCacheRemovesOnlyStale(t *testing.T) {
	s := cacheTestServer(t)
	dir := coverDir(s.dataDir)
	old := time.Now().Add(-2 * time.Hour)
	valid := map[string]bool{"b1.t6.jpg": true}
	writeTestFile(t, dir, "b1.t6.jpg", []byte("valid"), old)
	writeTestFile(t, dir, "gone.t6.jpg", []byte("stale"), old)
	writeTestFile(t, dir, "b1.jpg", []byte("legacy"), old)              // pre-t6 name
	writeTestFile(t, dir, "fresh.t6.jpg", []byte("fresh"), time.Time{}) // not valid yet, too new

	files, bytes, err := cleanCoverCache(s.dataDir, valid, cacheCleanMinAge)
	if err != nil {
		t.Fatal(err)
	}
	if files != 2 || bytes != int64(len("stale")+len("legacy")) {
		t.Fatalf("removed %d files / %d bytes, want 2 / %d", files, bytes, len("stale")+len("legacy"))
	}
	for _, keep := range []string{"b1.t6.jpg", "fresh.t6.jpg"} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Fatalf("%s should be kept: %v", keep, err)
		}
	}
	for _, gone := range []string{"gone.t6.jpg", "b1.jpg"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s should be gone", gone)
		}
	}
}

func TestClearBookCacheKeepsStoredOriginals(t *testing.T) {
	s := cacheTestServer(t)
	dir := coverDir(s.dataDir)
	manual := "manual-book-b1.jpg"
	scraped := "scrape-series-s1.jpg"
	if _, err := saveManualCover(s.dataDir, "book", "b1", tinyPNG(t)); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dir, scraped, tinyPNG(t), time.Time{})
	writeTestFile(t, dir, thumbName(scraped), []byte("scraped-thumb"), time.Time{})
	writeTestFile(t, dir, "b1.t6.jpg", []byte("book-thumb"), time.Time{})
	writeTestFile(t, dir, "b1.orig.t6.jpg", []byte("book-orig"), time.Time{})
	writeTestFile(t, dir, "b1.jpg", []byte("legacy"), time.Time{})
	if err := s.meta.setManual("book", "b1", manual); err != nil {
		t.Fatal(err)
	}
	if err := s.meta.setSeries("s1", &scrapedInfo{Title: "S", Cover: scraped}); err != nil {
		t.Fatal(err)
	}

	targets, err := s.clearCacheFiles("book", "b1")
	if err != nil {
		t.Fatal(err)
	}
	deleteCacheFiles(s.dataDir, targets)

	for _, gone := range []string{"b1.t6.jpg", "b1.orig.t6.jpg", "b1.jpg", thumbName(manual), thumbName(scraped)} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s should be removed", gone)
		}
	}
	for _, keep := range []string{manual, scraped} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Fatalf("%s (stored original) should survive: %v", keep, err)
		}
	}
}

func TestClearAllCoverCacheKeepsStoredOriginals(t *testing.T) {
	s := cacheTestServer(t)
	dir := coverDir(s.dataDir)
	manual := "manual-series-s1.jpg"
	scraped := "scrape-series-s1.jpg"
	writeTestFile(t, dir, manual, tinyPNG(t), time.Time{})
	writeTestFile(t, dir, thumbName(manual), []byte("mt"), time.Time{})
	writeTestFile(t, dir, scraped, tinyPNG(t), time.Time{})
	writeTestFile(t, dir, thumbName(scraped), []byte("st"), time.Time{})
	writeTestFile(t, dir, "b1.t6.jpg", []byte("bt"), time.Time{})
	writeTestFile(t, dir, "orphan.t6.jpg", []byte("ot"), time.Time{})
	if err := s.meta.setManual("series", "s1", manual); err != nil {
		t.Fatal(err)
	}
	if err := s.meta.setSeries("s1", &scrapedInfo{Title: "S", Cover: scraped}); err != nil {
		t.Fatal(err)
	}

	if files, _, err := s.clearAllCoverCache(); err != nil || files != 4 {
		t.Fatalf("clearAllCoverCache = %d files, %v; want 4", files, err)
	}
	for _, keep := range []string{manual, scraped} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Fatalf("%s should survive: %v", keep, err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("%d files left, want the 2 stored originals", len(entries))
	}
}

func TestExportManualCoversEmbedsImage(t *testing.T) {
	s := cacheTestServer(t)
	if _, err := saveManualCover(s.dataDir, "series", "s1", tinyPNG(t)); err != nil {
		t.Fatal(err)
	}
	if err := s.meta.setManual("series", "s1", "manual-series-s1.jpg"); err != nil {
		t.Fatal(err)
	}
	out := s.exportManualCovers()
	if len(out) != 1 || out[0].Level != "series" || out[0].ID != "s1" || out[0].Data == "" {
		t.Fatalf("unexpected export: %+v", out)
	}
}
