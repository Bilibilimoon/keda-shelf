package main

import (
	"archive/zip"
	"encoding/xml"
	"html"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// bookMeta is the embedded metadata of one archive book: EPUB package
// document (content.opf) or a cbz ComicInfo.xml. It is cached with the scan
// result; an empty non-nil value means "extracted, nothing there".
type bookMeta struct {
	Title    string   `json:"title,omitempty"`
	Authors  []string `json:"authors,omitempty"`
	Summary  string   `json:"summary,omitempty"`
	Subjects []string `json:"subjects,omitempty"`
	// CoverEntry is the archive-internal path of the declared cover image.
	CoverEntry string `json:"cover_entry,omitempty"`
}

// archiveMetadata pulls embedded metadata for the format; it never returns
// nil so the "already extracted" state survives scan caching.
func archiveMetadata(file, format string) *bookMeta {
	var m *bookMeta
	switch format {
	case "epub":
		m = epubMetadata(file)
	case "cbz", "zip", "cb7":
		m = comicInfoMetadata(file)
	}
	if m == nil {
		m = &bookMeta{}
	}
	return m
}

// --- EPUB ---

type opfMetaEl struct {
	Name    string `xml:"name,attr"`
	Content string `xml:"content,attr"`
}

type opfMetadata struct {
	Titles      []string    `xml:"title"`
	Creators    []string    `xml:"creator"`
	Description string      `xml:"description"`
	Subjects    []string    `xml:"subject"`
	Metas       []opfMetaEl `xml:"meta"`
}

type opfItem struct {
	ID         string `xml:"id,attr"`
	Href       string `xml:"href,attr"`
	MediaType  string `xml:"media-type,attr"`
	Properties string `xml:"properties,attr"`
}

type opfPackage struct {
	Metadata opfMetadata `xml:"metadata"`
	Manifest struct {
		Items []opfItem `xml:"item"`
	} `xml:"manifest"`
}

func epubMetadata(file string) *bookMeta {
	zr, err := zip.OpenReader(file)
	if err != nil {
		return nil
	}
	defer zr.Close()
	entries := map[string]*zip.File{}
	for _, f := range zr.File {
		entries[f.Name] = f
	}

	opfPath := epubOPFPath(entries)
	if opfPath == "" {
		return nil
	}
	raw, err := readZipEntry(entries, opfPath)
	if err != nil {
		return nil
	}
	var pkg opfPackage
	if err := xml.Unmarshal(raw, &pkg); err != nil {
		return nil
	}

	m := &bookMeta{}
	if len(pkg.Metadata.Titles) > 0 {
		m.Title = strings.TrimSpace(pkg.Metadata.Titles[0])
	}
	for _, c := range pkg.Metadata.Creators {
		if c = strings.TrimSpace(c); c != "" {
			m.Authors = append(m.Authors, c)
		}
	}
	m.Summary = plainText(pkg.Metadata.Description)
	for _, s := range pkg.Metadata.Subjects {
		if s = strings.TrimSpace(s); s != "" {
			m.Subjects = append(m.Subjects, s)
		}
	}
	if cover := opfCoverPath(&pkg, path.Dir(opfPath), entries); cover != "" {
		m.CoverEntry = cover
	}
	if m.Title == "" && len(m.Authors) == 0 && m.Summary == "" && m.CoverEntry == "" {
		return nil
	}
	return m
}

// epubOPFPath locates the package document via META-INF/container.xml and
// falls back to any .opf in the archive.
func epubOPFPath(entries map[string]*zip.File) string {
	if raw, err := readZipEntry(entries, "META-INF/container.xml"); err == nil {
		var c struct {
			Rootfiles []struct {
				FullPath string `xml:"full-path,attr"`
			} `xml:"rootfiles>rootfile"`
		}
		if xml.Unmarshal(raw, &c) == nil && len(c.Rootfiles) > 0 && c.Rootfiles[0].FullPath != "" {
			if entries[c.Rootfiles[0].FullPath] != nil {
				return c.Rootfiles[0].FullPath
			}
		}
	}
	for name := range entries {
		if strings.EqualFold(path.Ext(name), ".opf") {
			return name
		}
	}
	return ""
}

// opfCoverPath resolves the cover image: EPUB 3 cover-image property, the
// EPUB 2 meta cover id, or an image whose file name mentions "cover".
func opfCoverPath(pkg *opfPackage, opfDir string, entries map[string]*zip.File) string {
	tryHref := func(href string) string {
		if href == "" {
			return ""
		}
		if dec, err := url.PathUnescape(href); err == nil {
			href = dec
		}
		full := href
		if opfDir != "" && opfDir != "." {
			full = path.Join(opfDir, href)
		}
		// Some producers URL-encode entry names; accept either form.
		if entries[full] != nil {
			return full
		}
		if entries[href] != nil {
			return href
		}
		return ""
	}
	coverID := ""
	for _, meta := range pkg.Metadata.Metas {
		if strings.EqualFold(meta.Name, "cover") {
			coverID = meta.Content
			break
		}
	}
	for _, it := range pkg.Manifest.Items {
		if strings.Contains(it.Properties, "cover-image") {
			if p := tryHref(it.Href); p != "" {
				return p
			}
		}
		if coverID != "" && it.ID == coverID {
			if p := tryHref(it.Href); p != "" {
				return p
			}
		}
	}
	for _, it := range pkg.Manifest.Items {
		if strings.HasPrefix(it.MediaType, "image/") &&
			strings.Contains(strings.ToLower(it.Href), "cover") {
			if p := tryHref(it.Href); p != "" {
				return p
			}
		}
	}
	return ""
}

// --- ComicInfo.xml (cbz) ---

func comicInfoMetadata(file string) *bookMeta {
	zr, err := zip.OpenReader(file)
	if err != nil {
		return nil
	}
	defer zr.Close()
	entries := map[string]*zip.File{}
	for _, f := range zr.File {
		entries[f.Name] = f
	}
	var raw []byte
	for _, name := range []string{"ComicInfo.xml", "comicinfo.xml"} {
		if b, err := readZipEntry(entries, name); err == nil {
			raw = b
			break
		}
	}
	if raw == nil {
		return nil
	}
	var ci struct {
		Series  string `xml:"Series"`
		Title   string `xml:"Title"`
		Writer  string `xml:"Writer"`
		Summary string `xml:"Summary"`
		Genre   string `xml:"Genre"`
		Tags    string `xml:"Tags"`
	}
	if err := xml.Unmarshal(raw, &ci); err != nil {
		return nil
	}
	m := &bookMeta{
		Title:   strings.TrimSpace(ci.Title),
		Summary: plainText(ci.Summary),
	}
	if m.Title == "" {
		m.Title = strings.TrimSpace(ci.Series)
	}
	for _, w := range splitList(ci.Writer) {
		m.Authors = append(m.Authors, w)
	}
	m.Subjects = append(m.Subjects, splitList(ci.Genre)...)
	m.Subjects = append(m.Subjects, splitList(ci.Tags)...)
	if m.Title == "" && m.Summary == "" && len(m.Authors) == 0 && len(m.Subjects) == 0 {
		return nil
	}
	return m
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == '、' || r == '，' || r == ';'
	}) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

var (
	htmlTagRe    = regexp.MustCompile(`(?s)<[^>]*>`)
	blockTagRe   = regexp.MustCompile(`(?i)</?(p|div|br|li|ul|ol|h[1-6]|table|tr|td|blockquote)[^>]*>`)
	whitespaceRe = regexp.MustCompile(`\s+`)
)

// plainText strips the lightweight markup publishers put into descriptions.
// Block tags separate words; inline tags vanish so CJK text is not split.
func plainText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || !strings.ContainsAny(s, "<&") {
		return s
	}
	s = blockTagRe.ReplaceAllString(s, "\n")
	s = htmlTagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return strings.TrimSpace(whitespaceRe.ReplaceAllString(s, " "))
}

func readZipEntry(entries map[string]*zip.File, name string) ([]byte, error) {
	f := entries[name]
	if f == nil {
		return nil, io.ErrUnexpectedEOF
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, 4<<20))
}
