package utils

import "strings"

// JSONFieldExtractor extracts the string value of a given field from streaming JSON fragments.
//
// When an LLM streams a tool call, the arguments arrive fragment by fragment (OpenAI/Anthropic)
// or all at once (Gemini). This extractor scans character by character with a state machine,
// and once the target key is detected it extracts the string value, handling JSON escapes.
type JSONFieldExtractor struct {
	key      string // the target to match, such as `"content"` or `"task"`
	state    extractState
	matchPos int
	escape   bool
	buf      strings.Builder
}

type extractState int

const (
	stateScan    extractState = iota // scanning, looking for the target key
	stateColon                       // key matched, waiting for the colon and opening quote
	stateExtract                     // extracting the string value
)

func NewFieldExtractor(fieldName string) *JSONFieldExtractor {
	return &JSONFieldExtractor{key: `"` + fieldName + `"`}
}

// Feed processes a delta and returns the extracted text (possibly empty).
func (e *JSONFieldExtractor) Feed(delta string) string {
	e.buf.Reset()
	for _, r := range delta {
		switch e.state {
		case stateScan:
			e.feedScan(r)
		case stateColon:
			e.feedColon(r)
		case stateExtract:
			e.feedExtract(r)
		}
	}
	return e.buf.String()
}

func (e *JSONFieldExtractor) feedScan(r rune) {
	if e.matchPos < len(e.key) && byte(r) == e.key[e.matchPos] {
		e.matchPos++
		if e.matchPos == len(e.key) {
			e.state = stateColon
			e.matchPos = 0
		}
		return
	}
	e.matchPos = 0
	if byte(r) == e.key[0] {
		e.matchPos = 1
	}
}

func (e *JSONFieldExtractor) feedColon(r rune) {
	switch r {
	case ':', ' ', '\t':
		// skip
	case '"':
		e.state = stateExtract
		e.escape = false
	default:
		e.state = stateScan
		e.matchPos = 0
		if byte(r) == e.key[0] {
			e.matchPos = 1
		}
	}
}

func (e *JSONFieldExtractor) feedExtract(r rune) {
	if e.escape {
		e.escape = false
		switch r {
		case 'n':
			e.buf.WriteByte('\n')
		case 't':
			e.buf.WriteByte('\t')
		case 'r':
			e.buf.WriteByte('\r')
		case '"', '\\', '/':
			e.buf.WriteRune(r)
		default:
			e.buf.WriteByte('\\')
			e.buf.WriteRune(r)
		}
		return
	}
	switch r {
	case '\\':
		e.escape = true
	case '"':
		e.state = stateScan
		e.matchPos = 0
	default:
		e.buf.WriteRune(r)
	}
}

// Reset resets the state (called at the start of a new LLM message turn).
func (e *JSONFieldExtractor) Reset() {
	e.state = stateScan
	e.matchPos = 0
	e.escape = false
}

// ThinkingSep is the separator marker between thinking text and body text.
// StreamFilter inserts this marker before a thinking-text segment, and the TUI switches its rendering style accordingly.
const ThinkingSep = "\x02"

// StreamFilter distinguishes a SubAgent's text replies from JSON tool calls.
// Text replies are marked as thinking content (prefix ThinkingSep); for JSON tool calls only the specified field is extracted.
//
// Criterion: on encountering { it enters JSON mode (tracking brace depth),
// and when the depth returns to zero it goes back to text mode.
type StreamFilter struct {
	fieldExt   *JSONFieldExtractor
	mode       filterMode
	braceDepth int
	inString   bool // inside a JSON string (braces are not counted)
	escJSON    bool // escape inside a JSON string
	thinking   bool // currently in a thinking-text segment
	buf        strings.Builder
}

type filterMode int

const (
	filterText filterMode = iota // text reply, passed through directly
	filterJSON                   // JSON tool call, extract the target field
)

func NewStreamFilter(fieldName string) *StreamFilter {
	return &StreamFilter{fieldExt: NewFieldExtractor(fieldName)}
}

// Feed processes a delta and returns displayable text.
// Text replies are output directly; target-field values inside JSON are extracted and output; the rest of the JSON structure is discarded.
func (f *StreamFilter) Feed(delta string) string {
	f.buf.Reset()
	for _, r := range delta {
		switch f.mode {
		case filterText:
			if r == '{' {
				f.thinking = false
				f.mode = filterJSON
				f.braceDepth = 1
				f.inString = false
				f.escJSON = false
				f.fieldExt.Reset()
				f.feedExtractor(r)
			} else {
				if !f.thinking {
					f.thinking = true
					f.buf.WriteString(ThinkingSep)
				}
				f.buf.WriteRune(r)
			}
		case filterJSON:
			f.feedExtractor(r)
			f.trackBraces(r)
		}
	}
	return f.buf.String()
}

// feedExtractor feeds a single character to fieldExt and writes the extracted result into buf.
func (f *StreamFilter) feedExtractor(r rune) {
	if text := f.fieldExt.Feed(string(r)); text != "" {
		f.buf.WriteString(text)
	}
}

// trackBraces tracks JSON brace depth and switches back to text mode when the depth returns to zero.
func (f *StreamFilter) trackBraces(r rune) {
	if f.escJSON {
		f.escJSON = false
		return
	}
	if f.inString {
		switch r {
		case '\\':
			f.escJSON = true
		case '"':
			f.inString = false
		}
		return
	}
	switch r {
	case '"':
		f.inString = true
	case '{':
		f.braceDepth++
	case '}':
		f.braceDepth--
		if f.braceDepth <= 0 {
			f.mode = filterText
		}
	}
}

// Reset resets the state.
func (f *StreamFilter) Reset() {
	f.mode = filterText
	f.braceDepth = 0
	f.inString = false
	f.escJSON = false
	f.thinking = false
	f.fieldExt.Reset()
}
