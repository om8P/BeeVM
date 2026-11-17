package main

import (
	"bytes"
	"testing"
)

func TestPrintLog(t *testing.T) {
	for _, tc := range []struct {
		log   string
		total int64
		want  string
	}{
		{"", 0, ""}, // the function printed nothing: no title either
		{"a\nb\n", 4, "function log:\na\nb\n"},
		{"end\n", 3000, "function log (last 4 B of 2.9 KB):\nend\n"},
	} {
		var buf bytes.Buffer
		printLog(&buf, tc.log, tc.total)
		if buf.String() != tc.want {
			t.Errorf("printLog(%q, %d) = %q, want %q", tc.log, tc.total, buf.String(), tc.want)
		}
	}
}
