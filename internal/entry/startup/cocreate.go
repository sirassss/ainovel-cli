package startup

import (
	"fmt"
	"strings"

	"github.com/voocel/ainovel-cli/internal/host"
)

// CoCreateSession holds the non-UI state of co-create mode.
type CoCreateSession struct {
	history        []host.CoCreateMessage
	draftPrompt    string
	ready          bool
	streamReply    string
	streamThinking string
	suggestions    []string
}

func NewCoCreateSession(initial string) *CoCreateSession {
	return &CoCreateSession{
		history: []host.CoCreateMessage{
			{Role: "user", Content: strings.TrimSpace(initial)},
		},
	}
}

func (s *CoCreateSession) History() []host.CoCreateMessage {
	if s == nil {
		return nil
	}
	return append([]host.CoCreateMessage(nil), s.history...)
}

func (s *CoCreateSession) ApplyReply(reply host.CoCreateReply) {
	if s == nil {
		return
	}
	s.streamReply = ""
	s.streamThinking = ""
	// History stores the assistant's full three-part Raw (including [DRAFT]) so the model can see
	// the draft it wrote last turn and update it cumulatively; storing only Message would keep the
	// [DRAFT] out of the context entirely, forcing the model to re-summarize from the dialogue each
	// turn and losing early details. On the degraded path Raw == Message, so it is equivalent.
	text := strings.TrimSpace(reply.Raw)
	if text == "" {
		text = strings.TrimSpace(reply.Message)
	}
	if text != "" {
		s.history = append(s.history, host.CoCreateMessage{Role: "assistant", Content: text})
	}
	// Overwrite draft only when Prompt is non-empty: the parse degraded path returns Prompt="", and
	// the previous draft must be kept, or a truncated reply would wipe the user's accumulated "current creation instructions".
	if prompt := strings.TrimSpace(reply.Prompt); prompt != "" {
		s.draftPrompt = prompt
	}
	s.ready = reply.Ready
	// Suggestions are overwritten directly (even with empty): each turn's guidance only matters for the present.
	s.suggestions = append(s.suggestions[:0], reply.Suggestions...)
}

func (s *CoCreateSession) AppendUser(text string) {
	if s == nil {
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	// The user has decided what to say next, so the suggestions are invalidated immediately, avoiding
	// stale suggestions sitting on the input box and misleading the user before the AI has replied.
	s.suggestions = nil
	s.history = append(s.history, host.CoCreateMessage{Role: "user", Content: text})
}

// ApplyDelta accumulates streaming output; kind="thinking" goes to the reasoning stream and "reply" to the reply preview.
// The two are accumulated separately so the UI can color them in separate blocks, letting the user see the LLM working during the thinking phase too.
func (s *CoCreateSession) ApplyDelta(kind, text string) {
	if s == nil {
		return
	}
	text = strings.TrimSpace(text)
	switch kind {
	case host.CoCreateProgressThinking:
		s.streamThinking = text
	case host.CoCreateProgressReply:
		s.streamReply = text
	}
}

func (s *CoCreateSession) StreamReply() string {
	if s == nil {
		return ""
	}
	return s.streamReply
}

func (s *CoCreateSession) StreamThinking() string {
	if s == nil {
		return ""
	}
	return s.streamThinking
}

func (s *CoCreateSession) DraftPrompt() string {
	if s == nil {
		return ""
	}
	return s.draftPrompt
}

func (s *CoCreateSession) Suggestions() []string {
	if s == nil {
		return nil
	}
	return s.suggestions
}

func (s *CoCreateSession) Ready() bool {
	if s == nil {
		return false
	}
	return s.ready
}

func (s *CoCreateSession) CanStart() bool {
	return strings.TrimSpace(s.DraftPrompt()) != ""
}

func (s *CoCreateSession) InitialInput() string {
	if s == nil || len(s.history) == 0 {
		return ""
	}
	return strings.TrimSpace(s.history[0].Content)
}

func (s *CoCreateSession) BuildPrompt() (string, error) {
	if s == nil || !s.CanStart() {
		return "", fmt.Errorf("cocreate draft prompt is required")
	}
	return s.DraftPrompt(), nil
}
