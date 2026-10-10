package utils

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// DecodeText decodes the bytes of a user-supplied text file to UTF-8: for invalid UTF-8 it transcodes as GB18030
// (a GBK superset) - Chinese novel txt files circulating online are mostly GBK-encoded, and reading them directly as UTF-8
// yields only garbage. Byte sequences that are not GBK are replaced by the decoder with U+FFFD (it was garbage anyway; the caller's
// zero-hit fallback error guides the user). Finally the UTF-8 BOM is stripped (otherwise a line-start match would include it).
func DecodeText(data []byte) string {
	if !utf8.Valid(data) {
		if decoded, err := simplifiedchinese.GB18030.NewDecoder().Bytes(data); err == nil {
			data = decoded
		}
	}
	return strings.TrimPrefix(string(data), "\uFEFF")
}
