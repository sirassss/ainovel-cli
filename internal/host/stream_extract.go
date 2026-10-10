package host

import (
	"strings"
	"unicode/utf8"
)

// toolDisplays configures the display policy of each tool on the stream panel. Tools not in this table take no part in streaming
// rendering (the observer drops their DeltaToolCall directly).
//
// Generic mode (nakedKey empty): the tokenizer renders the args JSON output by the LLM as indented
// "key: value" text, nested objects/arrays are indented by level, and string/number/bool values stream out.
// It is fully decoupled from the schema: if the LLM outputs one more field, the panel gets one more row, with no code change needed.
//
// Naked-stream mode (nakedKey non-empty): only the string value of the target top-level field flows out verbatim, and all other fields
// are skipped. Used for draft_chapter so the whole-chapter markdown is not decorated into "content: # …".
// The header always starts with "✻ ": this is the agreed prefix for the TUI's renderStreamContent to take the renderAgentBlock
// highlight path (gold ✻ + cyan-background blue-underline label + dim rule), consistent with the fallback
// header (streamHeaderFallback); changed to plain text it would fall to the body path and be drawn in the terminal
// default color, and the title would no longer stand out.
var toolDisplays = map[string]toolDisplay{
	"draft_chapter": {nakedKey: "content"},

	"plan_chapter":        {header: "✻ Kế hoạch"},
	"edit_chapter":        {header: "✻ Trau chuốt"},
	"commit_chapter":      {header: "✻ Nộp chương"},
	"save_review":         {header: "✻ Đánh giá"},
	"save_arc_summary":    {header: "✻ Tóm tắt cung"},
	"save_volume_summary": {header: "✻ Tóm tắt tập"},
	"save_foundation":     {header: "✻ Thiết lập"},
	"revise_outline":      {header: "✻ Sửa dàn ý"},
	"read_chapter":        {header: "✻ Đọc chương"},
	"check_consistency":   {header: "✻ Kiểm tra nhất quán"},
	"novel_context":       {header: "✻ Tra ngữ cảnh"},
}

type toolDisplay struct {
	header   string
	nakedKey string
}

// jsonFieldExtractor is a streaming JSON tokenizer. It drives a state machine byte by byte and turns the LLM's tool
// args stream into readable text. One instance serves only one tool call; after the top-level container closes, Done()=true.
type jsonFieldExtractor struct {
	cfg toolDisplay

	state pState
	stack []byte // container stack: 'O' obj / 'A' arr

	keyBuf strings.Builder

	escape bool
	uHex   []byte

	started bool // whether any character has been emitted (for the newline between the header and the first key)

	done bool
}

type pState int

const (
	psRoot         pState = iota
	psBeforeKey           // inside obj: waiting for the next key or }
	psInKey               // inside obj: parsing a key
	psAfterKey            // inside obj: waiting for :
	psBeforeValue         // waiting for the first character of a value
	psStringStream        // string value, streaming out cooked characters
	psStringSkip          // string value, skipped (a non-target field in naked-stream mode)
	psNumberStream        // number, streaming out
	psNumberSkip          // number, skipped
	psPrimStream          // true/false/null, streaming out
	psPrimSkip            // true/false/null, skipped
	psDone                // top-level container closed
)

func newToolExtractor(tool string) *jsonFieldExtractor {
	cfg, ok := toolDisplays[tool]
	if !ok {
		return nil
	}
	return &jsonFieldExtractor{cfg: cfg}
}

func (e *jsonFieldExtractor) Done() bool { return e.done }

func (e *jsonFieldExtractor) Feed(chunk string) string {
	if e.done || chunk == "" {
		return ""
	}
	var out strings.Builder
	for i := 0; i < len(chunk); i++ {
		e.step(chunk[i], &out)
		if e.done {
			break
		}
	}
	return out.String()
}

// ── Container stack / indentation ──

func (e *jsonFieldExtractor) push(kind byte) {
	e.stack = append(e.stack, kind)
}

func (e *jsonFieldExtractor) pop() {
	if len(e.stack) == 0 {
		return
	}
	e.stack = e.stack[:len(e.stack)-1]
}

func (e *jsonFieldExtractor) parent() byte {
	if len(e.stack) == 0 {
		return 0
	}
	return e.stack[len(e.stack)-1]
}

// writeIndent writes the current indentation. Depth = nesting level = len(stack)-1 (no indentation inside the root container).
func (e *jsonFieldExtractor) writeIndent(out *strings.Builder) {
	depth := len(e.stack) - 1
	for range depth {
		out.WriteString("  ")
	}
}

// ── State machine ──

func (e *jsonFieldExtractor) step(c byte, out *strings.Builder) {
	switch e.state {
	case psRoot:
		switch c {
		case '{':
			e.push('O')
			e.state = psBeforeKey
		case '[':
			// Does not actually happen (tool args are always an obj); tolerated: when the root arr
			e.push('A')
			e.state = psBeforeValue
		}
	case psBeforeKey:
		switch c {
		case '"':
			e.keyBuf.Reset()
			e.escape = false
			e.state = psInKey
		case '}':
			e.closeContainer(out)
		case ' ', '\t', '\n', '\r', ',':
		}
	case psInKey:
		if e.escape {
			e.keyBuf.WriteByte(c)
			e.escape = false
			return
		}
		if c == '\\' {
			e.escape = true
			return
		}
		if c == '"' {
			e.emitKeyLine(out, e.keyBuf.String())
			e.state = psAfterKey
			return
		}
		e.keyBuf.WriteByte(c)
	case psAfterKey:
		if c == ':' {
			e.state = psBeforeValue
		}
	case psBeforeValue:
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			return
		}
		switch c {
		case '"':
			e.beginString(out)
		case '{':
			e.beginNested('O', out)
		case '[':
			e.beginNested('A', out)
		case ']', '}':
			e.closeContainer(out)
		case 't', 'f', 'n':
			e.beginPrim(c, out)
		default:
			if c == '-' || (c >= '0' && c <= '9') {
				e.beginNumber(c, out)
			}
		}
	case psStringStream:
		e.handleStringByte(c, out, false)
	case psStringSkip:
		e.handleStringByte(c, out, true)
	case psNumberStream:
		if isNumberByte(c) {
			out.WriteByte(c)
			return
		}
		e.afterValueChar(c, out)
	case psNumberSkip:
		if isNumberByte(c) {
			return
		}
		e.afterValueChar(c, out)
	case psPrimStream:
		if c >= 'a' && c <= 'z' {
			out.WriteByte(c)
			return
		}
		e.afterValueChar(c, out)
	case psPrimSkip:
		if c >= 'a' && c <= 'z' {
			return
		}
		e.afterValueChar(c, out)
	case psDone:
	}
}

// ── Line rendering ──

// emitKeyLine is called when the key inside an obj has been parsed, and writes the "<lf><indent>key:" prefix.
// In naked-stream mode no key prefix is written (the key is recorded in keyBuf for beginString to judge).
func (e *jsonFieldExtractor) emitKeyLine(out *strings.Builder, key string) {
	if e.cfg.nakedKey != "" {
		return
	}
	if !e.started {
		if e.cfg.header != "" {
			out.WriteString(e.cfg.header)
			out.WriteByte('\n')
		}
		e.started = true
	} else {
		out.WriteByte('\n')
	}
	e.writeIndent(out)
	out.WriteString(key)
	out.WriteByte(':')
}

// emitArrayItem is called at the start of each element in an arr and writes "<lf><indent>-". For primitive
// elements a space follows and then the value is emitted; struct elements are handled by the later nesting breaking lines naturally.
func (e *jsonFieldExtractor) emitArrayItem(out *strings.Builder) {
	if e.cfg.nakedKey != "" {
		return
	}
	if !e.started {
		if e.cfg.header != "" {
			out.WriteString(e.cfg.header)
			out.WriteByte('\n')
		}
		e.started = true
	} else {
		out.WriteByte('\n')
	}
	e.writeIndent(out)
	out.WriteByte('-')
}

// ── Value start ──

func (e *jsonFieldExtractor) beginString(out *strings.Builder) {
	if e.cfg.nakedKey != "" {
		// Naked stream: only the string value of the target key in the top-level obj is output
		if e.cfg.nakedKey == e.keyBuf.String() && len(e.stack) == 1 && e.stack[0] == 'O' {
			e.state = psStringStream
		} else {
			e.state = psStringSkip
		}
		e.escape = false
		e.uHex = nil
		return
	}
	// Generic: an obj field is followed by "key: " ("key:" already emitted, then add a space); an arr element is followed by "- "
	if e.parent() == 'A' {
		e.emitArrayItem(out)
		out.WriteByte(' ')
	} else {
		out.WriteByte(' ')
	}
	e.state = psStringStream
	e.escape = false
	e.uHex = nil
}

func (e *jsonFieldExtractor) beginNumber(first byte, out *strings.Builder) {
	if e.cfg.nakedKey != "" {
		e.state = psNumberSkip
		return
	}
	if e.parent() == 'A' {
		e.emitArrayItem(out)
		out.WriteByte(' ')
	} else {
		out.WriteByte(' ')
	}
	out.WriteByte(first)
	e.state = psNumberStream
}

func (e *jsonFieldExtractor) beginPrim(first byte, out *strings.Builder) {
	if e.cfg.nakedKey != "" {
		e.state = psPrimSkip
		return
	}
	if e.parent() == 'A' {
		e.emitArrayItem(out)
		out.WriteByte(' ')
	} else {
		out.WriteByte(' ')
	}
	out.WriteByte(first)
	e.state = psPrimStream
}

func (e *jsonFieldExtractor) beginNested(kind byte, out *strings.Builder) {
	if e.cfg.nakedKey != "" {
		// Naked-stream mode does not expand nesting; track with stack depth to the matching } / ]
		e.push(kind)
		if kind == 'O' {
			e.state = psBeforeKey
		} else {
			e.state = psBeforeValue
		}
		return
	}
	// Generic mode: when an arr element is a nested structure, first emit a separate line "<indent>-"
	// (no space after the ":" of an obj key, so the nested child key naturally breaks to the next line)
	if e.parent() == 'A' {
		e.emitArrayItem(out)
	}
	e.push(kind)
	if kind == 'O' {
		e.state = psBeforeKey
	} else {
		e.state = psBeforeValue
	}
}

// closeContainer handles } or ].
func (e *jsonFieldExtractor) closeContainer(out *strings.Builder) {
	e.pop()
	if len(e.stack) == 0 {
		// Empty args (e.g. novel_context takes no arguments) fallback: emitKeyLine never had a chance to output the header,
		// so add it here, avoiding ending up with "neither title nor content".
		if !e.started && e.cfg.nakedKey == "" && e.cfg.header != "" {
			out.WriteString(e.cfg.header)
			out.WriteByte('\n')
			e.started = true
		}
		// A trailing newline gives a clear boundary between the panel and the next output
		if e.started {
			out.WriteByte('\n')
		}
		e.state = psDone
		e.done = true
		return
	}
	if e.parent() == 'O' {
		e.state = psBeforeKey
	} else {
		e.state = psBeforeValue
	}
}

// ── String streaming ──

func (e *jsonFieldExtractor) handleStringByte(c byte, out *strings.Builder, skipping bool) {
	if e.uHex != nil {
		e.uHex = append(e.uHex, c)
		if len(e.uHex) == 4 {
			if r, ok := parseHex4(e.uHex); ok && !skipping {
				var buf [4]byte
				n := utf8.EncodeRune(buf[:], r)
				out.Write(buf[:n])
			}
			e.uHex = nil
		}
		return
	}
	if e.escape {
		e.escape = false
		if !skipping {
			writeEscapedByte(out, c)
		}
		if c == 'u' {
			e.uHex = make([]byte, 0, 4)
		}
		return
	}
	if c == '\\' {
		e.escape = true
		return
	}
	if c == '"' {
		e.afterValueDone()
		return
	}
	if !skipping {
		out.WriteByte(c)
	}
}

func writeEscapedByte(out *strings.Builder, c byte) {
	switch c {
	case 'n':
		out.WriteByte('\n')
	case 't':
		out.WriteByte('\t')
	case 'r':
		out.WriteByte('\r')
	case '"':
		out.WriteByte('"')
	case '\\':
		out.WriteByte('\\')
	case '/':
		out.WriteByte('/')
	case 'b', 'f':
		// Backspace / form feed: ignored
	case 'u':
		// The caller builds the uHex buffer; nothing is output here
	default:
		out.WriteByte('\\')
		out.WriteByte(c)
	}
}

// ── Wrap-up ──

// afterValueDone transitions to the next state after a string closes (the closing `"` has been read).
func (e *jsonFieldExtractor) afterValueDone() {
	e.escape = false
	e.uHex = nil
	if len(e.stack) == 0 {
		e.state = psDone
		e.done = true
		return
	}
	if e.parent() == 'O' {
		e.state = psBeforeKey
	} else {
		e.state = psBeforeValue
	}
}

// afterValueChar decides the next state by character when the "end character" of a number / primitive has been read.
// That character may be , / } / ] / whitespace, and this function dispatches on it.
func (e *jsonFieldExtractor) afterValueChar(c byte, out *strings.Builder) {
	switch c {
	case '}', ']':
		e.closeContainer(out)
	case ',', ' ', '\t', '\n', '\r':
		if len(e.stack) == 0 {
			e.state = psDone
			e.done = true
			return
		}
		if e.parent() == 'O' {
			e.state = psBeforeKey
		} else {
			e.state = psBeforeValue
		}
	}
}

// ── Utilities ──

func isNumberByte(c byte) bool {
	switch c {
	case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9',
		'-', '+', '.', 'e', 'E':
		return true
	}
	return false
}

func parseHex4(b []byte) (rune, bool) {
	var r rune
	for _, d := range b {
		var v rune
		switch {
		case d >= '0' && d <= '9':
			v = rune(d - '0')
		case d >= 'a' && d <= 'f':
			v = rune(d-'a') + 10
		case d >= 'A' && d <= 'F':
			v = rune(d-'A') + 10
		default:
			return 0, false
		}
		r = r*16 + v
	}
	return r, true
}
