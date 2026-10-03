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

// fromCFHTML returns the HTML document a CF_HTML block carries: from
// StartHTML to EndHTML, or the fragment when an application gave no document
// (StartHTML of -1, which the format allows). A block whose offsets do not fit
// it is cut at the end of its header instead, so a wrong offset costs the
// precision of the cut, not the content.
func fromCFHTML(raw []byte) string {
	s := string(trimNUL(raw))
	if !strings.HasPrefix(s, "Version:") {
		return s
	}
	h := header(s)
	start, end := h["StartHTML"], h["EndHTML"]
	if start < 0 {
		start, end = h["StartFragment"], h["EndFragment"]
	}
	if start > 0 && start <= end && end <= len(s) {
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
	fragmentStart = "<html><body><!--StartFragment-->"
	fragmentEnd   = "<!--EndFragment--></body></html>"
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
