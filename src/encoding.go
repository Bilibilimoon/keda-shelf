package main

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// displayName turns a file name stored in a legacy Chinese encoding into
// UTF-8. Windows-made archives (and files copied from Windows) commonly carry
// GBK bytes; Linux keeps them as opaque bytes, which show up as mojibake.
//
// Names that are already valid UTF-8 are returned untouched, so ordinary Linux
// paths are never reinterpreted. Path/Rel keep the original bytes and are
// still what the file is opened with; only what users see changes.
func displayName(raw string) string {
	if raw == "" || utf8.ValidString(raw) {
		return raw
	}
	if out, err := simplifiedchinese.GBK.NewDecoder().String(raw); err == nil && isCleanUTF8(out) {
		return out
	}
	if out, err := simplifiedchinese.GB18030.NewDecoder().String(raw); err == nil && isCleanUTF8(out) {
		return out
	}
	return raw
}

// isCleanUTF8 rejects results that contain the replacement rune: a decoder
// that silently substitutes invalid bytes would turn a binary-looking name
// into a string of U+FFFD, which is worse than showing the original bytes.
func isCleanUTF8(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsRune(s, utf8.RuneError)
}

// zipEntryName is the display name of a zip entry. Entries written by
// non-Unicode tools leave the UTF-8 flag unset; those are decoded as GBK.
func zipEntryName(f *zip.File) string {
	if f.NonUTF8 {
		return displayName(f.Name)
	}
	return f.Name
}

var xmlDeclEncodingRe = regexp.MustCompile(`(?i)(<\?xml[^>]*?encoding\s*=\s*["'])[^"']*(["'])`)

// xmlDecode unmarshals metadata that may not be UTF-8. Chinese comic tools
// write ComicInfo.xml and occasionally content.opf in GBK. The decoder first
// tries the bytes as-is (the common UTF-8 case) and falls back to GBK/GB18030
// when the data is not valid UTF-8.
func xmlDecode(raw []byte, dst any) error {
	if utf8.Valid(raw) {
		// Declared gbk with UTF-8 bytes would make encoding/xml reject the
		// label, so normalise the declaration before parsing.
		return xml.Unmarshal(xmlDeclEncodingRe.ReplaceAll(raw, []byte("${1}UTF-8${2}")), dst)
	}
	for _, enc := range []interface {
		Bytes([]byte) ([]byte, error)
	}{simplifiedchinese.GBK.NewDecoder(), simplifiedchinese.GB18030.NewDecoder()} {
		out, err := enc.Bytes(raw)
		if err != nil || !utf8.Valid(out) {
			continue
		}
		return xml.Unmarshal(xmlDeclEncodingRe.ReplaceAll(out, []byte("${1}UTF-8${2}")), dst)
	}
	return fmt.Errorf("xml: unsupported text encoding")
}
