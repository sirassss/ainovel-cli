package host

import (
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/utils"
)

// handleSubagentDelta routes a subagent's text and tool-call arguments:
// - DeltaText flows out directly as markdown
// - DeltaToolCall only extracts and streams fields for known long-content tools (such as draft_chapter.content); the argument JSON of other tools is discarded entirely
func (o *observer) handleSubagentDelta(p *agentcore.ProgressPayload) {
	if p.DeltaKind != agentcore.DeltaToolCall {
		o.emitStreamDelta(p.Delta, false)
		return
	}
	if p.Tool == "" {
		return // tool name not ready; try again on the next delta
	}

	// When the tool name is recognized in the stream, emit a TOOL in-progress event early so the spinner covers the whole LLM generation period
	// (otherwise the "in progress" state of tools like draft_chapter would only show during the few dozen milliseconds of the real Execute).
	// When the real ProgressToolStart arrives it finds an existing toolStarts record and only fills in the summary.
	o.ensureSubagentToolStarted(p.Agent, p.Tool)
	o.updateToolCallSummaryFromDelta(p.Agent, p.Tool, p.Delta)

	cur, ok := o.streamExtractors[p.Agent]
	// After the same tool call's args have closed (top-level } hit), trailing deltas may still arrive:
	// some providers (observed with deepseek-v4-flash) split a single args into several chunks,
	// and the very last chunk carries whitespace or repeated characters after the `}`. If this were handled as
	// "tool name matches + rebuild once Done", the new extractor would emit a ✻ header again and parse the tail
	// tokens as new args. These deltas are redundant tails and can simply be discarded.
	if ok && cur.tool == p.Tool && cur.ext.Done() {
		return
	}
	// Tool name changed or never built: create a new one.
	if !ok || cur.tool != p.Tool {
		ext := newToolExtractor(p.Tool)
		if ext == nil {
			delete(o.streamExtractors, p.Agent)
			return
		}
		cur = &agentExtractor{tool: p.Tool, ext: ext}
		o.streamExtractors[p.Agent] = cur
	}
	if emitted := cur.ext.Feed(p.Delta); emitted != "" {
		if !cur.emittedAny {
			cur.emittedAny = true
			// streamClear makes the extractor's ✻ header land at the start of the new round, so that
			// renderStreamContent's HasPrefix("✻") check takes the renderAgentBlock highlight
			// path; using ensureStreamParagraphBreak only inserts a blank line without opening a round, so ✻ would still be
			// wrapped by the preceding thinking/body text and fall into renderChapterBlock, drawn in the default color.
			o.streamClear()
			// streamClear defensively cleared streamExtractors. The current cur must keep being Fed
			// the later deltas of this tool call, so it must be registered back immediately; otherwise when the next delta
			// arrives a new extractor would be created and start parsing from the middle of args (entering psBeforeKey only
			// at the `{` of a nested object), treating timeline_events.time / foreshadow_updates.id
			// and the like as top-level fields, so the ✻ header shows up repeatedly in the TUI.
			o.streamExtractors[p.Agent] = cur
		}
		o.emitStreamDelta(emitted, false)
	}
}

func (o *observer) emitStreamDelta(delta string, thinking bool) {
	if delta == "" {
		return
	}
	if thinking != o.streamThinking {
		o.emitD(utils.ThinkingSep)
		o.streamThinking = thinking
	}
	o.emitD(delta)
	o.streamHasContent = true
	o.streamLastByte = delta[len(delta)-1]
}

// ensureSubagentToolStarted registers an in-progress TOOL call for the agent in advance when a tool_call is first recognized in the stream,
// so the event-stream spinner covers the period in which "the LLM streams the tool_call
// arguments" (usually 99% of the call's total time). The args are not complete yet, so the plain tool name is used as the
// summary for now; when the real ProgressToolStart arrives the summary with arguments is filled in.
func (o *observer) ensureSubagentToolStarted(agent, tool string) {
	if agent == "" || tool == "" {
		return
	}
	if _, ok := o.toolStarts[agent]; ok {
		return // an in-progress call already exists; idempotent
	}
	o.resetStreamArgLabel(agent, tool)
	id := nextEventID()
	o.toolStarts[agent] = &activeCall{
		id:      id,
		start:   time.Now(),
		summary: tool, // start with the plain tool name; it may be updated to tool(chapter N) when ProgressToolStart arrives
		depth:   1,
	}
	o.emitAndLog(Event{
		ID:       id,
		Time:     time.Now(),
		Category: "TOOL",
		Agent:    agent,
		Summary:  tool,
		Level:    "info",
		Depth:    1,
	})
	o.updateAgent(agent, func(a *agentState) {
		a.state = "working"
		a.tool = tool
	})
	o.emitFallbackStreamHeader(tool)
}

func (o *observer) resetStreamArgLabel(agent, tool string) {
	key := streamArgKey(agent, tool)
	delete(o.streamArgPrefixes, key)
	delete(o.streamArgLabels, key)
}

// emitFallbackStreamHeader adds a ✻ title line to the stream panel for tools with no configured extractor.
// Both paths must call it to stay consistent:
//  1. ensureSubagentToolStarted — subagent streamed tool args (DeltaToolCall)
//  2. handleToolUpdate ProgressToolStart — subagent non-streamed tool args
//
// If either is missing, the tool titles of streaming and non-streaming models will behave inconsistently.
func (o *observer) emitFallbackStreamHeader(tool string) {
	if _, has := toolDisplays[tool]; has {
		return // there is an extractor; it emits the header itself
	}
	o.streamClear()
	o.emitStreamDelta(streamHeaderFallback(tool)+"\n", false)
}

// streamHeaderFallback generates the streaming header text for tools with no configured extractor,
// so users can see "what is being called" even for lightweight read-type tools.
//
// The prefix "✻ " is the agreed "agent dispatch block" marker — when the TUI's renderStreamContent sees this
// prefix it renders through the renderAgentBlock path (icon + highlighted label + divider line);
// otherwise it falls to the body-block path with the terminal default color, and the header looks like ordinary body text, not eye-catching.
func streamHeaderFallback(tool string) string {
	return "✻ " + tool
}

// streamClear tells the TUI to open a new streamRound and resets the state related to paragraph separation.
// Logically a new round is an "empty stream"; otherwise the first extractor emit next time would wrongly pad a leading blank line.
//
// streamThinking must be reset along with it: emitStreamDelta uses streamThinking to track across calls
// whether the previous segment was thinking. No content has been output in the new round yet, so the next emit(thinking=false)
// should not insert a ThinkingSep again. Otherwise the fallback header (such as ✻ read chapter) would be
// taken at the head by \x02, the HasPrefix("✻") of renderStreamContent would fail to match, the whole segment would fall to the body path
// and then be split into thinking segments by ThinkingSep, and the title color would be drawn in the thinking color.
func (o *observer) streamClear() {
	o.emitC()
	o.streamHasContent = false
	o.streamLastByte = 0
	o.streamThinking = false
	// The previous round's subagent had ProgressToolEnd deleted before it ended; clear defensively here.
	if len(o.streamExtractors) > 0 {
		o.streamExtractors = make(map[string]*agentExtractor)
	}
}
