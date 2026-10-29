package main

import (
	"embed"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

//go:embed admin.html
var adminFS embed.FS

func (s *server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "" {
		http.NotFound(w, r)
		return
	}
	raw, err := adminFS.ReadFile("admin.html")
	if err != nil {
		http.Error(w, "missing admin page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(raw)
}

type statusResp struct {
	Version      string            `json:"version"`
	Ready        bool              `json:"ready"`
	Roots        []string          `json:"roots"`
	LibraryName  string            `json:"library_name"`
	LANIP        string            `json:"lan_ip"`
	Port         int               `json:"port"`
	OPDSUrl      string            `json:"opds_url"`
	BaseURL      string            `json:"base_url"`
	Series       int               `json:"series"`
	Books        int               `json:"books"`
	Pages        int               `json:"pages"`
	Bytes        int64             `json:"bytes"`
	ScannedAt    string            `json:"scanned_at"`
	Scanning     bool              `json:"scanning"`
	ScanProgress *scanProgressView `json:"scan_progress,omitempty"`
	LastScan     *scanView         `json:"last_scan,omitempty"`
	Problems     int               `json:"problems"`
	TLS          bool              `json:"tls"`
	User         string            `json:"user"`
	IsAdmin      bool              `json:"is_admin"`
}

type scanResult struct {
	At         time.Time
	DurationMS int64
	Series     int
	Books      int
	Pages      int
	Bytes      int64
}

type scanView struct {
	At         string `json:"at"`
	DurationMS int64  `json:"duration_ms"`
	Series     int    `json:"series"`
	Books      int    `json:"books"`
	Pages      int    `json:"pages"`
	Bytes      int64  `json:"bytes"`
}

type scanProgressView struct {
	Dirs   int64 `json:"dirs"`
	Series int64 `json:"series"`
	Books  int64 `json:"books"`
}

func (s *server) apiStatus(w http.ResponseWriter, r *http.Request) {
	series, _, at := s.lib.snapshot()
	ip := detectLANIP()
	base := ""
	if ip != "" {
		base = "http://" + ip + ":" + itoa(s.port)
	}
	u := userFrom(r)
	s.scanMu.Lock()
	scanning := s.scanning
	last := s.lastScan
	var prog *scanProgressView
	if s.prog != nil {
		prog = &scanProgressView{
			Dirs:   s.prog.dirs.Load(),
			Series: s.prog.series.Load(),
			Books:  s.prog.books.Load(),
		}
	}
	s.scanMu.Unlock()
	resp := statusResp{
		Version:      appVersion,
		Ready:        s.lib.rootCount() > 0,
		Roots:        s.roots,
		LibraryName:  s.cfg.LibraryName,
		LANIP:        ip,
		Port:         s.port,
		BaseURL:      base,
		OPDSUrl:      base + "/opds/v1.2/catalog",
		Series:       len(series),
		ScannedAt:    at.Format("2006-01-02 15:04:05"),
		Scanning:     scanning,
		ScanProgress: prog,
		Problems:     len(s.lib.problemsSnapshot()),
		TLS:          s.tlsOn,
	}
	if last != nil {
		resp.LastScan = &scanView{
			At:         last.At.Format("2006-01-02 15:04:05"),
			DurationMS: last.DurationMS,
			Series:     last.Series,
			Books:      last.Books,
			Pages:      last.Pages,
			Bytes:      last.Bytes,
		}
	}
	for _, sr := range series {
		for _, b := range sr.Books {
			resp.Pages += b.Pages
			resp.Bytes += b.Size
		}
	}
	resp.Books = countBooks(series)
	if u != nil {
		resp.User = u.Name
		resp.IsAdmin = isAdmin(r)
	}
	writeJSON(w, resp)
}

func countBooks(series []*Series) int {
	n := 0
	for _, s := range series {
		n += len(s.Books)
	}
	return n
}

type userView struct {
	Name       string   `json:"name"`
	Restricted bool     `json:"restricted"`
	Dirs       []string `json:"dirs"`
}

func (s *server) apiUsers(w http.ResponseWriter, r *http.Request) {
	users := make([]userView, 0, len(s.users.Users))
	for _, u := range s.users.Users {
		dirs := u.Dirs
		if dirs == nil {
			dirs = []string{}
		}
		users = append(users, userView{Name: u.Name, Restricted: u.Restricted, Dirs: dirs})
	}
	writeJSON(w, map[string]any{"users": users})
}

type seriesView struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Rel   string `json:"rel"`
	Books int    `json:"books"`
}

func (s *server) apiSeriesList(w http.ResponseWriter, r *http.Request) {
	series, _, _ := s.lib.snapshot()
	out := make([]seriesView, 0, len(series))
	for _, sr := range series {
		out = append(out, seriesView{ID: sr.ID, Name: sr.Name, Rel: sr.Rel, Books: len(sr.Books)})
	}
	writeJSON(w, map[string]any{"series": out})
}

type permReq struct {
	Name       string   `json:"name"`
	Restricted bool     `json:"restricted"`
	Dirs       []string `json:"dirs"`
}

func (s *server) apiUserPerm(w http.ResponseWriter, r *http.Request) {
	var req permReq
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	u := s.users.find(req.Name)
	if u == nil {
		writeErr(w, http.StatusNotFound, "用户不存在")
		return
	}
	if !req.Restricted {
		u.Restricted = false
		u.Dirs = nil
	} else {
		u.Restricted = true
		u.Dirs = normalizePermDirs(req.Dirs, s.grantedDirs())
	}
	if err := s.users.save(s.configDir); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败")
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// grantedDirs returns every directory path that can be granted: each series
// path plus all of its ancestors. Renamed or removed directories drop out.
func (s *server) grantedDirs() map[string]bool {
	series, _, _ := s.lib.snapshot()
	valid := map[string]bool{}
	for _, sr := range series {
		if sr.Rel == "/" {
			continue
		}
		parts := strings.Split(sr.Rel, "/")
		for i := 1; i <= len(parts); i++ {
			valid[strings.Join(parts[:i], "/")] = true
		}
	}
	return valid
}

// normalizePermDirs keeps only existing paths, deduplicates, and collapses a
// child grant that is already covered by an ancestor grant.
func normalizePermDirs(dirs []string, valid map[string]bool) []string {
	seen := map[string]bool{}
	var legal []string
	for _, d := range dirs {
		if valid[d] && !seen[d] {
			seen[d] = true
			legal = append(legal, d)
		}
	}
	sort.Strings(legal)
	var kept []string
	var last string
	for _, d := range legal {
		if last != "" && strings.HasPrefix(d, last+"/") {
			continue
		}
		kept = append(kept, d)
		last = d
	}
	return kept
}

type userReq struct {
	Name     string `json:"name"`
	Password string `json:"password"`
	Admin    bool   `json:"admin"`
}

// validUsername keeps account names to a plain character set. They end up in
// inline onclick handlers on the admin page, so allowing quotes or angle
// brackets there would turn a crafted name into script injection.
func validUsername(name string) bool {
	if len(name) == 0 || len(name) > 32 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

func (s *server) apiAddUser(w http.ResponseWriter, r *http.Request) {
	var req userReq
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Password) < 6 {
		writeErr(w, http.StatusBadRequest, "用户名不能为空，密码至少 6 位")
		return
	}
	if !validUsername(req.Name) {
		writeErr(w, http.StatusBadRequest, "用户名只能包含字母、数字、._- ，最长 32 位")
		return
	}
	if s.users.find(req.Name) != nil {
		writeErr(w, http.StatusConflict, "用户已存在")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "无法创建用户")
		return
	}
	// User management belongs to the NAS admin. New accounts are always
	// standard readers; the admin flag is never accepted from the client.
	s.users.Users = append(s.users.Users, User{Name: req.Name, Hash: string(hash), Admin: false})
	if err := s.users.save(s.configDir); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败")
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *server) apiDeleteUser(w http.ResponseWriter, r *http.Request) {
	var req userReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.EqualFold(req.Name, userFrom(r).Name) {
		writeErr(w, http.StatusBadRequest, "不能删除当前登录用户")
		return
	}
	if !s.users.remove(req.Name) {
		writeErr(w, http.StatusNotFound, "用户不存在")
		return
	}
	_ = s.users.save(s.configDir)
	writeJSON(w, map[string]any{"ok": true})
}

func (s *server) apiSetPassword(w http.ResponseWriter, r *http.Request) {
	var req userReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Password) < 6 {
		writeErr(w, http.StatusBadRequest, "密码至少 6 位")
		return
	}
	u := s.users.find(req.Name)
	if u == nil {
		writeErr(w, http.StatusNotFound, "用户不存在")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "无法更新密码")
		return
	}
	u.Hash = string(hash)
	if err := s.users.save(s.configDir); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败")
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *server) apiRescan(w http.ResponseWriter, r *http.Request) {
	if s.lib.rootCount() == 0 {
		writeErr(w, http.StatusBadRequest, "尚未授权漫画目录")
		return
	}
	started := s.startScan()
	writeJSON(w, map[string]any{"ok": true, "started": started})
}

func (s *server) apiScanProblems(w http.ResponseWriter, r *http.Request) {
	probs := s.lib.problemsSnapshot()
	if probs == nil {
		probs = []scanProblem{}
	}
	writeJSON(w, map[string]any{"problems": probs})
}

// startScan rebuilds the library from every granted root and swaps it in when
// finished. Unchanged archives keep their cached page counts. It returns
// false when a scan is already running.
func (s *server) startScan() bool { return s.runScan(false) }

// autoScanLoop runs an incremental scan on a timer. It never stacks scans: a
// manual rescan in progress simply skips that tick.
func (s *server) autoScanLoop() {
	t := time.NewTicker(s.scanEvery)
	defer t.Stop()
	for range t.C {
		s.runScan(true)
	}
}

// runScan rebuilds the library in the background. With auto=true the new
// snapshot is only swapped in (and its cache written) when something actually
// changed, so a periodic scan over an untouched library is invisible to
// readers and does not rewrite library.json every time.
func (s *server) runScan(auto bool) bool {
	s.scanMu.Lock()
	if s.scanning || len(s.roots) == 0 {
		s.scanMu.Unlock()
		return false
	}
	s.scanning = true
	prev := s.lib.booksByPath()
	prog := &scanProgress{}
	s.prog = prog
	s.scanMu.Unlock()

	go func() {
		start := time.Now()
		lib := newLibrary(s.roots, s.cfg.LibraryName)
		lib.scan(prev, prog)
		dur := time.Since(start)

		if auto {
			s.scanMu.Lock()
			unchanged := s.lib.fingerprint() == lib.fingerprint()
			s.scanMu.Unlock()
			if unchanged {
				s.scanMu.Lock()
				s.scanning = false
				s.prog = nil
				s.scanMu.Unlock()
				log.Printf("automatic scan: no changes (%s)", dur.Round(time.Millisecond))
				s.maybeCleanCoverCache()
				return
			}
		}

		_ = osWriteLastScan(s.dataDir, dur)
		if err := lib.saveCache(libraryCachePath(s.dataDir)); err != nil {
			log.Printf("save library cache: %v", err)
		}
		if n := len(lib.problemsSnapshot()); n > 0 {
			log.Printf("scan found %d problem files (see admin page)", n)
		}

		series, _, _ := lib.snapshot()
		res := &scanResult{
			At:         time.Now(),
			DurationMS: dur.Milliseconds(),
			Series:     len(series),
			Books:      countBooks(series),
		}
		for _, sr := range series {
			for _, b := range sr.Books {
				res.Pages += b.Pages
				res.Bytes += b.Size
			}
		}

		s.scanMu.Lock()
		s.lib = lib
		s.scanning = false
		s.lastScan = res
		s.prog = nil
		s.scanMu.Unlock()
		s.maybeCleanCoverCache()
		if auto {
			log.Printf("automatic scan: updated library in %s (%d series, %d books)",
				dur.Round(time.Millisecond), res.Series, res.Books)
		}
	}()
	return true
}

func (s *server) apiLog(w http.ResponseWriter, r *http.Request) {
	n := 200
	if v, err := strconv.Atoi(r.URL.Query().Get("lines")); err == nil && v > 0 && v <= 1000 {
		n = v
	}
	lines := readLogTail(filepath.Join(s.dataDir, "app.log"), n)
	if lines == nil {
		lines = []string{}
	}
	writeJSON(w, map[string]any{"lines": lines})
}

// apiExport streams a single JSON bundle with the settings, accounts, scrape
// matches, admin-supplied covers and a recent log excerpt, so a reinstall can
// be reconfigured from one file.
func (s *server) apiExport(w http.ResponseWriter, r *http.Request) {
	series, _, _ := s.lib.snapshot()
	stats := map[string]any{
		"series": len(series),
		"books":  countBooks(series),
	}
	// Manual cover images travel inline: their files live outside the export
	// directory tree, and without the bytes a reinstall would fall back to the
	// built-in cover.
	overlay := s.meta.overlay()
	overlay.ManualCovers = nil
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition",
		`attachment; filename="mangagate-export-`+time.Now().Format("20060102-150405")+`.json"`)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{
		"version":       appVersion,
		"exported_at":   time.Now().Format(time.RFC3339),
		"config":        s.cfg,
		"users":         s.users.Users,
		"scraped":       overlay,
		"manual_covers": s.exportManualCovers(),
		"library":       stats,
		"log_tail":      readLogTail(filepath.Join(s.dataDir, "app.log"), 200),
	})
}

// exportManualCovers serializes the admin-supplied cover images as base64 so
// an import can recreate both the files and the overrides.
func (s *server) exportManualCovers() []exportManualCover {
	out := []exportManualCover{}
	for key, c := range s.meta.manualCovers() {
		level, id, ok := strings.Cut(key, ":")
		if !ok || c.File == "" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(coverDir(s.dataDir), filepath.Base(c.File)))
		if err != nil || len(raw) == 0 {
			continue
		}
		out = append(out, exportManualCover{
			Level: level,
			ID:    id,
			Data:  base64.StdEncoding.EncodeToString(raw),
			At:    c.At,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Level != out[j].Level {
			return out[i].Level < out[j].Level
		}
		return out[i].ID < out[j].ID
	})
	return out
}

type exportManualCover struct {
	Level string    `json:"level"`
	ID    string    `json:"id"`
	Data  string    `json:"data"` // base64 of the stored image
	At    time.Time `json:"at"`
}

type importPayload struct {
	Config       *Config             `json:"config"`
	Users        []User              `json:"users"`
	Scraped      *metaOverlay        `json:"scraped"`
	ManualCovers []exportManualCover `json:"manual_covers"`
}

// apiImport restores users (with permissions), scrape matches, manual covers
// and the library name from an export file. Ports and granted roots belong to
// the fnOS platform and are never imported.
func (s *server) apiImport(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Requested-With") != "mangagate" {
		writeErr(w, http.StatusForbidden, "bad request origin")
		return
	}
	// Manual cover images are embedded as base64 (up to 16 MB each), so the
	// bundle can exceed the old 8 MB limit by a fair margin.
	r.Body = http.MaxBytesReader(w, r.Body, 128<<20)
	var payload importPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeErr(w, http.StatusBadRequest, "导入文件格式不正确")
		return
	}
	if payload.Config == nil && len(payload.Users) == 0 && payload.Scraped == nil && len(payload.ManualCovers) == 0 {
		writeErr(w, http.StatusBadRequest, "导入文件里没有可恢复的数据")
		return
	}
	added, updated := 0, 0
	for _, iu := range payload.Users {
		name := strings.TrimSpace(iu.Name)
		if name == "" || iu.Hash == "" || !validUsername(name) {
			continue
		}
		if ex := s.users.find(name); ex != nil {
			ex.Hash = iu.Hash
			ex.Restricted = iu.Restricted
			ex.Dirs = normalizePermDirs(iu.Dirs, s.grantedDirs())
			updated++
		} else {
			s.users.Users = append(s.users.Users, User{
				Name: name, Hash: iu.Hash,
				Restricted: iu.Restricted,
				Dirs:       normalizePermDirs(iu.Dirs, s.grantedDirs()),
			})
			added++
		}
	}
	if added+updated > 0 {
		if err := s.users.save(s.configDir); err != nil {
			writeErr(w, http.StatusInternalServerError, "保存用户失败")
			return
		}
	}
	if payload.Config != nil && strings.TrimSpace(payload.Config.LibraryName) != "" {
		s.cfg.LibraryName = payload.Config.LibraryName
		if err := saveConfig(s.configDir, s.cfg); err != nil {
			log.Printf("import save config: %v", err)
		}
	}
	if payload.Scraped != nil {
		o := *payload.Scraped
		// Cover images are not part of the scrape overlay export; text
		// metadata survives, and the manual_covers array below carries the
		// admin-supplied images.
		o.ManualCovers = nil
		for _, info := range o.Series {
			info.Cover = ""
		}
		for _, info := range o.Books {
			info.Cover = ""
		}
		s.meta.replaceOverlay(o)
	}
	covers := 0
	for _, mc := range payload.ManualCovers {
		if mc.Level != "series" && mc.Level != "book" || !validScrapeID(mc.ID) {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(mc.Data)
		if err != nil || len(raw) == 0 || len(raw) > manualCoverMaxBytes {
			log.Printf("import manual cover %s:%s skipped: undecodable or too large", mc.Level, mc.ID)
			continue
		}
		if _, err := s.applyManualCoverBytes(mc.Level, mc.ID, raw); err != nil {
			log.Printf("import manual cover %s:%s failed: %v", mc.Level, mc.ID, err)
			continue
		}
		covers++
	}
	log.Printf("import finished: %d users added, %d updated, %d manual covers", added, updated, covers)
	writeJSON(w, map[string]any{
		"ok": true, "users_added": added, "users_updated": updated, "manual_covers": covers,
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Header.Get("X-Requested-With") != "mangagate" {
		writeErr(w, http.StatusForbidden, "bad request origin")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, "无效的请求内容")
		return false
	}
	return true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// --- scraping (Bangumi) ---

type scrapeSeriesRow struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Rel     string `json:"rel"`
	Books   int    `json:"books"`
	Term    string `json:"term"`
	Scraped bool   `json:"scraped"`
	Manual  bool   `json:"manual"`
	Title   string `json:"title,omitempty"`
	Authors string `json:"authors,omitempty"`
}

type scrapeBookRow struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	SeriesID   string `json:"series_id"`
	SeriesName string `json:"series_name"`
	Term       string `json:"term"`
	Embedded   bool   `json:"embedded"`
	Scraped    bool   `json:"scraped"`
	Manual     bool   `json:"manual"`
	MatchTitle string `json:"match_title,omitempty"`
}

type suggestView struct {
	SeriesID   string          `json:"series_id"`
	SeriesName string          `json:"series_name"`
	Candidate  scrapeCandidate `json:"candidate"`
}

// apiScrapeStatus reports, for the admin page, every series and book with
// its match state, a pre-cleaned search term, the batch progress and the
// pending suggestions.
func (s *server) apiScrapeStatus(w http.ResponseWriter, r *http.Request) {
	series, _, _ := s.lib.snapshot()
	rows := make([]scrapeSeriesRow, 0, len(series))
	books := make([]scrapeBookRow, 0)
	for _, sr := range series {
		row := scrapeSeriesRow{
			ID:    sr.ID,
			Name:  sr.Name,
			Rel:   sr.Rel,
			Books: len(sr.Books),
			Term:  cleanSearchTerm(sr.Name),
		}
		if info := s.meta.series(sr.ID); info != nil {
			row.Scraped = true
			row.Title = info.Title
			row.Authors = strings.Join(info.Authors, " / ")
		}
		row.Manual = s.meta.manual("series", sr.ID) != nil
		rows = append(rows, row)
		for _, b := range sr.Books {
			brow := scrapeBookRow{
				ID:         b.ID,
				Title:      b.Title,
				SeriesID:   sr.ID,
				SeriesName: sr.Name,
				Term:       cleanSearchTerm(b.Title),
				Embedded:   b.Meta != nil && (b.Meta.Title != "" || b.Meta.Summary != "" || len(b.Meta.Authors) > 0),
			}
			if info := s.meta.book(b.ID); info != nil {
				brow.Scraped = true
				brow.MatchTitle = info.Title
			}
			brow.Manual = s.meta.manual("book", b.ID) != nil
			books = append(books, brow)
		}
	}
	suggestions := []suggestView{}
	for id, cand := range s.meta.suggestions() {
		sr := s.lib.findSeries(id)
		if sr == nil {
			continue
		}
		suggestions = append(suggestions, suggestView{
			SeriesID: id, SeriesName: sr.Name, Candidate: *cand,
		})
	}
	s.autoMu.Lock()
	auto := map[string]any{
		"running": s.autoRunning,
		"done":    s.autoDone,
		"total":   s.autoTotal,
	}
	s.autoMu.Unlock()
	writeJSON(w, map[string]any{
		"series": rows, "books": books,
		"suggestions": suggestions, "auto": auto,
	})
}

type scrapeSearchReq struct {
	Term string `json:"term"`
}

func (s *server) apiScrapeSearch(w http.ResponseWriter, r *http.Request) {
	var req scrapeSearchReq
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Term = strings.TrimSpace(req.Term)
	if req.Term == "" {
		writeErr(w, http.StatusBadRequest, "请输入搜索词")
		return
	}
	cands, err := s.scrape.Search(req.Term)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "刮削源请求失败："+err.Error())
		return
	}
	writeJSON(w, map[string]any{"candidates": cands})
}

type scrapeApplyReq struct {
	Level     string `json:"level"`
	ID        string `json:"id"`
	SubjectID int    `json:"subject_id"`
}

// applySubject confirms one candidate: fetch details and credits, cache the
// cover locally, then store the info on the series or book. Shared by the
// interactive dialog and the batch run.
func (s *server) applySubject(level, id string, subjectID int) (string, error) {
	info, err := s.scrape.Subject(subjectID)
	if err != nil {
		return "", err
	}
	if authors := s.scrape.Persons(subjectID); len(authors) > 0 {
		info.Authors = authors
	}
	if info.Cover != "" {
		name := "scrape-" + level + "-" + id + ".jpg"
		if saved, err := s.scrape.DownloadCover(info.Cover, s.dataDir, name); err == nil {
			info.Cover = saved
		} else {
			log.Printf("scrape cover download failed: %v", err)
			info.Cover = ""
		}
	}
	if level == "series" {
		err = s.meta.setSeries(id, info)
		s.meta.clearSuggest(id)
	} else {
		err = s.meta.setBook(id, info)
	}
	if err != nil {
		return "", err
	}
	return info.Title, nil
}

// validScrapeID guards the series/book identifiers that get interpolated
// into cache file names: anything outside the hash character set is rejected.
func validScrapeID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func (s *server) apiScrapeApply(w http.ResponseWriter, r *http.Request) {
	var req scrapeApplyReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.SubjectID <= 0 || (req.Level != "series" && req.Level != "book") || !validScrapeID(req.ID) {
		writeErr(w, http.StatusBadRequest, "无效的匹配请求")
		return
	}
	title, err := s.applySubject(req.Level, req.ID, req.SubjectID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "拉取条目失败："+err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "title": title})
}

type scrapeRemoveReq struct {
	Level string `json:"level"`
	ID    string `json:"id"`
}

func (s *server) apiScrapeRemove(w http.ResponseWriter, r *http.Request) {
	var req scrapeRemoveReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if (req.Level != "series" && req.Level != "book") || !validScrapeID(req.ID) {
		writeErr(w, http.StatusBadRequest, "无效的请求")
		return
	}
	if err := s.meta.remove(req.Level, req.ID); err != nil {
		writeErr(w, http.StatusBadRequest, "无效的请求")
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *server) apiScrapeIgnore(w http.ResponseWriter, r *http.Request) {
	var req scrapeRemoveReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validScrapeID(req.ID) {
		writeErr(w, http.StatusBadRequest, "无效的请求")
		return
	}
	if err := s.meta.setIgnored(req.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败")
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// --- manual covers ---

// validCoverTarget reports whether level/id name something that exists in the
// library; only real entries can receive an override.
func (s *server) validCoverTarget(level, id string) bool {
	if level == "series" {
		return validScrapeID(id) && s.lib.findSeries(id) != nil
	}
	if level == "book" {
		return validScrapeID(id) && s.lib.findBook(id) != nil
	}
	return false
}

// applyManualCoverBytes stores decoded image bytes as the override for one
// series or book and removes a previous file when its name changed.
func (s *server) applyManualCoverBytes(level, id string, raw []byte) (string, error) {
	old := ""
	if c := s.meta.manual(level, id); c != nil {
		old = c.File
	}
	name, err := saveManualCover(s.dataDir, level, id, raw)
	if err != nil {
		return "", err
	}
	if err := s.meta.setManual(level, id, name); err != nil {
		return "", err
	}
	if old != "" && filepath.Base(old) != name {
		removeStoredCover(s.dataDir, old)
	}
	return name, nil
}

type manualCoverReq struct {
	Level string `json:"level"`
	ID    string `json:"id"`
	URL   string `json:"url"`
}

// apiCoverManual stores a cover fetched from an image URL.
func (s *server) apiCoverManual(w http.ResponseWriter, r *http.Request) {
	var req manualCoverReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.validCoverTarget(req.Level, req.ID) {
		writeErr(w, http.StatusBadRequest, "无效的请求")
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	u, err := url.Parse(req.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		writeErr(w, http.StatusBadRequest, "请填写 http/https 图片地址")
		return
	}
	hreq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, req.URL, nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "图片地址无效")
		return
	}
	hreq.Header.Set("User-Agent", "mangagate/"+appVersion)
	resp, err := newHTTPClient(s.scrapeProxy).Do(hreq)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "下载图片失败："+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		writeErr(w, http.StatusBadGateway, "下载图片失败：HTTP "+itoa(resp.StatusCode))
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, manualCoverMaxBytes+1))
	if err != nil || len(raw) > manualCoverMaxBytes {
		writeErr(w, http.StatusBadRequest, "图片过大（上限 16MB）")
		return
	}
	name, err := s.applyManualCoverBytes(req.Level, req.ID, raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "file": name})
}

// apiCoverUpload stores a cover uploaded from the admin page as multipart
// form data (fields: level, id, file).
func (s *server) apiCoverUpload(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Requested-With") != "mangagate" {
		writeErr(w, http.StatusForbidden, "bad request origin")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, manualCoverMaxBytes+(1<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "上传内容过大或格式不正确")
		return
	}
	level, id := r.FormValue("level"), r.FormValue("id")
	if !s.validCoverTarget(level, id) {
		writeErr(w, http.StatusBadRequest, "无效的请求")
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "请选择图片文件")
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, manualCoverMaxBytes+1))
	if err != nil || len(raw) > manualCoverMaxBytes {
		writeErr(w, http.StatusBadRequest, "图片过大（上限 16MB）")
		return
	}
	name, err := s.applyManualCoverBytes(level, id, raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "file": name})
}

// apiCoverManualClear drops the override and deletes its image.
func (s *server) apiCoverManualClear(w http.ResponseWriter, r *http.Request) {
	var req scrapeRemoveReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Level != "series" && req.Level != "book" || !validScrapeID(req.ID) {
		writeErr(w, http.StatusBadRequest, "无效的请求")
		return
	}
	file, err := s.meta.clearManual(req.Level, req.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败")
		return
	}
	files, bytes := removeStoredCover(s.dataDir, file)
	writeJSON(w, map[string]any{"ok": true, "files": files, "bytes": bytes})
}

// apiCoverPreview serves the thumbnail the feeds currently use for a series or
// book; the admin page can show it without going through OPDS Basic auth.
func (s *server) apiCoverPreview(w http.ResponseWriter, r *http.Request) {
	level, id := r.URL.Query().Get("level"), r.URL.Query().Get("id")
	var raw []byte
	switch level {
	case "series":
		sr := s.lib.findSeries(id)
		if sr == nil {
			http.NotFound(w, r)
			return
		}
		if c := s.meta.manual("series", id); c != nil && c.File != "" {
			raw = storedCoverThumbnail(coverDir(s.dataDir), c.File)
		}
		if len(raw) == 0 {
			if info := s.meta.series(id); info != nil && info.Cover != "" {
				raw = storedCoverThumbnail(coverDir(s.dataDir), info.Cover)
			}
		}
		if len(raw) == 0 {
			var b *Book
			if sr.Cover != "" {
				b = s.lib.findBook(sr.Cover)
			}
			if b == nil && len(sr.Books) > 0 {
				b = sr.Books[0]
			}
			if b != nil {
				if raw = s.storedThumbnail(b); len(raw) == 0 {
					raw = coverJPEG(b, s.dataDir)
				}
			}
		}
	case "book":
		b := s.lib.findBook(id)
		if b == nil {
			http.NotFound(w, r)
			return
		}
		if raw = s.storedThumbnail(b); len(raw) == 0 {
			raw = coverJPEG(b, s.dataDir)
		}
	default:
		writeErr(w, http.StatusBadRequest, "无效的请求")
		return
	}
	if len(raw) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	_, _ = w.Write(raw)
}

// --- cover cache ---

func (s *server) apiCacheStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, coverCacheUsage(s.dataDir))
}

type cacheClearReq struct {
	Level string `json:"level"`
	ID    string `json:"id"`
}

// apiCacheClear removes regenerable cover files. Stored scraped and manual
// cover images are data and stay; only derived files are deleted.
func (s *server) apiCacheClear(w http.ResponseWriter, r *http.Request) {
	var req cacheClearReq
	if !decodeJSON(w, r, &req) {
		return
	}
	var files int
	var bytes int64
	if req.Level == "all" {
		var err error
		files, bytes, err = s.clearAllCoverCache()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "清理失败")
			return
		}
	} else {
		targets, err := s.clearCacheFiles(req.Level, req.ID)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		files, bytes = deleteCacheFiles(s.dataDir, targets)
	}
	log.Printf("cover cache cleared (%s %s): %d files, %s", req.Level, req.ID, files, humanSize(bytes))
	writeJSON(w, map[string]any{"ok": true, "files": files, "bytes": bytes})
}

type autoReq struct {
	Action string `json:"action"` // start | stop
}

// apiScrapeAuto starts or stops the background batch: every unmatched,
// not-yet-ignored series is searched once; a clearly matching top candidate
// is applied, an uncertain one is stored as a suggestion.
func (s *server) apiScrapeAuto(w http.ResponseWriter, r *http.Request) {
	var req autoReq
	if !decodeJSON(w, r, &req) {
		return
	}
	switch req.Action {
	case "stop":
		s.autoMu.Lock()
		s.autoStop = true
		s.autoMu.Unlock()
		writeJSON(w, map[string]any{"ok": true})
	case "start":
		s.autoMu.Lock()
		if s.autoRunning {
			s.autoMu.Unlock()
			writeErr(w, http.StatusConflict, "批量匹配已在运行")
			return
		}
		s.autoRunning = true
		s.autoDone = 0
		s.autoStop = false
		s.autoMu.Unlock()
		go s.runAutoScrape()
		writeJSON(w, map[string]any{"ok": true})
	default:
		writeErr(w, http.StatusBadRequest, "action 必须是 start 或 stop")
	}
}

func (s *server) runAutoScrape() {
	defer func() {
		s.autoMu.Lock()
		s.autoRunning = false
		s.autoMu.Unlock()
	}()
	type target struct{ id, term, name string }
	var list []target
	series, _, _ := s.lib.snapshot()
	for _, sr := range series {
		if s.meta.series(sr.ID) != nil || s.meta.isIgnored(sr.ID) {
			continue
		}
		list = append(list, target{sr.ID, cleanSearchTerm(sr.Name), sr.Name})
	}
	s.autoMu.Lock()
	s.autoTotal = len(list)
	s.autoMu.Unlock()
	for _, t := range list {
		s.autoMu.Lock()
		stop := s.autoStop
		s.autoMu.Unlock()
		if stop {
			break
		}
		if t.term != "" {
			if cands, err := s.scrape.Search(t.term); err == nil && len(cands) > 0 {
				if scrapeConfident(t.term, cands[0]) {
					if _, err := s.applySubject("series", t.id, cands[0].ID); err != nil {
						log.Printf("auto scrape apply %q: %v", t.name, err)
					}
				} else {
					_ = s.meta.setSuggest(t.id, &cands[0])
				}
			}
		}
		s.autoMu.Lock()
		s.autoDone++
		s.autoMu.Unlock()
		// Stay well under the upstream rate limits.
		time.Sleep(700 * time.Millisecond)
	}
}

// unameArch maps the kernel architecture to our embedded binary suffix.
func unameArch() string {
	out, err := exec.Command("uname", "-m").Output()
	if err != nil {
		return "arm64"
	}
	switch strings.TrimSpace(string(out)) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	}
	return "arm64"
}

func coverPath(dataDir, id string) string { return filepath.Join(coverDir(dataDir), id+".jpg") }
