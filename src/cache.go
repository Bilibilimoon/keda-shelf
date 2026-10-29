package main

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"
)

// cacheCleanEvery throttles the automatic stale-cover sweep: a scan runs every
// few minutes by default, but walking the cover directory that often buys
// nothing.
const cacheCleanEvery = 24 * time.Hour

// cacheCleanMinAge keeps the sweep away from files that a scrape or a request
// may be writing right now; their metadata record does not exist yet.
const cacheCleanMinAge = 10 * time.Minute

type cacheUsage struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

// coverCacheUsage sums the files currently under covers/.
func coverCacheUsage(dataDir string) cacheUsage {
	u := cacheUsage{}
	entries, err := os.ReadDir(coverDir(dataDir))
	if err != nil {
		return u
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if info, err := e.Info(); err == nil {
			u.Files++
			u.Bytes += info.Size()
		}
	}
	return u
}

// validCoverCache is every cache file name reachable from the current library
// and metadata. Anything else under covers/ is stale.
func (s *server) validCoverCache() map[string]bool {
	series, _, _ := s.lib.snapshot()
	valid := make(map[string]bool)
	for _, sr := range series {
		for _, b := range sr.Books {
			for _, n := range bookCacheNames(b) {
				valid[n] = true
			}
		}
	}
	// Stored cover images are data, and their thumbnails are what the feeds
	// serve for scraped and manual covers.
	for _, f := range s.meta.storedCoverFiles() {
		base := filepath.Base(f)
		valid[base] = true
		valid[thumbName(base)] = true
	}
	return valid
}

// cleanCoverCache removes files that the current library can no longer reach.
// Files modified within minAge are skipped so an in-flight download or render
// is not deleted before its metadata record is written.
func cleanCoverCache(dataDir string, valid map[string]bool, minAge time.Duration) (files int, bytes int64, err error) {
	dir := coverDir(dataDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	cutoff := time.Now().Add(-minAge)
	for _, e := range entries {
		if e.IsDir() || valid[e.Name()] {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if os.Remove(filepath.Join(dir, e.Name())) == nil {
			files++
			bytes += info.Size()
		}
	}
	return files, bytes, nil
}

// maybeCleanCoverCache schedules the stale-cover sweep at most once a day in
// the background. It is called after a scan so the library snapshot is fresh;
// the sweep never blocks requests or the scan swap.
func (s *server) maybeCleanCoverCache() {
	s.cleanMu.Lock()
	if !s.lastClean.IsZero() && time.Since(s.lastClean) < cacheCleanEvery {
		s.cleanMu.Unlock()
		return
	}
	s.lastClean = time.Now()
	s.cleanMu.Unlock()
	go func() {
		files, bytes, err := cleanCoverCache(s.dataDir, s.validCoverCache(), cacheCleanMinAge)
		if err != nil {
			log.Printf("cover cache cleanup: %v", err)
			return
		}
		if files > 0 {
			log.Printf("cover cache cleanup: removed %d stale files (%s)", files, humanSize(bytes))
		}
	}()
}

// bookCacheTargets is the set of files a book's "clear cache" action removes:
// every derived cover, plus the thumbnail the feed serves, which may belong to
// a series- or book-level stored cover. Stored originals are never included.
func (s *server) bookCacheTargets(b *Book) map[string]bool {
	targets := make(map[string]bool)
	for _, n := range bookCacheNames(b) {
		targets[n] = true
	}
	for _, n := range bookCacheLegacyNames(b) {
		targets[n] = true
	}
	if c := s.meta.manual("book", b.ID); c != nil {
		targets[thumbName(filepath.Base(c.File))] = true
	} else if c := s.meta.manual("series", b.SeriesID); c != nil {
		targets[thumbName(filepath.Base(c.File))] = true
	}
	cover := ""
	if info := s.meta.book(b.ID); info != nil && info.Cover != "" {
		cover = info.Cover
	} else if info := s.meta.series(b.SeriesID); info != nil {
		cover = info.Cover
	}
	if cover != "" {
		targets[thumbName(filepath.Base(cover))] = true
	}
	return targets
}

// clearCacheFiles validates the target and returns the file names to delete.
func (s *server) clearCacheFiles(level, id string) (map[string]bool, error) {
	switch level {
	case "book":
		b := s.lib.findBook(id)
		if b == nil {
			return nil, errors.New("找不到这本书")
		}
		return s.bookCacheTargets(b), nil
	case "series":
		sr := s.lib.findSeries(id)
		if sr == nil {
			return nil, errors.New("找不到这个系列")
		}
		targets := make(map[string]bool)
		for _, b := range sr.Books {
			for n := range s.bookCacheTargets(b) {
				targets[n] = true
			}
		}
		if info := s.meta.series(id); info != nil && info.Cover != "" {
			targets[thumbName(filepath.Base(info.Cover))] = true
		}
		if c := s.meta.manual("series", id); c != nil {
			targets[thumbName(filepath.Base(c.File))] = true
		}
		return targets, nil
	default:
		return nil, errors.New("无效的清理范围")
	}
}

// deleteCacheFiles removes the named files under covers/, returning the count
// and bytes freed.
func deleteCacheFiles(dataDir string, targets map[string]bool) (int, int64) {
	dir := coverDir(dataDir)
	files, bytes := 0, int64(0)
	for name := range targets {
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil {
			if os.Remove(p) == nil {
				files++
				bytes += info.Size()
			}
		}
	}
	return files, bytes
}

// clearAllCoverCache removes every derived file under covers/ while keeping
// the stored cover images (scraped and manual) that cannot be regenerated
// offline.
func (s *server) clearAllCoverCache() (int, int64, error) {
	protected := make(map[string]bool)
	for _, f := range s.meta.storedCoverFiles() {
		protected[filepath.Base(f)] = true
	}
	dir := coverDir(s.dataDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	files, bytes := 0, int64(0)
	for _, e := range entries {
		if e.IsDir() || protected[e.Name()] {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if os.Remove(filepath.Join(dir, e.Name())) == nil {
			files++
			bytes += info.Size()
		}
	}
	return files, bytes, nil
}
