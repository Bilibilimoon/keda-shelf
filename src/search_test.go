package main

import "testing"

func TestSearchMatchesTitleSeriesAndPath(t *testing.T) {
	sr := &Series{Name: "进击的巨人", Rel: "漫画/进击的巨人"}
	b := &Book{
		Title: "第3卷",
		Rel:   "漫画/进击的巨人/第3卷.cbz",
		Meta:  &bookMeta{Authors: []string{"谏山创"}, Subjects: []string{"热血"}},
	}
	text := bookSearchText(sr, b)
	for _, q := range []string{"进击的巨人", "第3卷", "谏山创", "热血", "漫画/进击", "进击 第3卷"} {
		if !matchesAllTerms(text, searchTerms(q)) {
			t.Errorf("query %q did not match", q)
		}
	}
	for _, q := range []string{"海贼王", "进击 第4卷", ""} {
		if matchesAllTerms(text, searchTerms(q)) {
			t.Errorf("query %q unexpectedly matched", q)
		}
	}
}

func TestSearchTermsLowercases(t *testing.T) {
	got := searchTerms("  One   PIECE  ")
	if len(got) != 2 || got[0] != "one" || got[1] != "piece" {
		t.Fatalf("searchTerms = %#v", got)
	}
}
