package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPageEntries(t *testing.T) {
	entries := make([]opdsEntry, 250)
	for i := range entries {
		entries[i].Title = "e"
	}

	f := &opdsFeed{}
	r := httptest.NewRequest("GET", "/opds/v1.2/series?page=2", nil)
	page := pageEntries(r, f, entries, 100)
	if len(page) != 100 {
		t.Fatalf("middle page size = %d, want 100", len(page))
	}
	rels := map[string]bool{}
	for _, l := range f.Links {
		rels[l.Rel] = true
		if l.Href == "" || !strings.Contains(l.Href, "page=") {
			t.Errorf("pager link missing href: %+v", l)
		}
	}
	if !rels["next"] || !rels["previous"] {
		t.Fatalf("middle page needs next and previous, got %v", rels)
	}

	f2 := &opdsFeed{}
	r2 := httptest.NewRequest("GET", "/opds/v1.2/series?page=3", nil)
	pageEntries(r2, f2, entries, 100)
	for _, l := range f2.Links {
		if l.Rel == "next" {
			t.Fatal("last page must not have a next link")
		}
	}

	f3 := &opdsFeed{}
	r3 := httptest.NewRequest("GET", "/opds/v1.2/series", nil)
	page3 := pageEntries(r3, f3, []opdsEntry{{Title: "only"}}, 100)
	if len(page3) != 1 {
		t.Fatal("single entry should pass through")
	}
}
