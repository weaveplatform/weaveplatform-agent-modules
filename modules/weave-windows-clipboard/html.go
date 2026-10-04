//go:build windows

package main

import (
	"fmt"
	"strconv"
	"strings"
)

// CF_HTML ("HTML Format") is UTF-8 HTML behind a header of byte offsets:
//
//	Version:0.9
//	StartHTML:0000000105
//	EndHTML:0000000199
//	StartFragment:0000000141
//	EndFragment:0000000163
//
// The offsets count from the start of the header. The other OSes carry bare
// HTML, so the header is removed on read and added on write.

// The comments CF_HTML wraps the copied fragment in.
const (
	startMarker = "<!--StartFragment-->"
	endMarker   = "<!--EndFragment-->"
)

// fromCFHTML returns the HTML a CF_HTML block carries: the fragment, which is
// what was copied and what the other OSes carry as bare HTML, so HTML written
// here reads back byte for byte.
//
// The offsets are byte offsets from the start of the header, which may end
// its lines in CRLF or LF. Applications get them slightly wrong — Office and
// browsers have both shipped EndFragment offsets a few bytes past the end
// marker — so the fragment offsets are trusted only when they fit the block
// and, where the block has the fragment markers, sit right against them.
// Otherwise the markers say where the fragment is. Failing both, the document
// from StartHTML to EndHTML is returned, and failing that the block from its
// first tag: a wrong offset costs the precision of the cut, not the content.
func fromCFHTML(raw []byte) string {
	s := string(trimNUL(raw))
	if !strings.HasPrefix(s, "Version:") {
		return s
	}
	h := header(s)
	start, end := h["StartFragment"], h["EndFragment"]
	fits := start > 0 && start <= end && end <= len(s)
	ms := strings.Index(s, startMarker)
	me := -1
	if ms >= 0 {
		if i := strings.Index(s[ms+len(startMarker):], endMarker); i >= 0 {
			me = ms + len(startMarker) + i
		}
	}
	switch {
	case fits && me < 0:
		return s[start:end] // no markers to check the offsets against
	case fits && strings.HasSuffix(s[:start], startMarker) && strings.HasPrefix(s[end:], endMarker):
		// Offsets that sit against the markers win over a search for them,
		// which a fragment that quotes the end marker itself would fool.
		return s[start:end]
	case me >= 0:
		return s[ms+len(startMarker) : me]
	}
	if start, end := h["StartHTML"], h["EndHTML"]; start > 0 && start <= end && end <= len(s) {
		return s[start:end]
	}
	if i := strings.Index(s, "<"); i >= 0 {
		return s[i:]
	}
	return ""
}

// header reads the header's numeric fields. A field that is absent or not a
// number reads as 0, which fromCFHTML treats as unusable.
func header(s string) map[string]int {
	out := make(map[string]int)
	for line := range strings.SplitSeq(s, "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), ":")
		if !ok || strings.HasPrefix(k, "<") {
			break
		}
		if n, err := strconv.Atoi(v); err == nil {
			out[k] = n
		}
	}
	return out
}

// cfHTMLHeader has fixed-width offsets, so its length does not depend on the
// numbers in it: formatting it once with zeros measures it.
const cfHTMLHeader = "Version:0.9\r\nStartHTML:%010d\r\nEndHTML:%010d\r\n" +
	"StartFragment:%010d\r\nEndFragment:%010d\r\n"

const (
	fragmentStart = "<html><body>" + startMarker
	fragmentEnd   = endMarker + "</body></html>"
)

// toCFHTML wraps HTML as the fragment of a CF_HTML document, which is what
// Windows applications paste.
func toCFHTML(fragment string) []byte {
	headerLen := len(fmt.Sprintf(cfHTMLHeader, 0, 0, 0, 0))
	startFragment := headerLen + len(fragmentStart)
	endFragment := startFragment + len(fragment)
	endHTML := endFragment + len(fragmentEnd)
	return []byte(fmt.Sprintf(cfHTMLHeader, headerLen, endHTML, startFragment, endFragment) +
		fragmentStart + fragment + fragmentEnd + "\x00")
}
