package main

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Book struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Rel      string    `json:"rel"`
	Path     string    `json:"path"`
	Format   string    `json:"format"`
	Size     int64     `json:"size"`
	Pages    int       `json:"pages"`
	ModTime  time.Time `json:"mod_time"`
	SeriesID string    `json:"series_id"`
	// Meta is the embedded archive metadata; nil only before the first
	// extraction, an empty struct means "checked, none present".
	Meta *bookMeta `json:"meta,omitempty"`
}

type Series struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Rel   string  `json:"rel"`
	Books []*Book `json:"books"`
	Cover string  `json:"cover"`
}

// scanProblem is one unreadable file or directory met during a scan, so the
// owner can fix broken archives instead of wondering why books are missing.
type scanProblem struct {
	Rel    string `json:"rel"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type library struct {
	mu        sync.RWMutex
	roots     []string
	name      string
	series    []*Series
	books     map[string]*Book
	scannedAt time.Time
	stats     scanStats
	problems  []scanProblem
}

type scanStats struct {
	Series int    `json:"series"`
	Books  int    `json:"books"`
	Pages  int    `json:"pages"`
	Bytes  int64  `json:"bytes"`
}

const maxScanProblems = 500

// scanProgress holds live counters published to the admin page during a scan.
type scanProgress struct {
	dirs   atomic.Int64
	series atomic.Int64
	books  atomic.Int64
}

// scanState carries the previous scan's books (to skip recounting unchanged
// archives), the progress counters and the problem list into the walk.
type scanState struct {
	prev     map[string]*Book
	prog     *scanProgress
	problems []scanProblem
}

func (st *scanState) dirDone() {
	if st.prog != nil {
		st.prog.dirs.Add(1)
	}
}

func (st *scanState) seriesFound(n int) {
	if st.prog != nil {
		st.prog.series.Add(int64(n))
	}
}

func (st *scanState) booksFound(n int) {
	if st.prog != nil {
		st.prog.books.Add(int64(n))
	}
}

func (st *scanState) problem(rel, p, reason string) {
	if len(st.problems) >= maxScanProblems {
		return
	}
	st.problems = append(st.problems, scanProblem{Rel: rel, Path: p, Reason: reason})
}

func newLibrary(roots []string, name string) *library {
	return &library{roots: roots, name: name, books: map[string]*Book{}}
}

// scan walks every granted root. With a single root the series rel-paths stay
// unprefixed (0.2.x compatibility, keeps existing permission and scrape IDs);
// with several roots each series path is prefixed by the root's base name so
// library boundaries, permission grants and feed paths stay unambiguous.
func (l *library) scan(prev map[string]*Book, prog *scanProgress) {
	st := &scanState{prev: prev, prog: prog}
	var series []*Series
	for _, root := range l.roots {
		prefix := ""
		if len(l.roots) > 1 {
			prefix = filepath.Base(root)
		}
		s, loose := scanDir(root, prefix, 0, st)
		series = append(series, s...)
		if len(loose) > 0 {
			// Single-root keeps the 0.2.x shape ("/", library name) so cached
			// permission and scrape IDs stay valid; multi-root names the
			// series after the library root, grantable as a whole.
			relKey := prefix
			name := l.name
			if prefix != "" {
				name = prefix
			} else {
				relKey = "/"
			}
			sr := &Series{ID: hashID("series:" + relKey), Name: name, Rel: relKey}
			l.attach(sr, loose)
			series = append(series, sr)
			st.seriesFound(1)
			st.booksFound(len(loose))
		}
	}
	sort.Slice(series, func(i, j int) bool { return naturalLess(series[i].Name, series[j].Name) })

	books := map[string]*Book{}
	stats := scanStats{}
	for _, s := range series {
		stats.Series++
		stats.Books += len(s.Books)
		if s.Cover == "" && len(s.Books) > 0 {
			s.Cover = s.Books[0].ID
		}
		for _, b := range s.Books {
			b.SeriesID = s.ID
			books[b.ID] = b
			stats.Pages += b.Pages
			stats.Bytes += b.Size
		}
	}

	l.mu.Lock()
	l.series = series
	l.books = books
	l.stats = stats
	l.problems = st.problems
	l.scannedAt = time.Now()
	l.mu.Unlock()
}

func (l *library) attach(s *Series, books []*Book) {
	for _, b := range books {
		b.SeriesID = s.ID
		s.Books = append(s.Books, b)
	}
	sort.Slice(s.Books, func(i, j int) bool { return naturalLess(s.Books[i].Title, s.Books[j].Title) })
}

func (l *library) snapshot() ([]*Series, map[string]*Book, time.Time) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.series, l.books, l.scannedAt
}

func (l *library) problemsSnapshot() []scanProblem {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]scanProblem, len(l.problems))
	copy(out, l.problems)
	return out
}

func (l *library) findBook(id string) *Book {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.books[id]
}

func (l *library) rootCount() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.roots)
}

func (l *library) findSeries(id string) *Series {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, s := range l.series {
		if s.ID == id {
			return s
		}
	}
	return nil
}

// booksByPath snapshots the current books keyed by absolute path, for reuse
// as the prev map of the next scan.
func (l *library) booksByPath() map[string]*Book {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make(map[string]*Book, len(l.books))
	for _, b := range l.books {
		out[b.Path] = b
	}
	return out
}

const (
	libraryCacheVersion = 2
	// legacyCacheVersion is the single-root format of 0.1.3–0.2.0.
	legacyCacheVersion = 1
)

type libraryCache struct {
	Version   int           `json:"version"`
	Roots     []string      `json:"roots,omitempty"`
	Root      string        `json:"root,omitempty"` // v1 single root
	ScannedAt time.Time     `json:"scanned_at"`
	Stats     scanStats     `json:"stats"`
	Series    []*Series     `json:"series"`
	Problems  []scanProblem `json:"problems,omitempty"`
}

func libraryCachePath(dataDir string) string { return filepath.Join(dataDir, "library.json") }

func (l *library) saveCache(file string) error {
	l.mu.RLock()
	c := libraryCache{
		Version:   libraryCacheVersion,
		Roots:     l.roots,
		ScannedAt: l.scannedAt,
		Stats:     l.stats,
		Series:    l.series,
		Problems:  l.problems,
	}
	l.mu.RUnlock()
	raw, err := json.MarshalIndent(&c, "", "  ")
	if err != nil {
		return err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// loadLibraryCache restores the library from a previous scan so restarts do
// not wait for a full rescan. It returns nil when there is no cache, when it
// was written for a different set of roots, or when it holds nothing to serve.
func loadLibraryCache(file string, roots []string, name string) *library {
	if len(roots) == 0 {
		return nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var c libraryCache
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil
	}
	if len(c.Series) == 0 {
		return nil
	}
	cachedRoots := c.Roots
	if c.Version == legacyCacheVersion && c.Root != "" {
		cachedRoots = []string{c.Root}
	}
	if !sameStrings(cachedRoots, roots) {
		return nil
	}
	l := &library{
		roots:     roots,
		name:      name,
		series:    c.Series,
		books:     map[string]*Book{},
		scannedAt: c.ScannedAt,
		stats:     c.Stats,
		problems:  c.Problems,
	}
	for _, s := range c.Series {
		for _, b := range s.Books {
			b.SeriesID = s.ID
			l.books[b.ID] = b
		}
	}
	return l
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// scanDir returns the series found under abs plus loose image-directory books
// that belong to the parent's series. rel is the path relative to (and
// prefixed with) the library root.
func scanDir(abs, rel string, depth int, st *scanState) ([]*Series, []*Book) {
	if depth > 8 {
		return nil, nil
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		st.problem(rel, abs, "目录无法读取："+err.Error())
		return nil, nil
	}
	st.dirDone()

	var archives, images []os.DirEntry
	var subdirs []os.DirEntry
	for _, e := range entries {
		name := e.Name()
		if ignoreEntry(name) {
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		if e.IsDir() {
			subdirs = append(subdirs, e)
			continue
		}
		if archiveFormat(name) != "" {
			archives = append(archives, e)
		} else if isImageName(name) {
			images = append(images, e)
		}
	}

	var out []*Series
	var loose []*Book

	if len(archives) > 0 {
		s := &Series{ID: hashID("series:" + rel), Name: filepath.Base(abs), Rel: rel}
		var books []*Book
		for _, a := range archives {
			books = append(books, makeBook(abs, path.Join(rel, a.Name()), a.Name(), archiveFormat(a.Name()), st))
		}
		sort.Slice(books, func(i, j int) bool { return naturalLess(books[i].Title, books[j].Title) })
		s.Books = books
		out = append(out, s)
		st.seriesFound(1)
		st.booksFound(len(books))
	}

	if len(images) > 0 && len(archives) == 0 {
		sort.Slice(images, func(i, j int) bool { return naturalLess(images[i].Name(), images[j].Name()) })
		stat, _ := images[0].Info()
		var size int64
		for _, img := range images {
			if st2, err := img.Info(); err == nil {
				size += st2.Size()
			}
		}
		var mod time.Time
		if stat != nil {
			mod = stat.ModTime()
		}
		loose = append(loose, &Book{
			ID:      hashID("book:" + rel),
			Title:   filepath.Base(abs),
			Rel:     rel,
			Path:    abs,
			Format:  "dir",
			Size:    size,
			Pages:   len(images),
			ModTime: mod,
		})
		st.booksFound(1)
	}

	for _, d := range subdirs {
		childRel := path.Join(rel, d.Name())
		cs, cl := scanDir(filepath.Join(abs, d.Name()), childRel, depth+1, st)
		out = append(out, cs...)
		if len(cl) > 0 {
			s := &Series{ID: hashID("series:" + childRel), Name: d.Name(), Rel: childRel}
			for _, b := range cl {
				b.SeriesID = s.ID
				s.Books = append(s.Books, b)
			}
			out = append(out, s)
			st.seriesFound(1)
			st.booksFound(len(cl))
		}
	}

	return out, loose
}

func makeBook(dir, rel, name, format string, st *scanState) *Book {
	full := filepath.Join(dir, name)
	b := &Book{
		ID:      hashID("book:" + rel),
		Title:   strings.TrimSuffix(name, filepath.Ext(name)),
		Rel:     rel,
		Path:    full,
		Format:  format,
		ModTime: time.Now(),
	}
	if info, err := os.Stat(full); err == nil {
		b.Size = info.Size()
		b.ModTime = info.ModTime()
	}
	if format == "cbz" || format == "zip" || format == "cb7" || format == "epub" {
		// An unchanged archive keeps both its page count and its extracted
		// metadata; archives without metadata cache an empty marker so they
		// are not reopened on every scan either.
		if old := prevBook(st, full, b.ModTime, b.Size); old != nil && old.Meta != nil {
			b.Pages = old.Pages
			b.Meta = old.Meta
		} else {
			pages, err := countZipPages(full)
			if err != nil {
				st.problem(rel, full, "压缩包无法读取："+err.Error())
			}
			b.Pages = pages
			b.Meta = archiveMetadata(full, format)
		}
	}
	return b
}

// prevBook returns the previous scan's book for an unchanged archive.
func prevBook(st *scanState, file string, mod time.Time, size int64) *Book {
	if st == nil || st.prev == nil {
		return nil
	}
	old := st.prev[file]
	if old != nil && old.ModTime.Equal(mod) && old.Size == size {
		return old
	}
	return nil
}

func countZipPages(file string) (int, error) {
	zr, err := zip.OpenReader(file)
	if err != nil {
		return 0, err
	}
	defer zr.Close()
	n := 0
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := f.Name
		if strings.HasPrefix(name, "__MACOSX/") || strings.HasPrefix(filepath.Base(name), ".") {
			continue
		}
		if isImageName(name) {
			n++
		}
	}
	return n, nil
}

// naturalLess compares strings so that "卷2" sorts before "卷10".
func naturalLess(a, b string) bool {
	la, lb := strings.ToLower(a), strings.ToLower(b)
	i, j := 0, 0
	for i < len(la) && j < len(lb) {
		if isDigit(la[i]) && isDigit(lb[j]) {
			var ni, nj int
			for i < len(la) && isDigit(la[i]) {
				ni = ni*10 + int(la[i]-'0')
				i++
			}
			for j < len(lb) && isDigit(lb[j]) {
				nj = nj*10 + int(lb[j]-'0')
				j++
			}
			if ni != nj {
				return ni < nj
			}
			continue
		}
		if la[i] != lb[j] {
			return la[i] < lb[j]
		}
		i++
		j++
	}
	return len(la)-i < len(lb)-j
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
