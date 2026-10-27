package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	defaultBangumiBase = "https://api.bgm.tv"
	defaultAniListBase = "https://graphql.anilist.co"
)

// scraper is one metadata source. Bangumi has the Chinese metadata but is
// unreachable from many home networks; AniList works everywhere but carries
// Japanese/English titles and English summaries.
type scraper interface {
	Search(keyword string) ([]scrapeCandidate, error)
	Subject(id int) (*scrapedInfo, error)
	Persons(id int) []string
	DownloadCover(url, dataDir, name string) (string, error)
}

func newHTTPClient(proxyURL string) *http.Client {
	if u, err := url.Parse(strings.TrimSpace(proxyURL)); err == nil && u.Scheme != "" && u.Host != "" {
		return &http.Client{
			Timeout:   12 * time.Second,
			Transport: &http.Transport{Proxy: http.ProxyURL(u)},
		}
	}
	return &http.Client{Timeout: 12 * time.Second}
}

func newScraper(source, base, proxy string) scraper {
	hc := newHTTPClient(proxy)
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "bangumi":
		if strings.TrimSpace(base) == "" {
			base = defaultBangumiBase
		}
		return &bangumiScraper{base: strings.TrimSuffix(base, "/"), hc: hc}
	default:
		return &anilistScraper{hc: hc}
	}
}

// --- Bangumi ---

type bangumiScraper struct {
	base string
	hc   *http.Client
}

// scrapedInfo is a confirmed match from the online source, stored in a
// sidecar file so re-scans and restarts keep it; the library files are never
// touched.
type scrapedInfo struct {
	Source    string    `json:"source"`
	SubjectID int       `json:"subject_id"`
	Title     string    `json:"title"`      // official name, Chinese preferred
	TitleOrig string    `json:"title_orig"` // original name
	Authors   []string  `json:"authors,omitempty"`
	Summary   string    `json:"summary,omitempty"`
	Tags      []string  `json:"tags,omitempty"`
	Date      string    `json:"date,omitempty"`
	Cover     string    `json:"cover,omitempty"` // file name under covers/
	At        time.Time `json:"at"`
}

type metaOverlay struct {
	Series map[string]*scrapedInfo `json:"series,omitempty"`
	Books  map[string]*scrapedInfo `json:"books,omitempty"`
	// Suggested holds the best unconfirmed candidate per series from the
	// batch run; Ignored marks series the admin dismissed.
	Suggested map[string]*scrapeCandidate `json:"suggested,omitempty"`
	Ignored   map[string]bool             `json:"ignored,omitempty"`
}

type metaStore struct {
	mu      sync.RWMutex
	file    string
	dataDir string
	o       metaOverlay
}

func loadMetaStore(file, dataDir string) *metaStore {
	m := &metaStore{file: file, dataDir: dataDir}
	if raw, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal(raw, &m.o)
	}
	return m
}

func (m *metaStore) series(id string) *scrapedInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.o.Series[id]
}

func (m *metaStore) book(id string) *scrapedInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.o.Books[id]
}

func (m *metaStore) setSeries(id string, info *scrapedInfo) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.o.Series == nil {
		m.o.Series = map[string]*scrapedInfo{}
	}
	m.o.Series[id] = info
	return m.persist()
}

func (m *metaStore) setBook(id string, info *scrapedInfo) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.o.Books == nil {
		m.o.Books = map[string]*scrapedInfo{}
	}
	m.o.Books[id] = info
	return m.persist()
}

func (m *metaStore) remove(level, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch level {
	case "series":
		if old := m.o.Series[id]; old != nil {
			_ = os.Remove(filepath.Join(coverDir(m.dataDir), filepath.Base(old.Cover)))
		}
		delete(m.o.Series, id)
	case "book":
		if old := m.o.Books[id]; old != nil {
			_ = os.Remove(filepath.Join(coverDir(m.dataDir), filepath.Base(old.Cover)))
		}
		delete(m.o.Books, id)
	default:
		return errors.New("unknown level")
	}
	return m.persist()
}

func (m *metaStore) persist() error {
	raw, err := json.MarshalIndent(&m.o, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.file + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, m.file)
}

func (m *metaStore) setSuggest(seriesID string, c *scrapeCandidate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.o.Suggested == nil {
		m.o.Suggested = map[string]*scrapeCandidate{}
	}
	m.o.Suggested[seriesID] = c
	return m.persist()
}

func (m *metaStore) clearSuggest(seriesID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.o.Suggested, seriesID)
	_ = m.persist()
}

func (m *metaStore) setIgnored(seriesID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.o.Ignored == nil {
		m.o.Ignored = map[string]bool{}
	}
	m.o.Ignored[seriesID] = true
	delete(m.o.Suggested, seriesID)
	return m.persist()
}

func (m *metaStore) isIgnored(seriesID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.o.Ignored[seriesID]
}

// suggestions lists pending candidates for the admin page.
func (m *metaStore) suggestions() map[string]*scrapeCandidate {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]*scrapeCandidate, len(m.o.Suggested))
	for k, v := range m.o.Suggested {
		out[k] = v
	}
	return out
}

// overlay returns the raw overlay for export/import.
func (m *metaStore) overlay() metaOverlay {
	m.mu.RLock()
	defer m.mu.RUnlock()
	o := m.o
	return o
}

// replaceOverlay swaps in imported scrape data (covers are not part of the
// export and are left empty).
func (m *metaStore) replaceOverlay(o metaOverlay) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.o.Series = o.Series
	m.o.Books = o.Books
	if m.o.Series == nil {
		m.o.Series = map[string]*scrapedInfo{}
	}
	if m.o.Books == nil {
		m.o.Books = map[string]*scrapedInfo{}
	}
	_ = m.persist()
}

// --- confidence ---

// normTitle keeps CJK runes, letters and digits, lower-cased and squeezed, so
// "進撃の巨人" and translated variants can be compared character-wise.
func normTitle(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 0x4e && r <= 0x9fff, // CJK unified
			r >= 0x3040 && r <= 0x30ff, // kana
			r >= 'a' && r <= 'z',
			r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// runeOverlap is the Jaccard similarity of the two rune sets.
func runeOverlap(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	sa := map[rune]bool{}
	for _, r := range a {
		sa[r] = true
	}
	inter, union := 0, len(sa)
	for _, r := range b {
		if sa[r] {
			inter++
		} else {
			union++
		}
	}
	return float64(inter) / float64(union)
}

// scrapeConfident decides whether the top candidate matches the local name
// closely enough to apply without a human. The bar is deliberately high:
// equal after normalisation, or an almost identical character set with a
// near-equal length (romanisation variants).
func scrapeConfident(term string, c scrapeCandidate) bool {
	a := normTitle(term)
	if a == "" {
		return false
	}
	for _, cand := range []string{c.Title, c.Orig} {
		b := normTitle(cand)
		if b == "" {
			continue
		}
		if a == b {
			return true
		}
		if abs(len(a)-len(b)) <= 2 && runeOverlap(a, b) >= 0.9 {
			return true
		}
	}
	return false
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// --- Bangumi client ---

type scrapeCandidate struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
	Orig  string `json:"title_orig"`
	Date  string `json:"date"`
	Image string `json:"image"`
}

func (sc *bangumiScraper) do(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = strings.NewReader(string(raw))
	}
	req, err := http.NewRequest(method, sc.base+path, rd)
	if err != nil {
		return err
	}
	// Bangumi rejects default library user agents.
	req.Header.Set("User-Agent", "mangagate/"+appVersion+" (fnOS)")
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := sc.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream %s: %s", resp.Status, strings.TrimSpace(readSnippet(resp.Body)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

func readSnippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 200))
	return string(b)
}

func (sc *bangumiScraper) Search(keyword string) ([]scrapeCandidate, error) {
	var resp struct {
		Data []struct {
			ID     int    `json:"id"`
			Name   string `json:"name"`
			NameCN string `json:"name_cn"`
			Date   string `json:"date"`
			Images struct {
				Large  string `json:"large"`
				Common string `json:"common"`
				Medium string `json:"medium"`
			} `json:"images"`
		} `json:"data"`
	}
	body := map[string]any{
		"keyword": keyword,
		"filter":  map[string]any{"type": []int{1}},
	}
	if err := sc.do("POST", "/v0/search/subjects?limit=10", body, &resp); err != nil {
		return nil, err
	}
	out := make([]scrapeCandidate, 0, len(resp.Data))
	for _, d := range resp.Data {
		title := d.NameCN
		if title == "" {
			title = d.Name
		}
		img := d.Images.Large
		if img == "" {
			img = d.Images.Common
		}
		if img == "" {
			img = d.Images.Medium
		}
		out = append(out, scrapeCandidate{
			ID: d.ID, Title: title, Orig: d.Name, Date: d.Date, Image: img,
		})
	}
	return out, nil
}

func (sc *bangumiScraper) Subject(id int) (*scrapedInfo, error) {
	var d struct {
		Name    string `json:"name"`
		NameCN  string `json:"name_cn"`
		Summary string `json:"summary"`
		Date    string `json:"date"`
		Images  struct {
			Large  string `json:"large"`
			Common string `json:"common"`
		} `json:"images"`
		Tags []struct {
			Name string `json:"name"`
		} `json:"tags"`
	}
	if err := sc.do("GET", fmt.Sprintf("/v0/subjects/%d", id), nil, &d); err != nil {
		return nil, err
	}
	info := &scrapedInfo{
		Source:    "bangumi",
		SubjectID: id,
		Title:     d.NameCN,
		TitleOrig: d.Name,
		Summary:   plainText(d.Summary),
		Date:      d.Date,
		At:        time.Now(),
	}
	if info.Title == "" {
		info.Title = d.Name
	}
	for _, t := range d.Tags {
		if len(info.Tags) < 12 && t.Name != "" {
			info.Tags = append(info.Tags, t.Name)
		}
	}
	if d.Images.Large != "" {
		info.Cover = d.Images.Large
	} else {
		info.Cover = d.Images.Common
	}
	return info, nil
}

// Persons fetches the writer credits for a subject.
func (sc *bangumiScraper) Persons(id int) []string {
	var ps []struct {
		Name     string `json:"name"`
		Relation string `json:"relation"`
	}
	if err := sc.do("GET", fmt.Sprintf("/v0/subjects/%d/persons", id), nil, &ps); err != nil {
		return nil
	}
	var exact, other []string
	for _, p := range ps {
		switch p.Relation {
		case "作者", "原作", "作画", "插图":
			if p.Name != "" {
				exact = append(exact, p.Name)
			}
		}
	}
	if len(exact) == 0 {
		for _, p := range ps {
			if strings.Contains(p.Relation, "作") && p.Name != "" {
				other = append(other, p.Name)
			}
		}
	}
	out := exact
	if len(out) == 0 {
		out = other
	}
	if len(out) > 4 {
		out = out[:4]
	}
	return out
}

// DownloadCover stores the cover image next to the generated covers and
// returns its file name; it leaves info untouched on failure.
func (sc *bangumiScraper) DownloadCover(url, dataDir, name string) (string, error) {
	return downloadCover(sc.hc, url, dataDir, name)
}

// --- AniList (GraphQL) ---

type anilistScraper struct {
	hc *http.Client
}

func (a *anilistScraper) gql(query string, vars map[string]any, out any) error {
	raw, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", defaultAniListBase, strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "mangagate/"+appVersion+" (fnOS)")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := a.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream %s: %s", resp.Status, strings.TrimSpace(readSnippet(resp.Body)))
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&envelope); err != nil {
		return err
	}
	if len(envelope.Errors) > 0 {
		return errors.New(envelope.Errors[0].Message)
	}
	if len(envelope.Data) == 0 {
		return errors.New("empty response")
	}
	return json.Unmarshal(envelope.Data, out)
}

type aliTitle struct {
	Native  string `json:"native"`
	Romaji  string `json:"romaji"`
	English string `json:"english"`
}

func (t aliTitle) best() string {
	for _, s := range []string{t.Native, t.English, t.Romaji} {
		if s != "" {
			return s
		}
	}
	return ""
}

func (a *anilistScraper) Search(keyword string) ([]scrapeCandidate, error) {
	const q = `query($s:String){Page(perPage:10){media(type:MANGA,search:$s,isAdult:false){id title{native romaji english} startDate{year} coverImage{extraLarge large}}}}`
	var resp struct {
		Page struct {
			Media []struct {
				ID        int      `json:"id"`
				Title     aliTitle `json:"title"`
				StartDate struct {
					Year int `json:"year"`
				} `json:"startDate"`
				CoverImage struct {
					ExtraLarge string `json:"extraLarge"`
					Large      string `json:"large"`
				} `json:"coverImage"`
			} `json:"media"`
		} `json:"Page"`
	}
	if err := a.gql(q, map[string]any{"s": keyword}, &resp); err != nil {
		return nil, err
	}
	out := make([]scrapeCandidate, 0, len(resp.Page.Media))
	for _, m := range resp.Page.Media {
		img := m.CoverImage.ExtraLarge
		if img == "" {
			img = m.CoverImage.Large
		}
		date := ""
		if m.StartDate.Year > 0 {
			date = fmt.Sprintf("%d", m.StartDate.Year)
		}
		out = append(out, scrapeCandidate{
			ID:    m.ID,
			Title: m.Title.best(),
			Orig:  m.Title.Romaji,
			Date:  date,
			Image: img,
		})
	}
	return out, nil
}

func (a *anilistScraper) Subject(id int) (*scrapedInfo, error) {
	const q = `query($id:Int){Media(id:$id){id title{native romaji english} description startDate{year} coverImage{extraLarge large} genres tags{name} staff(sort:RELEVANCE,perPage:8){edges{role node{name{full}}}}}}`
	var resp struct {
		Media struct {
			ID          int      `json:"id"`
			Title       aliTitle `json:"title"`
			Description string   `json:"description"`
			StartDate   struct {
				Year int `json:"year"`
			} `json:"startDate"`
			CoverImage struct {
				ExtraLarge string `json:"extraLarge"`
				Large      string `json:"large"`
			} `json:"coverImage"`
			Genres []string `json:"genres"`
			Tags   []struct {
				Name string `json:"name"`
			} `json:"tags"`
			Staff struct {
				Edges []struct {
					Role string `json:"role"`
					Node struct {
						Name struct {
							Full string `json:"full"`
						} `json:"name"`
					} `json:"node"`
				} `json:"edges"`
			} `json:"staff"`
		} `json:"Media"`
	}
	if err := a.gql(q, map[string]any{"id": id}, &resp); err != nil {
		return nil, err
	}
	m := resp.Media
	info := &scrapedInfo{
		Source:    "anilist",
		SubjectID: m.ID,
		Title:     m.Title.best(),
		TitleOrig: m.Title.Romaji,
		Summary:   plainText(m.Description),
		At:        time.Now(),
	}
	if m.StartDate.Year > 0 {
		info.Date = fmt.Sprintf("%d", m.StartDate.Year)
	}
	for _, g := range m.Genres {
		if len(info.Tags) < 12 {
			info.Tags = append(info.Tags, g)
		}
	}
	for _, t := range m.Tags {
		if len(info.Tags) < 12 {
			info.Tags = append(info.Tags, t.Name)
		}
	}
	seen := map[string]bool{}
	for _, e := range m.Staff.Edges {
		if len(info.Authors) >= 4 {
			break
		}
		if strings.Contains(e.Role, "Story") || strings.Contains(e.Role, "Art") {
			n := e.Node.Name.Full
			if n != "" && !seen[n] {
				seen[n] = true
				info.Authors = append(info.Authors, n)
			}
		}
	}
	info.Cover = m.CoverImage.ExtraLarge
	if info.Cover == "" {
		info.Cover = m.CoverImage.Large
	}
	return info, nil
}

// AniList delivers the staff with the subject itself.
func (a *anilistScraper) Persons(id int) []string { return nil }

func (a *anilistScraper) DownloadCover(url, dataDir, name string) (string, error) {
	return downloadCover(a.hc, url, dataDir, name)
}

// downloadCover is shared by both sources.
func downloadCover(hc *http.Client, rawURL, dataDir, name string) (string, error) {
	if rawURL == "" {
		return "", errors.New("no cover url")
	}
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "mangagate/"+appVersion+" (fnOS)")
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cover download %s", resp.Status)
	}
	if err := os.MkdirAll(coverDir(dataDir), 0o755); err != nil {
		return "", err
	}
	file := filepath.Join(coverDir(dataDir), name)
	tmp := file + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, io.LimitReader(resp.Body, 16<<20)); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return name, os.Rename(tmp, file)
}

// --- search-term cleanup ---

var (
	bracketRe   = regexp.MustCompile(`[\[【（(]([^\]】）)]*)[\]】）)]`)
	volumeRe    = regexp.MustCompile(`(?i)\s*(第\s*\d+\s*[卷册话話回部]|\d+\s*[卷册话話回部]\s*$|vol\.?\s*\d+\s*$|v\d+\s*$|book\s*\d+\s*$)`)
	extRe       = regexp.MustCompile(`(?i)\.(epub|pdf|cbz|zip|cbr|rar|cb7)$`)
	sepNoiseRe  = regexp.MustCompile(`\s*[-_.·]\s*$`)
	spaceFoldRe = regexp.MustCompile(`\s+`)
)

// cleanSearchTerm strips release-group brackets, volume/episode suffixes and
// file extensions so directory and file names become searchable titles.
func cleanSearchTerm(name string) string {
	s := strings.TrimSpace(name)
	s = extRe.ReplaceAllString(s, "")
	s = bracketRe.ReplaceAllString(s, " ")
	for i := 0; i < 3; i++ {
		next := volumeRe.ReplaceAllString(s, "")
		next = strings.TrimSpace(sepNoiseRe.ReplaceAllString(next, ""))
		if next == s {
			break
		}
		s = next
	}
	s = strings.TrimSpace(s)
	s = spaceFoldRe.ReplaceAllString(s, " ")
	return s
}
