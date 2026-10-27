package main

import "strings"

// searchTerms splits a query into lower-cased words. Every word has to occur
// in the searched text (AND), which makes multi-word queries such as
// "进击的巨人 第3卷" narrow down instead of widening the result.
func searchTerms(q string) []string {
	var out []string
	for _, f := range strings.Fields(strings.ToLower(q)) {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// bookSearchText is the searchable text of a book: its title, the series it
// belongs to, its relative path (so a folder name finds its contents) and any
// embedded archive metadata.
func bookSearchText(sr *Series, b *Book) string {
	var sb strings.Builder
	appendSearchField(&sb, b.Title)
	if sr != nil {
		appendSearchField(&sb, sr.Name)
	}
	appendSearchField(&sb, b.Rel)
	if b.Meta != nil {
		appendSearchField(&sb, b.Meta.Title)
		appendSearchField(&sb, b.Meta.Summary)
		for _, a := range b.Meta.Authors {
			appendSearchField(&sb, a)
		}
		for _, s := range b.Meta.Subjects {
			appendSearchField(&sb, s)
		}
	}
	return sb.String()
}

// bookSearchWithScrape adds the owner-confirmed online metadata, which often
// carries the official title, author and tags users actually search for.
func (s *server) bookSearchWithScrape(sr *Series, b *Book) string {
	var sb strings.Builder
	sb.WriteString(bookSearchText(sr, b))
	if sr != nil {
		if info := s.meta.series(sr.ID); info != nil {
			appendScrapedInfo(&sb, info)
		}
	}
	if info := s.meta.book(b.ID); info != nil {
		appendScrapedInfo(&sb, info)
	}
	return sb.String()
}

func appendScrapedInfo(sb *strings.Builder, info *scrapedInfo) {
	appendSearchField(sb, info.Title)
	appendSearchField(sb, info.TitleOrig)
	appendSearchField(sb, info.Summary)
	for _, a := range info.Authors {
		appendSearchField(sb, a)
	}
	for _, t := range info.Tags {
		appendSearchField(sb, t)
	}
}

func appendSearchField(sb *strings.Builder, s string) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return
	}
	sb.WriteString(s)
	sb.WriteByte('\n')
}

// matchesAllTerms reports whether every term occurs in text. An empty term
// list never matches, so a blank query cannot dump the whole library.
func matchesAllTerms(text string, terms []string) bool {
	if len(terms) == 0 {
		return false
	}
	for _, t := range terms {
		if !strings.Contains(text, t) {
			return false
		}
	}
	return true
}
