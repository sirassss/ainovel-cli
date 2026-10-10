package diag

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/store"
)

// SkelEvent is the redacted behavior skeleton of one session message: structural signals
// (role / tools / error / repeat fingerprint) are kept, and all free text (novel text, prompts,
// thinking) is masked. It is a stricter projection than store.compactMessage, which compacts by
// size (>4KB); this one ignores size and lets no text out.
type SkelEvent struct {
	Agent    string     // source session: writer-ch07 / architect-arc02 ...
	Role     string     // assistant / tool / user
	Tools    []SkelTool // tool calls within the message
	ErrClass string     // role=tool with is_error: first line of the error (a framework error string, no novel text)
	TextSha  string     // short hash of the masked text; same sha = the same passage generated repeatedly (loop signal)
	Redacted int        // number of text/thinking blocks masked in this event (for the redaction self-check)
}

// SkelTool is the redacted projection of one tool call.
type SkelTool struct {
	Name     string            // tool name (structural signal, no novel text)
	Args     map[string]string // key → raw scalar / quoted short string / "<redacted len sha>"
	Invalid  bool              // ArgsInvalid: the arguments the model sent could not be parsed (#34 signal)
	ParseErr string            // ArgsParseError: why parsing failed
}

// redactMessage projects one agentcore.Message into a behavior skeleton.
func redactMessage(agent string, m agentcore.Message) SkelEvent {
	ev := SkelEvent{Agent: agent, Role: string(m.Role)}
	isErr, _ := m.Metadata["is_error"].(bool)

	var text strings.Builder
	for _, b := range m.Content {
		switch b.Type {
		case agentcore.ContentText:
			// Keep the first line of a tool error result: it is our own error string (e.g.
			// InputValidationError), carries no novel text, and is key to locating a loop.
			// All other text goes into the masking pool.
			if m.Role == agentcore.RoleTool && isErr && ev.ErrClass == "" {
				ev.ErrClass = firstLine(b.Text, 160)
				continue
			}
			if strings.TrimSpace(b.Text) != "" {
				text.WriteString(b.Text)
				ev.Redacted++
			}
		case agentcore.ContentThinking:
			if strings.TrimSpace(b.Thinking) != "" {
				text.WriteString(b.Thinking)
				ev.Redacted++
			}
		case agentcore.ContentToolCall:
			if b.ToolCall != nil {
				ev.Tools = append(ev.Tools, redactToolCall(b.ToolCall))
			}
		}
	}
	if t := text.String(); t != "" {
		ev.TextSha = shortHash(t)
	}
	return ev
}

// redactToolCall projects one tool call: tool name + arguments (values redacted) + parse-failure flag.
func redactToolCall(tc *agentcore.ToolCall) SkelTool {
	return SkelTool{
		Name:     tc.Name,
		Args:     redactArgs(tc.Args),
		Invalid:  tc.ArgsInvalid,
		ParseErr: tc.ArgsParseError,
	}
}

// redactArgs projects a tool argument object into key → redacted value. Non-object arguments
// return nil (ArgsInvalid/ParseErr are recorded separately on SkelTool).
func redactArgs(raw json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = projectValue(v)
	}
	return out
}

// projectValue projects a single argument value by JSON type:
//   - scalar (number / bool / null): the raw value is a structural signal, kept (chapter: 7)
//   - short identifier-like string: kept quoted, exposing the type (chapter: "7" ← the stringified-number signal of #34)
//   - string with Chinese text / spaces / long text, object, array: masked as <redacted ...> (no novel text leaves)
//   - already a [session_compact: ...] placeholder: safe and informative, kept as is
func projectValue(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return ""
	}
	switch s[0] {
	case '"':
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return redactPlaceholder(s)
		}
		if strings.HasPrefix(str, store.CompactTag) {
			return str
		}
		// Only keep short values that look like an identifier / number / enum (chapter:"7",
		// type:"premise", agent:"writer"); any string with Chinese text, spaces or other symbols
		// is treated as novel text and always masked.
		if utf8.RuneCountInString(str) <= 32 && isStructuralToken(str) {
			return strconv.Quote(str)
		}
		return redactPlaceholder(str)
	case '{':
		return fmt.Sprintf("<redacted object len=%d>", len(raw))
	case '[':
		return fmt.Sprintf("<redacted array len=%d>", len(raw))
	default:
		return s
	}
}

// isStructuralToken reports whether a string "looks like an identifier": pure ASCII letters / digits / `_-.:/`,
// with no spaces and no Chinese text. It separates structural signals (kept) from novel text fragments (masked).
func isStructuralToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.' || r == ':' || r == '/':
		default:
			return false
		}
	}
	return true
}

func redactPlaceholder(s string) string {
	return fmt.Sprintf("<redacted len=%d sha=%s>", utf8.RuneCountInString(s), shortHash(s))
}

// shortHash takes a short hash of the text; it is only used to tell whether the same text recurs, not for cryptography.
func shortHash(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}

// firstLine takes the first line and truncates it by rune, for error string summaries.
func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\n\r"); i >= 0 {
		s = s[:i]
	}
	if utf8.RuneCountInString(s) > max {
		r := []rune(s)
		s = string(r[:max]) + "…"
	}
	return s
}
