package main

import (
	"archive/zip"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
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
	Series int   `json:"series"`
	Books  int   `json:"books"`
	Pages  int   `json:"pages"`
	Bytes  int64 `json:"bytes"`
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
		res := scanDir(root, prefix, 0, scanOpts{root: true, libName: l.name}, st)
		series = append(series, res.series...)
		// Image directories directly in the granted folder keep the 0.2.x
		// shape: each is a series of its own unless the folder itself holds
		// images, in which case they all join the library's "/" series.
		var loose []*Book
		if rootHasImages := len(res.books) > 0 && res.books[0].Rel == prefix; rootHasImages {
			loose = res.books
		} else {
			for _, b := range res.books {
				sr := makeSeries(hashID("series:"+b.Rel), b.Title, b.Rel)
				sr.Books = []*Book{b}
				series = append(series, sr)
				st.seriesFound(1)
			}
		}
		if len(loose) > 0 {
			// Loose books (images or volume directories sitting directly in
			// the root) stay under the unchanged 0.2.x key "/" so cached
			// permissions and scrape matches keep working; multi-root only
			// changes the shown name to the sub-library.
			name := l.name
			if prefix != "" {
				name = displayName(prefix)
			}
			sr := makeSeries(hashID("series:/"), name, "/")
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

// ownSeries returns the series scanDir built for the directory rel itself, as
// opposed to one found in its subdirectories.
func ownSeries(out []*Series, rel string) *Series {
	for _, s := range out {
		if s.Rel == rel {
			return s
		}
	}
	return nil
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

// fingerprint summarizes the current snapshot so an automatic rescan can tell
// a real change from a no-op and leave the served library (and its cache)
// untouched in the common case.
func (l *library) fingerprint() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	h := sha1.New()
	for _, s := range l.series {
		fmt.Fprintf(h, "S|%s|%s|%s\n", s.ID, s.Name, s.Rel)
		for _, b := range s.Books {
			fmt.Fprintf(h, "B|%s|%s|%s|%d|%d|%d|%v\n",
				b.ID, b.Title, b.Rel, b.Size, b.Pages, b.ModTime.UnixNano(), b.Meta)
		}
	}
	for _, p := range l.problems {
		fmt.Fprintf(h, "P|%s|%s\n", p.Rel, p.Reason)
	}
	return hex.EncodeToString(h.Sum(nil))
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

// scanWalk is what one directory contributes to the library: the series found
// inside it, plus image-directory books its parent may take as volumes.
// scanOpts carries the two things a directory needs to know about its place:
// whether it is the granted root, and whether the work above it takes every
// image directory below as one of its volumes.
type scanOpts struct {
	root bool
	// libName names a series built for the granted folder itself (its loose
	// archives), so it reads as the library instead of the folder name.
	libName string
	// absorb means the work above takes every image folder below as a volume.
	absorb bool
}

type scanWalk struct {
	series []*Series
	books  []*Book
	// adopted marks a directory that renamed a child work to its own name;
	// the adopting stops there so "合集2/别当欧尼酱了/KOX" keeps the middle
	// folder's name.
	adopted bool
}

// scanDir walks a directory tree. rel is the path relative to the library
// root; isRoot marks the granted directory itself.
//
// A directory holding archives is a work, and image directories below it are
// its volumes. A directory of nothing but image directories is a work made of
// volumes when those directories are named like volumes ("第01卷", "番外04",
// "BD特典1"); otherwise each of them is a work of its own, which keeps a
// classic "one folder per comic" library unchanged. A directory wrapping a
// single work adopts that work's books under its own name, so a release-group
// folder does not become the series, while a container of many works
// ("epub格式的漫画") never becomes one itself.
func scanDir(abs, rel string, depth int, opts scanOpts, st *scanState) scanWalk {
	if depth > 8 {
		return scanWalk{}
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		st.problem(rel, abs, "目录无法读取："+err.Error())
		return scanWalk{}
	}
	st.dirDone()

	var archives, images, subdirs []os.DirEntry
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

	var res scanWalk
	var subBooks []*Book
	var sole scanWalk
	soleOK := len(subdirs) == 1
	for _, d := range subdirs {
		// A work above takes every image folder below it as a volume. The
		// granted root is not a work, so its loose archives must not turn its
		// folders into volumes.
		child := scanDir(filepath.Join(abs, d.Name()), path.Join(rel, d.Name()), depth+1,
			scanOpts{absorb: !opts.root && (len(archives) > 0 || len(images) > 0)}, st)
		res.series = append(res.series, child.series...)
		subBooks = append(subBooks, child.books...)
		sole = child
	}

	name := displayName(filepath.Base(abs))
	if opts.root && opts.libName != "" {
		name = opts.libName
	}

	if len(archives) > 0 {
		if opts.absorb && len(archives) == 1 && len(images) == 0 && len(subBooks) == 0 && len(res.series) == 0 && isVolumeName(name) {
			// "第61話/第61话.pdf": a volume folder around a single file.
			b := makeBook(abs, path.Join(rel, archives[0].Name()), archives[0].Name(), archiveFormat(archives[0].Name()), st)
			st.booksFound(1)
			return scanWalk{books: []*Book{b}}
		}
		s := makeSeries(hashID("series:"+rel), name, rel)
		for _, a := range archives {
			s.Books = append(s.Books, makeBook(abs, path.Join(rel, a.Name()), a.Name(), archiveFormat(a.Name()), st))
		}
		if opts.root {
			// The granted folder is not a work: its archives form a series of
			// their own and the folders beside them stay separate.
			st.seriesFound(1)
			st.booksFound(len(s.Books))
			res.series = append(res.series, s)
			res.books = subBooks
			return res
		}
		s.Books = append(s.Books, subBooks...)
		sortBooks(s.Books)
		st.seriesFound(1)
		st.booksFound(len(s.Books))
		res.series = append(res.series, s)
		return res
	}

	// Images here make one book; the volumes below it are offered together.
	var offered []*Book
	if len(images) > 0 {
		offered = append(offered, imageDirBook(abs, rel, images))
	}
	offered = append(offered, subBooks...)

	if len(offered) == 0 {
		if soleOK && !sole.adopted && len(sole.series) == 1 && len(sole.books) == 0 && !opts.root {
			// A wrapper around a single work takes that work's books, so a
			// release-group folder does not become the series.
			s := makeSeries(hashID("series:"+rel), name, rel)
			s.Books = sole.series[0].Books
			res.series = []*Series{s}
			res.adopted = true
		}
		return res
	}

	if opts.root || opts.absorb || !looksLikeVolumes(offered) {
		// The caller decides: a work above takes these as its volumes, the
		// library itself turns each of them into a series of its own.
		st.booksFound(len(offered))
		res.books = offered
		return res
	}

	s := makeSeries(hashID("series:"+rel), name, rel)
	s.Books = offered
	sortBooks(s.Books)
	st.seriesFound(1)
	st.booksFound(len(s.Books))
	res.series = append(res.series, s)
	return res
}

// looksLikeVolumes reports whether these image-directory books are the volumes
// of one work rather than unrelated comics that happen to share a folder.
func looksLikeVolumes(books []*Book) bool {
	if len(books) < 2 {
		return false
	}
	n := 0
	for _, b := range books {
		if isVolumeName(b.Title) {
			n++
		}
	}
	return n*2 > len(books)
}

var (
	volPrefixRe = regexp.MustCompile(`(?i)^(第\s*[0-9零一二三四五六七八九十百千]+\s*[卷册冊話话回部集季]|(卷|冊|册|話|话|回|部|集)\s*[0-9]+|[0-9]+\s*[卷册冊話话回部集]|vol\.?\s*[0-9]+|ch(?:apter)?\.?\s*[0-9]+|番外|特典|extra|[0-9]+$)`)
	volWordRe   = regexp.MustCompile(`(?i)(番外|特典|設定集|设定集|画集|畫集|短篇|总集|總集|附录|附錄|连载|連載|更新至)`)
	sideStoryRe = regexp.MustCompile(`(?i)(番外|特典|短篇|画集|畫集|設定集|设定集|总集|總集)`)
)

// isVolumeName recognizes folder names that mark one volume of a serialized
// work: 第01卷, 番外04, BD特典1, 第06卷补图与通知, vol.3, 12.
func isVolumeName(name string) bool {
	n := strings.TrimSpace(name)
	if n == "" {
		return false
	}
	if volPrefixRe.MatchString(n) || volWordRe.MatchString(n) {
		return true
	}
	// "[Seaside Scanlations] Vol. 1 Ch. 1": the marker follows the group tag.
	stripped := strings.TrimSpace(bracketRe.ReplaceAllString(n, " "))
	if stripped == n {
		return false
	}
	return volPrefixRe.MatchString(stripped) || volWordRe.MatchString(stripped)
}

// sortBooks keeps a series in reading order: 第2卷 before 第10卷, and the
// side stories (番外, 特典) after the numbered volumes.
func sortBooks(books []*Book) {
	sort.SliceStable(books, func(i, j int) bool {
		ai, aj := volumeRank(books[i].Title), volumeRank(books[j].Title)
		if ai != aj {
			return ai < aj
		}
		return naturalLess(books[i].Title, books[j].Title)
	})
}

// volumeRank orders main volumes before side story volumes.
func volumeRank(name string) int {
	switch {
	case sideStoryRe.MatchString(name):
		return 1
	default:
		return 0
	}
}

// imageDirBook turns the images found directly in a directory into one book.
func imageDirBook(abs, rel string, images []os.DirEntry) *Book {
	sort.Slice(images, func(i, j int) bool { return naturalLess(images[i].Name(), images[j].Name()) })
	first, _ := images[0].Info()
	var size int64
	for _, img := range images {
		if info, err := img.Info(); err == nil {
			size += info.Size()
		}
	}
	var mod time.Time
	if first != nil {
		mod = first.ModTime()
	}
	return &Book{
		ID:      hashID("book:" + rel),
		Title:   displayName(filepath.Base(abs)),
		Rel:     rel,
		Path:    abs,
		Format:  "dir",
		Size:    size,
		Pages:   len(images),
		ModTime: mod,
	}
}

func makeBook(dir, rel, name, format string, st *scanState) *Book {
	full := filepath.Join(dir, name)
	b := &Book{
		ID:      hashID("book:" + rel),
		Title:   displayName(strings.TrimSuffix(name, filepath.Ext(name))),
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
		name := zipEntryName(f)
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

// makeSeries builds a series; kept in one place so the three fields cannot
// drift apart when the struct grows.
func makeSeries(id, name, rel string) *Series {
	return &Series{ID: id, Name: name, Rel: rel}
}
