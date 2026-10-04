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

// fromCFHTML returns the HTML a CF_HTML block carries: the fragment, from
// StartFragment to EndFragment, which is what was copied and what the other
// OSes carry as bare HTML — so HTML written here reads back byte for byte —
// or, when the fragment offsets do not fit the block, the document from
// StartHTML to EndHTML. A block whose offsets fit neither is cut at the end
// of its header instead, so a wrong offset costs the precision of the cut,
// not the content.
func fromCFHTML(raw []byte) string {
	s := string(trimNUL(raw))
	if !strings.HasPrefix(s, "Version:") {
		return s
	}
	h := header(s)
	for _, span := range [][2]string{{"StartFragment", "EndFragment"}, {"StartHTML", "EndHTML"}} {
		start, end := h[span[0]], h[span[1]]
		if start > 0 && start <= end && end <= len(s) {
			return s[start:end]
		}
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
