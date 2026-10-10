package imp

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// Supported source encoding labels, written to the Manifest and progress events; no silent fallback (RFC §7.1).
const (
	encodingUTF8    = "utf-8"
	encodingUTF8BOM = "utf-8-bom"
	encodingGB18030 = "gb18030"
)

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// decoded is the result of one decode: the text plus the encoding actually chosen.
type decoded struct {
	text     string
	encoding string
}

// decodeSource decodes in the order UTF-8 / UTF-8 BOM / GB18030 and returns the chosen encoding.
// If decoding cannot be done reliably or a replacement character appears, it fails outright and the error includes the detection result; "trying GB18030" is never hidden as a silent fallback.
func decodeSource(raw []byte) (decoded, error) {
	if bytes.HasPrefix(raw, utf8BOM) {
		body := raw[len(utf8BOM):]
		if !utf8.Valid(body) {
			return decoded{}, fmt.Errorf("Khai báo UTF-8 BOM nhưng nội dung không phải UTF-8 hợp lệ")
		}
		return decoded{text: string(body), encoding: encodingUTF8BOM}, nil
	}
	if utf8.Valid(raw) {
		return decoded{text: string(raw), encoding: encodingUTF8}, nil
	}
	out, err := simplifiedchinese.GB18030.NewDecoder().Bytes(raw)
	if err != nil {
		return decoded{}, fmt.Errorf("Không phải UTF-8 hợp lệ, và giải mã GB18030 cũng lỗi: %w", err)
	}
	if !utf8.Valid(out) {
		return decoded{}, fmt.Errorf("Kết quả giải mã GB18030 vẫn không phải UTF-8 hợp lệ, không thể giải mã đáng tin cậy")
	}
	if i := bytes.IndexRune(out, utf8.RuneError); i >= 0 {
		return decoded{}, fmt.Errorf("Giải mã GB18030 xuất hiện ký tự thay thế (U+FFFD tại byte %d), không thể giải mã đáng tin cậy; hãy xác nhận mã hóa của tệp", i)
	}
	return decoded{text: string(out), encoding: encodingGB18030}, nil
}

// normalize performs only conversions that do not change literary content: CRLF/CR are unified to LF.
// Blank lines, indentation, title lines and body characters are preserved; leading text, empty chapters, ads and so-called trailing noise are not removed (RFC §7.2).
// The BOM was already stripped in the decodeSource stage.
func normalize(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return text
}

// Ingest reads the source file, decodes and normalizes it, and atomically creates the meta/import/ workspace snapshot via a directory rename.
// It returns the workspace handle and the Manifest; the caller emits progress events from them.
func Ingest(bookDir, sourcePath string, in Intent) (*Workspace, *Manifest, error) {
	raw, err := os.ReadFile(sourcePath)
	if err != nil {
		return nil, nil, fmt.Errorf("Đọc tệp nguồn: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil, fmt.Errorf("Tệp nguồn rỗng: %s", sourcePath)
	}
	dec, err := decodeSource(raw)
	if err != nil {
		return nil, nil, err
	}
	normBytes := []byte(normalize(dec.text))

	m := Manifest{
		Version:          workspaceSchemaVersion,
		SourceName:       filepath.Base(sourcePath),
		RawSHA256:        Digest(raw),
		NormalizedSHA256: Digest(normBytes),
		Encoding:         dec.encoding,
		SizeBytes:        int64(len(raw)),
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
	}
	if in.Version == 0 {
		in.Version = workspaceSchemaVersion
	}

	ws, err := createWorkspace(bookDir, m, in, normBytes)
	if err != nil {
		return nil, nil, err
	}
	return ws, &m, nil
}

// SourceUnit is a stable coordinate the model can reference (RFC §7.3).
// ID is only for display and model references; all ordering/containment/increment checks use the numeric (Line, Part) order, and lexicographic comparison of ID strings is forbidden.
type SourceUnit struct {
	ID        string `json:"id"`   // L1257; an over-budget line is split into L1257.1, L1257.2
	Line      int    `json:"line"` // 1-based
	Part      int    `json:"part"` // 0=whole line; virtual slices 1..N
	StartByte int    `json:"start_byte"`
	EndByte   int    `json:"end_byte"`
	Text      string `json:"text"`
}

// unitLess defines the total order of SourceUnit: Line first, then Part, both compared numerically (A1 revision).
func unitLess(a, b SourceUnit) bool {
	if a.Line != b.Line {
		return a.Line < b.Line
	}
	return a.Part < b.Part
}

// buildSourceUnits builds the stable coordinate table from normalized text.
// A normal line is one unit; when a single line's bytes exceed maxUnitBytes, multiple virtual units are generated only at UTF-8 character boundaries,
// without writing back to source.txt, inserting soft line breaks or changing any source character (RFC §7.3). maxUnitBytes<=0 means no slicing.
func buildSourceUnits(normalized []byte, maxUnitBytes int) []SourceUnit {
	var units []SourceUnit
	n := len(normalized)
	line := 0
	offset := 0
	for offset < n {
		nl := bytes.IndexByte(normalized[offset:], '\n')
		lineEnd := n
		if nl >= 0 {
			lineEnd = offset + nl
		}
		line++
		if maxUnitBytes > 0 && lineEnd-offset > maxUnitBytes {
			part := 0
			s := offset
			for s < lineEnd {
				e := s + maxUnitBytes
				if e >= lineEnd {
					e = lineEnd
				} else {
					for e > s && !utf8.RuneStart(normalized[e]) {
						e--
					}
					if e == s { // extreme fallback for a single over-long rune
						e = s + maxUnitBytes
					}
				}
				part++
				units = append(units, SourceUnit{
					ID: fmt.Sprintf("L%d.%d", line, part), Line: line, Part: part,
					StartByte: s, EndByte: e, Text: string(normalized[s:e]),
				})
				s = e
			}
		} else {
			units = append(units, SourceUnit{
				ID: fmt.Sprintf("L%d", line), Line: line, Part: 0,
				StartByte: offset, EndByte: lineEnd, Text: string(normalized[offset:lineEnd]),
			})
		}
		if nl < 0 {
			break
		}
		offset = lineEnd + 1
	}
	return units
}

// resolveBoundaryByte maps a boundary decision to an exact byte position:
// with no anchor it takes the unit start; with an anchor it requires a unique verbatim hit inside that unit, then maps it to a byte offset (RFC §8.3).
func resolveBoundaryByte(unitByID map[string]SourceUnit, unitID, anchor string) (int, error) {
	u, ok := unitByID[unitID]
	if !ok {
		return 0, fmt.Errorf("Ranh giới tham chiếu unit không tồn tại: %s", unitID)
	}
	if anchor == "" {
		return u.StartByte, nil
	}
	switch strings.Count(u.Text, anchor) {
	case 0:
		return 0, fmt.Errorf("Neo %q không nằm trong unit %s", anchor, unitID)
	case 1:
		return u.StartByte + strings.Index(u.Text, anchor), nil
	default:
		return 0, fmt.Errorf("Neo %q không duy nhất trong unit %s", anchor, unitID)
	}
}
