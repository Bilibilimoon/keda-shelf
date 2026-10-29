package main

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	atomNS       = "http://www.w3.org/2005/Atom"
	opdsNS       = "http://opds-spec.org/2010/catalog"
	relAcquire   = "http://opds-spec.org/acquisition"
	relImage     = "http://opds-spec.org/image"
	relThumbnail = "http://opds-spec.org/image/thumbnail"
	relSubsec    = "subsection"

	kindNav   = "application/atom+xml;profile=opds-catalog;kind=navigation"
	kindAcq   = "application/atom+xml;profile=opds-catalog;kind=acquisition"
	opdsRoot  = "/opds/v1.2"
	searchRel = "search"
)

type opdsLink struct {
	Rel    string `xml:"rel,attr"`
	Href   string `xml:"href,attr"`
	Type   string `xml:"type,attr,omitempty"`
	Title  string `xml:"title,attr,omitempty"`
	Length int64  `xml:"length,attr,omitempty"`
}

type opdsAuthor struct {
	Name string `xml:"name"`
	URI  string `xml:"uri,omitempty"`
}

type opdsContent struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}

type opdsEntry struct {
	Title      string         `xml:"title"`
	ID         string         `xml:"id"`
	Updated    string         `xml:"updated"`
	Authors    []opdsAuthor   `xml:"author,omitempty"`
	Content    opdsContent    `xml:"content"`
	Categories []opdsCategory `xml:"category,omitempty"`
	Links      []opdsLink     `xml:"link"`
}

type opdsCategory struct {
	Term string `xml:"term,attr"`
}

type opdsFeed struct {
	XMLName xml.Name    `xml:"feed"`
	Xmlns   string      `xml:"xmlns,attr"`
	XmlnsDC string      `xml:"xmlns:dc,attr,omitempty"`
	ID      string      `xml:"id"`
	Title   string      `xml:"title"`
	Updated string      `xml:"updated"`
	Author  *opdsAuthor `xml:"author,omitempty"`
	Links   []opdsLink  `xml:"link"`
	Entries []opdsEntry `xml:"entry"`
}

func (s *server) handleOPDS(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case p == "/opds" || p == opdsRoot || p == opdsRoot+"/catalog":
		s.serveRootFeed(w, r)
	case p == opdsRoot+"/series":
		s.serveSeriesList(w, r)
	case strings.HasPrefix(p, opdsRoot+"/series/"):
		s.serveSeriesRoute(w, r, strings.TrimPrefix(p, opdsRoot+"/series/"))
	case p == opdsRoot+"/recent":
		s.serveRecent(w, r)
	case strings.HasPrefix(p, opdsRoot+"/lib/"):
		name, err := url.PathUnescape(strings.TrimPrefix(p, opdsRoot+"/lib/"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		s.serveLibraryFeed(w, r, name)
	case strings.HasPrefix(p, opdsRoot+"/books/"):
		s.serveBookRoute(w, r, strings.TrimPrefix(p, opdsRoot+"/books/"))
	case p == opdsRoot+"/search":
		s.serveSearch(w, r)
	case p == opdsRoot+"/opensearch.xml":
		s.serveOpenSearch(w, r)
	default:
		http.NotFound(w, r)
	}
}

func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// dirPerm carries the directory subtrees (paths relative to the library
// root) a restricted user may see; nil means unrestricted (gateway users and
// unrestricted Basic Auth users). A grant on a directory covers everything
// beneath it, including series added after the grant was saved.
type dirPerm struct {
	dirs []string
}

func (p *dirPerm) ok(rel string) bool {
	if p == nil {
		return true
	}
	for _, d := range p.dirs {
		if rel == d || strings.HasPrefix(rel, d+"/") {
			return true
		}
	}
	return false
}

func reqPerm(r *http.Request) *dirPerm {
	u := userFrom(r)
	if u == nil || !u.Restricted {
		return nil
	}
	return &dirPerm{dirs: u.Dirs}
}

func feedUpdated(series []*Series) string {
	var latest time.Time
	for _, sr := range series {
		for _, b := range sr.Books {
			if b.ModTime.After(latest) {
				latest = b.ModTime
			}
		}
	}
	if latest.IsZero() {
		latest = time.Now()
	}
	return latest.UTC().Format(time.RFC3339)
}

func (s *server) newFeed(r *http.Request, id, title, kind string) *opdsFeed {
	now := time.Now().UTC().Format(time.RFC3339)
	return &opdsFeed{
		Xmlns:   atomNS,
		XmlnsDC: "http://purl.org/dc/terms/",
		ID:      "urn:mangagate:" + id,
		Title:   title,
		Updated: now,
		Author:  &opdsAuthor{Name: "可达书柜"},
		Links: []opdsLink{
			{Rel: "self", Href: baseURL(r) + r.URL.RequestURI(), Type: kind},
			{Rel: "start", Href: baseURL(r) + opdsRoot + "/catalog", Type: kindNav},
			{Rel: searchRel, Href: baseURL(r) + opdsRoot + "/search?q={searchTerms}", Type: kindAcq},
			{Rel: "http://opds-spec.org/search", Href: baseURL(r) + opdsRoot + "/opensearch.xml", Type: "application/opensearchdescription+xml"},
		},
	}
}

// pageEntries slices entries into pages and attaches standard next/previous
// links, replacing the old hard truncation.
func pageEntries(r *http.Request, f *opdsFeed, entries []opdsEntry, per int) []opdsEntry {
	page := 1
	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	pages := (len(entries) + per - 1) / per
	if pages < 1 {
		pages = 1
	}
	start := (page - 1) * per
	if start > len(entries) {
		start = len(entries)
	}
	end := start + per
	if end > len(entries) {
		end = len(entries)
	}
	q := r.URL.Query()
	link := func(rel string, n int) {
		q.Set("page", itoa(n))
		f.Links = append(f.Links, opdsLink{
			Rel: rel, Href: baseURL(r) + r.URL.Path + "?" + q.Encode(), Type: kindNav,
		})
	}
	if page < pages {
		link("next", page+1)
	}
	if page > 1 {
		link("previous", page-1)
	}
	return entries[start:end]
}

func (s *server) serveRootFeed(w http.ResponseWriter, r *http.Request) {
	series, _, at := s.lib.snapshot()
	perm := reqPerm(r)
	f := s.newFeed(r, "root", s.cfg.LibraryName, kindNav)
	f.Updated = at.UTC().Format(time.RFC3339)
	visible := 0
	var visibleEntries []opdsEntry
	for _, sr := range series {
		if !perm.ok(sr.Rel) {
			continue
		}
		visible++
		visibleEntries = append(visibleEntries, s.seriesEntry(r, sr))
	}
	f.Entries = append(f.Entries, opdsEntry{
		Title:   "全部系列",
		ID:      "urn:mangagate:all-series",
		Updated: f.Updated,
		Content: opdsContent{Type: "text", Value: fmt.Sprintf("%d 个系列", visible)},
		Links: []opdsLink{
			{Rel: relSubsec, Href: baseURL(r) + opdsRoot + "/series", Type: kindNav},
		},
	})
	f.Entries = append(f.Entries, opdsEntry{
		Title:   "最近添加",
		ID:      "urn:mangagate:recent",
		Updated: f.Updated,
		Content: opdsContent{Type: "text", Value: "最新入库的 50 本"},
		Links: []opdsLink{
			{Rel: relSubsec, Href: baseURL(r) + opdsRoot + "/recent", Type: kindAcq},
		},
	})
	// With several granted roots each library gets its own entry point; a
	// restricted user only sees libraries with at least one visible series.
	for _, rt := range s.roots {
		base := filepath.Base(rt)
		n := 0
		for _, sr := range series {
			if sr.Rel != base && !strings.HasPrefix(sr.Rel, base+"/") {
				continue
			}
			if perm.ok(sr.Rel) {
				n++
			}
		}
		if n == 0 {
			continue
		}
		f.Entries = append(f.Entries, opdsEntry{
			Title:   base,
			ID:      "urn:mangagate:lib:" + base,
			Updated: f.Updated,
			Content: opdsContent{Type: "text", Value: fmt.Sprintf("%d 个系列", n)},
			Links: []opdsLink{
				{Rel: relSubsec, Href: baseURL(r) + opdsRoot + "/lib/" + url.PathEscape(base), Type: kindNav},
			},
		})
	}
	f.Entries = append(f.Entries, pageEntries(r, f, visibleEntries, 100)...)
	writeXML(w, f, kindNav)
}

// serveLibraryFeed lists the series belonging to one library root.
func (s *server) serveLibraryFeed(w http.ResponseWriter, r *http.Request, name string) {
	valid := false
	for _, rt := range s.roots {
		if filepath.Base(rt) == name {
			valid = true
			break
		}
	}
	if !valid {
		http.NotFound(w, r)
		return
	}
	series, _, at := s.lib.snapshot()
	perm := reqPerm(r)
	f := s.newFeed(r, "lib:"+name, name, kindNav)
	f.Updated = at.UTC().Format(time.RFC3339)
	var entries []opdsEntry
	for _, sr := range series {
		if sr.Rel != name && !strings.HasPrefix(sr.Rel, name+"/") {
			continue
		}
		if !perm.ok(sr.Rel) {
			continue
		}
		entries = append(entries, s.seriesEntry(r, sr))
	}
	f.Entries = append(f.Entries, pageEntries(r, f, entries, 100)...)
	writeXML(w, f, kindNav)
}

// serveRecent lists the newest books across all libraries, newest first.
func (s *server) serveRecent(w http.ResponseWriter, r *http.Request) {
	series, _, at := s.lib.snapshot()
	perm := reqPerm(r)
	var books []*Book
	for _, sr := range series {
		if !perm.ok(sr.Rel) {
			continue
		}
		books = append(books, sr.Books...)
	}
	sort.Slice(books, func(i, j int) bool { return books[i].ModTime.After(books[j].ModTime) })
	if len(books) > 50 {
		books = books[:50]
	}
	f := s.newFeed(r, "recent", "最近添加", kindAcq)
	f.Updated = at.UTC().Format(time.RFC3339)
	for _, b := range books {
		f.Entries = append(f.Entries, s.bookEntry(r, b))
	}
	writeXML(w, f, kindAcq)
}

func (s *server) seriesEntry(r *http.Request, sr *Series) opdsEntry {
	cover := sr.Cover
	if cover == "" && len(sr.Books) > 0 {
		cover = sr.Books[0].ID
	}
	title := sr.Name
	content := fmt.Sprintf("%d 本", len(sr.Books))
	var authors []opdsAuthor
	var cats []opdsCategory
	if info := s.meta.series(sr.ID); info != nil {
		if info.Title != "" {
			title = info.Title
		}
		if info.Summary != "" {
			content = info.Summary
		}
		authors = authorEntries(info.Authors)
		cats = categoryEntries(info.Tags)
	}
	e := opdsEntry{
		Title:      title,
		ID:         "urn:mangagate:series:" + sr.ID,
		Updated:    feedUpdated([]*Series{sr}),
		Authors:    authors,
		Categories: cats,
		Content:    opdsContent{Type: "text", Value: content},
		Links: []opdsLink{
			{Rel: relSubsec, Href: baseURL(r) + opdsRoot + "/series/" + sr.ID, Type: kindAcq},
		},
	}
	if cover != "" {
		thumb := baseURL(r) + opdsRoot + "/books/" + cover + "/thumbnail"
		orig := baseURL(r) + opdsRoot + "/books/" + cover + "/cover"
		e.Links = append(e.Links,
			opdsLink{Rel: relImage, Href: orig, Type: "image/jpeg"},
			opdsLink{Rel: relThumbnail, Href: thumb, Type: "image/jpeg"},
		)
	}
	return e
}

func (s *server) serveSeriesList(w http.ResponseWriter, r *http.Request) {
	series, _, at := s.lib.snapshot()
	perm := reqPerm(r)
	f := s.newFeed(r, "series", "全部系列", kindNav)
	f.Updated = at.UTC().Format(time.RFC3339)
	var entries []opdsEntry
	for _, sr := range series {
		if !perm.ok(sr.Rel) {
			continue
		}
		entries = append(entries, s.seriesEntry(r, sr))
	}
	f.Entries = append(f.Entries, pageEntries(r, f, entries, 100)...)
	writeXML(w, f, kindNav)
}

func (s *server) serveSeriesRoute(w http.ResponseWriter, r *http.Request, rest string) {
	rest = strings.Trim(rest, "/")
	perm := reqPerm(r)
	if strings.HasSuffix(rest, "/thumbnail") {
		id := strings.TrimSuffix(rest, "/thumbnail")
		sr := s.lib.findSeries(id)
		if sr == nil || !perm.ok(sr.Rel) {
			http.NotFound(w, r)
			return
		}
		cover := sr.Cover
		if cover == "" && len(sr.Books) > 0 {
			cover = sr.Books[0].ID
		}
		s.serveCover(w, r, s.lib.findBook(cover))
		return
	}
	if strings.HasSuffix(rest, "/cover") {
		id := strings.TrimSuffix(rest, "/cover")
		sr := s.lib.findSeries(id)
		if sr == nil || !perm.ok(sr.Rel) {
			http.NotFound(w, r)
			return
		}
		cover := sr.Cover
		if cover == "" && len(sr.Books) > 0 {
			cover = sr.Books[0].ID
		}
		s.serveOriginalCover(w, r, s.lib.findBook(cover))
		return
	}
	sr := s.lib.findSeries(rest)
	if sr == nil || !perm.ok(sr.Rel) {
		http.NotFound(w, r)
		return
	}
	f := s.newFeed(r, "series:"+sr.ID, sr.Name, kindAcq)
	f.Updated = feedUpdated([]*Series{sr})
	for _, b := range sr.Books {
		f.Entries = append(f.Entries, s.bookEntry(r, b))
	}
	writeXML(w, f, kindAcq)
}

func (s *server) bookEntry(r *http.Request, b *Book) opdsEntry {
	typ, size := acquisitionInfo(b)
	// Scraped info overrides embedded metadata, embedded beats the file name.
	title := b.Title
	summary := ""
	var authors []opdsAuthor
	var cats []opdsCategory
	if b.Meta != nil {
		if b.Meta.Title != "" {
			title = b.Meta.Title
		}
		summary = b.Meta.Summary
		authors = authorEntries(b.Meta.Authors)
		cats = categoryEntries(b.Meta.Subjects)
	}
	if info := s.meta.book(b.ID); info != nil {
		if info.Title != "" {
			title = info.Title
		}
		if info.Summary != "" {
			summary = info.Summary
		}
		if len(info.Authors) > 0 {
			authors = authorEntries(info.Authors)
		}
		if len(info.Tags) > 0 {
			cats = categoryEntries(info.Tags)
		}
	}
	content := bookDescription(b)
	if summary != "" {
		content = summary + "\n" + bookDescription(b)
	}
	e := opdsEntry{
		Title:      title,
		ID:         "urn:mangagate:book:" + b.ID,
		Updated:    b.ModTime.UTC().Format(time.RFC3339),
		Authors:    authors,
		Categories: cats,
		Content:    opdsContent{Type: "text", Value: content},
		Links: []opdsLink{
			{Rel: relAcquire, Href: baseURL(r) + opdsRoot + "/books/" + b.ID + "/file", Type: typ, Length: size},
			{Rel: relImage, Href: baseURL(r) + opdsRoot + "/books/" + b.ID + "/cover", Type: "image/jpeg"},
			{Rel: relThumbnail, Href: baseURL(r) + opdsRoot + "/books/" + b.ID + "/thumbnail", Type: "image/jpeg"},
		},
	}
	return e
}

func authorEntries(names []string) []opdsAuthor {
	var out []opdsAuthor
	for _, n := range names {
		if n != "" {
			out = append(out, opdsAuthor{Name: n})
		}
	}
	return out
}

func categoryEntries(tags []string) []opdsCategory {
	var out []opdsCategory
	for _, t := range tags {
		if t != "" {
			out = append(out, opdsCategory{Term: t})
		}
	}
	return out
}

func acquisitionInfo(b *Book) (string, int64) {
	if b.Format == "dir" {
		return "application/zip", 0
	}
	return opdsTypeFor(b.Path), b.Size
}

func bookDescription(b *Book) string {
	parts := []string{}
	if b.Pages > 0 {
		parts = append(parts, fmt.Sprintf("%d 页", b.Pages))
	}
	if b.Size > 0 {
		parts = append(parts, humanSize(b.Size))
	}
	if b.Format != "" {
		parts = append(parts, strings.ToUpper(b.Format))
	}
	return strings.Join(parts, " · ")
}

func (s *server) serveBookRoute(w http.ResponseWriter, r *http.Request, rest string) {
	rest = strings.Trim(rest, "/")
	perm := reqPerm(r)
	if strings.HasSuffix(rest, "/file") {
		s.serveBookFile(w, r, perm, strings.TrimSuffix(rest, "/file"))
		return
	}
	if strings.HasSuffix(rest, "/thumbnail") {
		id := strings.TrimSuffix(rest, "/thumbnail")
		b := s.lib.findBook(id)
		if b == nil || !perm.ok(b.Rel) {
			http.NotFound(w, r)
			return
		}
		s.serveCover(w, r, b)
		return
	}
	if strings.HasSuffix(rest, "/cover") {
		id := strings.TrimSuffix(rest, "/cover")
		b := s.lib.findBook(id)
		if b == nil || !perm.ok(b.Rel) {
			http.NotFound(w, r)
			return
		}
		s.serveOriginalCover(w, r, b)
		return
	}
	b := s.lib.findBook(rest)
	if b == nil || !perm.ok(b.Rel) {
		http.NotFound(w, r)
		return
	}
	f := s.newFeed(r, "book:"+b.ID, b.Title, kindAcq)
	f.Updated = b.ModTime.UTC().Format(time.RFC3339)
	f.Entries = append(f.Entries, s.bookEntry(r, b))
	writeXML(w, f, kindAcq)
}

func (s *server) serveCover(w http.ResponseWriter, r *http.Request, b *Book) {
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	var raw []byte
	if raw = s.storedThumbnail(b); len(raw) == 0 {
		raw = coverJPEG(b, s.dataDir)
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(raw)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(raw)
	}
}

// serveOriginalCover answers the OPDS "image" relation with the full-size
// cover: the scraped original when present, otherwise the cover extracted
// from the book itself. It falls back to the thumbnail when no original is
// available so clients following the link still get a picture.
func (s *server) serveOriginalCover(w http.ResponseWriter, r *http.Request, b *Book) {
	raw, ctype := s.originalCover(b)
	if len(raw) == 0 {
		raw, ctype = coverJPEG(b, s.dataDir), "image/jpeg"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(raw)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(raw)
	}
}

// storedCoverFile returns the base name of the highest-priority stored cover
// for the book: an admin-supplied image (book first, then series) beats a
// scraped one (same order). Stored images are served through their own
// endpoints and never go through the book extractor.
func (s *server) storedCoverFile(b *Book) string {
	if b == nil {
		return ""
	}
	if c := s.meta.manual("book", b.ID); c != nil && c.File != "" {
		return filepath.Base(c.File)
	}
	if c := s.meta.manual("series", b.SeriesID); c != nil && c.File != "" {
		return filepath.Base(c.File)
	}
	if info := s.meta.book(b.ID); info != nil && info.Cover != "" {
		return filepath.Base(info.Cover)
	}
	if info := s.meta.series(b.SeriesID); info != nil && info.Cover != "" {
		return filepath.Base(info.Cover)
	}
	return ""
}

// originalCover returns the stored cover when the book or its series has a
// manual or scraped match, otherwise the full-size cover extracted from the
// book itself.
func (s *server) originalCover(b *Book) ([]byte, string) {
	if b == nil {
		return nil, ""
	}
	if file := s.storedCoverFile(b); file != "" {
		// Covers live flat under covers/; Base keeps a tampered sidecar file
		// from pointing outside that directory.
		if raw, err := os.ReadFile(filepath.Join(coverDir(s.dataDir), file)); err == nil && len(raw) > 0 {
			return raw, detectImageType(raw)
		}
	}
	if raw := coverOriginalJPEG(b, s.dataDir); len(raw) > 0 {
		return raw, "image/jpeg"
	}
	return nil, ""
}

// storedThumbnail serves the thumbnail of a stored cover (manual first, then
// scraped); unlike the original it is always a small JPEG, generated from the
// stored image on first use for covers downloaded before thumbnails were kept.
func (s *server) storedThumbnail(b *Book) []byte {
	if b == nil {
		return nil
	}
	file := s.storedCoverFile(b)
	if file == "" {
		return nil
	}
	return storedCoverThumbnail(coverDir(s.dataDir), file)
}

// storedCoverThumbnail reads the pre-generated thumbnail of one stored cover,
// lazily generating and caching it when an older version stored only the
// original. It returns nil when the image is missing or undecodable.
func storedCoverThumbnail(dir, file string) []byte {
	orig := filepath.Join(dir, filepath.Base(file))
	thumb := filepath.Join(dir, thumbName(filepath.Base(file)))
	if raw, err := os.ReadFile(thumb); err == nil && len(raw) > 0 {
		return raw
	}
	raw, err := os.ReadFile(orig)
	if err != nil || len(raw) == 0 {
		return nil
	}
	img, err := decodeLimited(bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	out := encodeCover(img)
	if len(out) == 0 {
		return nil
	}
	_ = os.WriteFile(thumb, out, 0o644)
	return out
}

// detectImageType sniffs the image format of raw bytes, defaulting to JPEG.
func detectImageType(raw []byte) string {
	if t := http.DetectContentType(raw); strings.HasPrefix(t, "image/") {
		return t
	}
	return "image/jpeg"
}

func (s *server) serveBookFile(w http.ResponseWriter, r *http.Request, perm *dirPerm, id string) {
	b := s.lib.findBook(id)
	if b == nil || !perm.ok(b.Rel) {
		http.NotFound(w, r)
		return
	}
	name := filepath.Base(b.Path)
	if b.Format == "dir" {
		if b.Rel == "" {
			name = b.Title
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", contentDisposition(name+".zip"))
		if r.Method == http.MethodHead {
			return
		}
		zw := zip.NewWriter(w)
		defer zw.Close()
		entries, err := os.ReadDir(b.Path)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() || ignoreEntry(e.Name()) || !isImageName(e.Name()) {
				continue
			}
			f, err := zw.Create(e.Name())
			if err != nil {
				continue
			}
			src, err := os.Open(filepath.Join(b.Path, e.Name()))
			if err != nil {
				continue
			}
			_, _ = io.Copy(f, src)
			src.Close()
		}
		return
	}

	f, err := os.Open(b.Path)
	if err != nil {
		http.Error(w, "file not available", http.StatusNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, "file not available", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", opdsTypeFor(b.Path))
	w.Header().Set("Content-Disposition", contentDisposition(name))
	w.Header().Set("Accept-Ranges", "bytes")
	// ServeContent handles Range requests, which the reader relies on for streaming.
	http.ServeContent(w, r, name, st.ModTime(), f)
}

func contentDisposition(name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 32 || r > 126 || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	return fmt.Sprintf("inline; filename=\"%s\"; filename*=UTF-8''%s", ascii, url.PathEscape(name))
}

func (s *server) serveSearch(w http.ResponseWriter, r *http.Request) {
	series, _, at := s.lib.snapshot()
	perm := reqPerm(r)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	f := s.newFeed(r, "search", "搜索："+q, kindAcq)
	f.Updated = at.UTC().Format(time.RFC3339)
	terms := searchTerms(q)
	if len(terms) == 0 {
		writeXML(w, f, kindAcq)
		return
	}
	var entries []opdsEntry
	for _, sr := range series {
		if !perm.ok(sr.Rel) {
			continue
		}
		for _, b := range sr.Books {
			if matchesAllTerms(s.bookSearchWithScrape(sr, b), terms) {
				entries = append(entries, s.bookEntry(r, b))
			}
		}
	}
	f.Entries = append(f.Entries, pageEntries(r, f, entries, 100)...)
	writeXML(w, f, kindAcq)
}

func (s *server) serveOpenSearch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/opensearchdescription+xml; charset=utf-8")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<OpenSearchDescription xmlns="http://a9.com/-/spec/opensearch/1.1/">
  <ShortName>可达书柜</ShortName>
  <Description>搜索书柜</Description>
  <InputEncoding>UTF-8</InputEncoding>
  <Url type="application/atom+xml;profile=opds-catalog;kind=acquisition" template="%s/opds/v1.2/search?q={searchTerms}"/>
</OpenSearchDescription>`, baseURL(r))
}

func writeXML(w http.ResponseWriter, v any, kind string) {
	raw, err := xml.MarshalIndent(v, "", "  ")
	if err != nil {
		http.Error(w, "feed error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", kind+"; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(raw)
}
