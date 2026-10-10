package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/voocel/agentcore/schema"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/store"
)

// SaveFoundationTool saves the foundation settings (premise/outline/characters); Architect only.
type SaveFoundationTool struct {
	store *store.Store
}

func NewSaveFoundationTool(store *store.Store) *SaveFoundationTool {
	return &SaveFoundationTool{store: store}
}

func (t *SaveFoundationTool) Name() string { return "save_foundation" }
func (t *SaveFoundationTool) Description() string {
	return "Lưu thiết lập nền tảng của tiểu thuyết (premise/outline/characters/world_rules/compass, v.v.). **Đây là cổng lưu bền duy nhất**: nội dung không lưu qua công cụ này sẽ không vào store, chỉ xuất Markdown/JSON trong tin nhắn là coi như mất. Tham số cố định là {type, content, scale?, volume?, arc?}. type chọn một trong premise / outline / layered_outline / characters / world_rules / expand_arc / append_volume / update_compass / complete_book (không có type=compass; cập nhật compass dùng update_compass). Với premise thì content phải là chuỗi Markdown; các type khác ưu tiên truyền trực tiếp mảng hoặc đối tượng JSON. expand_arc hiệu chỉnh và mở rộng một cung phác thảo chưa viết (một cung không được quá 8 chương chi tiết — đánh giá cuối cung phải đọc trọn cả cung, quá dài thì không đánh giá nổi; vượt giới hạn thì tách thành nhiều cung; cần volume + arc, content là {title, goal, chapters}, có thể sửa mục tiêu phác thảo ban đầu dựa trên nội dung chương đã hoàn thành); append_volume thêm tập mới (content là VolumeOutline JSON đầy đủ, gồm cấu trúc cung; cấp cao nhất có \"final\": true là tuyên bố tập cuối — cả truyện kết thúc ở tập này, viết xong mọi chương sẽ tự kết thúc truyện, không cần gọi complete_book); update_compass cập nhật hướng kết cục (content là StoryCompass JSON, chỉ có các trường {ending_direction: string bắt buộc, open_threads?: string[], estimated_scale?: string}, mọi trường khác đều không nhận); complete_book tuyên bố kết thúc cả truyện (content truyền đối tượng rỗng {}, đẩy thẳng Phase=Complete; công cụ sẽ kiểm tra: các chương trong dàn ý đã viết hết, không có hàng đợi viết lại, compass không còn open_threads chưa thu — xác nhận tuyến dài đã thu thì phải update_compass xóa sạch open_threads và lưu trước; muốn kết sớm thì dùng append_volume với tập cuối final). append_volume / complete_book bắt buộc kèm tham số reason (lý do phán định một câu, đối chiếu danh sách kiểm tra kết thúc truyện, ghi vào bản ghi kiểm toán). scale tùy chọn, chỉ cho phép short / mid / long."
}
func (t *SaveFoundationTool) Label() string { return "Lưu thiết lập" }

// Write tool (updates Outline/Progress/Characters across domains); concurrency is forbidden.
func (t *SaveFoundationTool) ReadOnly(_ json.RawMessage) bool        { return false }
func (t *SaveFoundationTool) ConcurrencySafe(_ json.RawMessage) bool { return false }

func (t *SaveFoundationTool) Schema() map[string]any {
	return schema.Object(
		schema.Property("type", schema.Enum("Loại thiết lập", "premise", "outline", "layered_outline", "characters", "world_rules", "expand_arc", "append_volume", "update_compass", "complete_book")).Required(),
		schema.Property("content", map[string]any{
			"description": "Nội dung. premise truyền chuỗi Markdown; các type khác truyền thẳng mảng hoặc đối tượng JSON, cũng chấp nhận chuỗi JSON. Với expand_arc truyền {title, goal, chapters}, title/goal là kế hoạch cung mục tiêu đã hiệu chỉnh theo các sự thật đã hoàn thành.",
		}).Required(),
		schema.Property("scale", schema.Enum("Cấp lập kế hoạch", "short", "mid", "long")),
		schema.Property("volume", schema.Int("Số thứ tự tập mục tiêu, tính từ 1 (chỉ bắt buộc khi expand_arc)")),
		schema.Property("arc", schema.Int("Số thứ tự cung mục tiêu trong tập, tính từ 1 (chỉ bắt buộc khi expand_arc)")),
		schema.Property("reason", schema.String("Lý do phán định cuối tập (bắt buộc khi append_volume / complete_book): đối chiếu danh sách kiểm tra kết thúc truyện, một câu nói rõ vì sao viết tiếp tập, tuyên bố tập cuối hay kết thúc truyện")),
	)
}

func (t *SaveFoundationTool) Execute(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	var a struct {
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
		Scale   string          `json:"scale"`
		Volume  int             `json:"volume"`
		Arc     int             `json:"arc"`
		Reason  string          `json:"reason"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("invalid args: %w: %w", errs.ErrToolArgs, err)
	}
	content, err := normalizeFoundationContent(a.Content)
	if err != nil {
		return nil, err
	}
	if a.Scale != "" {
		switch domain.PlanningTier(a.Scale) {
		case domain.PlanningTierShort, domain.PlanningTierMid, domain.PlanningTierLong:
		default:
			return nil, fmt.Errorf("invalid scale %q, expected short/mid/long: %w", a.Scale, errs.ErrToolArgs)
		}
	}

	result := map[string]any{"saved": true, "type": a.Type, "scale": a.Scale}

	// A full outline belongs to the planning phase only. While writing, the protected incremental operations must be used, and after completion the book must be reopened first;
	// otherwise the protection of completed chapters would be bypassed and the consistency between Progress and chapter facts broken.
	progress, err := t.store.Progress.Load()
	if err != nil {
		return nil, fmt.Errorf("check foundation phase: %w: %w", errs.ErrStoreRead, err)
	}
	if (a.Type == "outline" || a.Type == "layered_outline") && progress != nil {
		switch progress.Phase {
		case domain.PhaseWriting:
			return nil, fmt.Errorf(
				"Giai đoạn viết cấm dùng %s để ghi đè toàn bộ dàn ý. Hãy dùng revise_outline để sửa các chương chưa xảy ra, expand_arc để mở rộng cung phác thảo, hoặc append_volume để thêm tập mới: %w",
				a.Type, errs.ErrToolPrecondition)
		case domain.PhaseComplete:
			return nil, fmt.Errorf(
				"Cả truyện đã kết thúc, cấm dùng %s để ghi đè toàn bộ dàn ý. Hãy mở lại tác phẩm trước, rồi dùng thao tác sửa dàn ý hoặc viết tiếp có bảo vệ: %w",
				a.Type, errs.ErrToolPrecondition)
		}
	}
	if a.Scale != "" {
		if err := t.store.RunMeta.SetPlanningTier(domain.PlanningTier(a.Scale)); err != nil {
			return nil, fmt.Errorf("save planning tier: %w: %w", errs.ErrStoreWrite, err)
		}
	}

	// The end-of-volume three-way choice (continue volume / finale / complete) is the heaviest semantic judgment of the whole book, and its rationale must become an audit fact
	// (decisions.jsonl, the same stream as plan_start/intervention); otherwise a premature finale or
	// a misjudged continuation could only be debugged by digging through session logs. The fact snapshot takes the progress at the moment of the decision (before the change lands).
	volumeEnd := a.Type == "append_volume" || a.Type == "complete_book"
	if volumeEnd && strings.TrimSpace(a.Reason) == "" {
		return nil, fmt.Errorf("%s bắt buộc kèm tham số reason: đối chiếu danh sách kiểm tra kết thúc truyện, một câu nói rõ lần này vì sao viết tiếp tập, tuyên bố tập cuối hay kết thúc truyện: %w", a.Type, errs.ErrToolArgs)
	}
	var volumeEndFacts json.RawMessage
	if volumeEnd {
		p, err := t.store.Progress.Load()
		if err != nil {
			return nil, fmt.Errorf("load progress for volume-end facts: %w: %w", errs.ErrStoreRead, err)
		}
		if p != nil {
			facts := map[string]any{"completed_chapters": len(p.CompletedChapters)}
			if p.Layered {
				outline, outlineErr := t.store.Outline.LoadOutline()
				if outlineErr != nil {
					return nil, fmt.Errorf("load outlined chapters for volume-end facts: %w: %w", errs.ErrStoreRead, outlineErr)
				}
				facts["dynamic_planning"] = true
				facts["outlined_chapters"] = len(outline)
			} else {
				facts["total_chapters"] = p.TotalChapters
			}
			volumeEndFacts, err = json.Marshal(facts)
			if err != nil {
				return nil, fmt.Errorf("marshal volume-end facts: %w", err)
			}
		}
	}

	decode := func(typeName string, out any) error {
		return decodeFoundationJSON(typeName, content, out)
	}

	switch a.Type {
	case "premise":
		if err := t.store.Outline.SavePremise(content); err != nil {
			return nil, fmt.Errorf("save premise: %w: %w", errs.ErrStoreWrite, err)
		}
		if err := t.store.Progress.AdvancePhase(domain.PhasePremise); err != nil {
			return nil, fmt.Errorf("update premise phase: %w: %w", errs.ErrStoreWrite, err)
		}

	case "outline":
		var entries []domain.OutlineEntry
		if err := decode("outline", &entries); err != nil {
			return nil, err
		}
		if defect := domain.StalledOutline(entries); defect != "" {
			return nil, fmt.Errorf("%s: %w", defect, errs.ErrToolArgs)
		}
		if err := t.store.Outline.SaveOutline(entries); err != nil {
			return nil, fmt.Errorf("save outline: %w: %w", errs.ErrStoreWrite, err)
		}
		if err := t.store.Progress.AdvancePhase(domain.PhaseOutline); err != nil {
			return nil, fmt.Errorf("update outline phase: %w: %w", errs.ErrStoreWrite, err)
		}
		if err := t.store.Progress.SetTotalChapters(len(entries)); err != nil {
			return nil, fmt.Errorf("set total chapters: %w: %w", errs.ErrStoreWrite, err)
		}
		if domain.PlanningTier(a.Scale) != domain.PlanningTierLong {
			if err := t.store.Progress.SetLayered(false); err != nil {
				return nil, fmt.Errorf("disable layered mode: %w: %w", errs.ErrStoreWrite, err)
			}
			if err := t.store.Progress.UpdateVolumeArc(0, 0); err != nil {
				return nil, fmt.Errorf("reset volume/arc: %w: %w", errs.ErrStoreWrite, err)
			}
			if err := t.store.Outline.ClearLayeredOutline(); err != nil {
				return nil, fmt.Errorf("clear layered outline: %w: %w", errs.ErrStoreWrite, err)
			}
		}
		result["chapters"] = len(entries)

	case "layered_outline":
		var volumes []domain.VolumeOutline
		if err := decode("layered_outline", &volumes); err != nil {
			return nil, err
		}
		for vi := range volumes {
			for ai := range volumes[vi].Arcs {
				arc := &volumes[vi].Arcs[ai]
				if defect := domain.OversizedArc(
					fmt.Sprintf("Tập %d cung %d", volumes[vi].Index, arc.Index), arcPlannedSize(arc)); defect != "" {
					return nil, fmt.Errorf("%s: %w", defect, errs.ErrToolArgs)
				}
			}
		}
		if defect := domain.StalledOutline(domain.FlattenOutline(volumes)); defect != "" {
			return nil, fmt.Errorf("%s: %w", defect, errs.ErrToolArgs)
		}
		if err := t.store.Outline.SaveLayeredOutline(volumes); err != nil {
			return nil, fmt.Errorf("save layered_outline: %w: %w", errs.ErrStoreWrite, err)
		}
		total := domain.EstimatedChapterCapacity(volumes)
		if err := t.store.Progress.AdvancePhase(domain.PhaseOutline); err != nil {
			return nil, fmt.Errorf("update outline phase: %w: %w", errs.ErrStoreWrite, err)
		}
		if err := t.store.Progress.SetTotalChapters(total); err != nil {
			return nil, fmt.Errorf("set total chapters: %w: %w", errs.ErrStoreWrite, err)
		}
		if err := t.store.Progress.SetLayered(true); err != nil {
			return nil, fmt.Errorf("enable layered mode: %w: %w", errs.ErrStoreWrite, err)
		}
		if len(volumes) > 0 && len(volumes[0].Arcs) > 0 {
			if err := t.store.Progress.UpdateVolumeArc(volumes[0].Index, volumes[0].Arcs[0].Index); err != nil {
				return nil, fmt.Errorf("set initial volume/arc: %w: %w", errs.ErrStoreWrite, err)
			}
		}
		result["volumes"] = len(volumes)
		result["dynamic_planning"] = true
		result["outlined_chapters"] = len(domain.FlattenOutline(volumes))

	case "characters":
		var chars []domain.Character
		if err := decode("characters", &chars); err != nil {
			return nil, err
		}
		if err := t.store.Characters.Save(chars); err != nil {
			return nil, fmt.Errorf("save characters: %w: %w", errs.ErrStoreWrite, err)
		}
		result["count"] = len(chars)

	case "world_rules":
		var rules []domain.WorldRule
		if err := decode("world_rules", &rules); err != nil {
			return nil, err
		}
		if err := t.store.World.SaveWorldRules(rules); err != nil {
			return nil, fmt.Errorf("save world_rules: %w: %w", errs.ErrStoreWrite, err)
		}
		result["count"] = len(rules)

	case "expand_arc":
		if a.Volume <= 0 || a.Arc <= 0 {
			return nil, fmt.Errorf(
				"expand_arc cần volume và arc (số tập/số cung đều tính từ 1, nhận được volume=%d arc=%d); "+
					"hãy gọi novel_context trước, điền giá trị index của cung mục tiêu trong layered_outline: %w",
				a.Volume, a.Arc, errs.ErrToolArgs)
		}
		var expansion domain.ArcExpansion
		if err := decode("expand_arc", &expansion); err != nil {
			return nil, err
		}
		if defect := domain.OversizedArc(
			fmt.Sprintf("Tập %d cung %d", a.Volume, a.Arc), len(expansion.Chapters)); defect != "" {
			return nil, fmt.Errorf("%s: %w", defect, errs.ErrToolArgs)
		}
		if err := t.store.ExpandArc(a.Volume, a.Arc, expansion); err != nil {
			return nil, fmt.Errorf("expand arc: %w: %w", errs.ErrStoreWrite, err)
		}
		result["volume"] = a.Volume
		result["arc"] = a.Arc
		result["title"] = expansion.Title
		result["goal"] = expansion.Goal
		result["chapters"] = len(expansion.Chapters)
		if err := t.consumeWriterFeedback(); err != nil {
			return nil, err
		}

	case "append_volume":
		p, err := t.store.Progress.Load()
		if err != nil {
			return nil, fmt.Errorf("load progress: %w: %w", errs.ErrStoreRead, err)
		}
		if p != nil && p.Phase == domain.PhaseComplete {
			return nil, fmt.Errorf("Cả truyện đã kết thúc (phase=complete), không cho phép thêm tập mới: %w", errs.ErrToolPrecondition)
		}
		var vol domain.VolumeOutline
		if err := decode("append_volume", &vol); err != nil {
			return nil, err
		}
		for i := range vol.Arcs {
			arc := &vol.Arcs[i]
			if defect := domain.OversizedArc(
				fmt.Sprintf("Tập mới cung %d", arc.Index), arcPlannedSize(arc)); defect != "" {
				return nil, fmt.Errorf("%s: %w", defect, errs.ErrToolArgs)
			}
		}
		prior, err := t.store.Outline.LoadLayeredOutline()
		if err != nil {
			return nil, fmt.Errorf("load layered outline: %w: %w", errs.ErrStoreRead, err)
		}
		if err := t.store.AppendVolume(vol); err != nil {
			return nil, fmt.Errorf("append volume: %w: %w", errs.ErrStoreWrite, err)
		}
		result["volume"] = vol.Index
		if vol.Final {
			result["final_volume"] = true
		} else if domain.FinaleVolume(prior) > 0 {
			// Fact echo: the previously declared finale state is lifted by appending an ordinary new volume (the new volume becomes the last volume)
			result["finale_released"] = true
		}
		result["arcs"] = len(vol.Arcs)
		chCount := 0
		for _, arc := range vol.Arcs {
			chCount += len(arc.Chapters)
		}
		if chCount > 0 {
			result["chapters"] = chCount
		}
		if err := t.consumeWriterFeedback(); err != nil {
			return nil, err
		}

	case "complete_book":
		// The only entry point for completing the whole book: push Phase=Complete directly.
		// Allowed only in the Writing phase, to prevent a mistaken call during planning from skipping the entire writing.
		// Rejected when called with a rewrite queue pending, guaranteeing PendingRewrites finishes before the book can end.
		progress, perr := t.store.Progress.Load()
		if perr != nil {
			return nil, fmt.Errorf("load progress: %w: %w", errs.ErrStoreRead, perr)
		}
		if progress == nil {
			return nil, fmt.Errorf("progress chưa được khởi tạo: %w", errs.ErrToolPrecondition)
		}
		if progress.Phase != domain.PhaseWriting {
			return nil, fmt.Errorf("complete_book chỉ gọi được ở giai đoạn writing (hiện phase=%s): %w", progress.Phase, errs.ErrToolPrecondition)
		}
		if len(progress.PendingRewrites) > 0 {
			return nil, fmt.Errorf("Còn %d chương trong hàng đợi viết lại, xử lý xong rồi mới gọi complete_book: %w", len(progress.PendingRewrites), errs.ErrToolPrecondition)
		}
		// Enumerable pre-completion checks must live in the code layer (the three-way split), and cannot rely only on
		// the "completion checklist" in the prompt. Real incident: right after planning landed and phase flipped to writing, a weak model
		// mistakenly called complete_book, and 0/68 chapters were directly marked complete.
		if len(progress.CompletedChapters) == 0 {
			return nil, fmt.Errorf("Chưa viết chương nào thì không thể kết thúc truyện; sau khi lập kế hoạch xong hệ thống tự đẩy phần viết, không cần gọi complete_book: %w", errs.ErrToolPrecondition)
		}
		next := progress.NextChapter()
		if progress.Layered {
			outline, outlineErr := t.store.Outline.LoadOutline()
			if outlineErr != nil {
				return nil, fmt.Errorf("load outlined chapters: %w: %w", errs.ErrStoreRead, outlineErr)
			}
			if next <= len(outline) {
				return nil, fmt.Errorf("Dàn ý chi tiết hiện còn chương chưa viết (chương kế %d/hiện đã chi tiết hóa %d), không thể kết thúc truyện; muốn kết sớm hãy dùng append_volume với JSON tập có \"final\": true ở cấp cao nhất để tuyên bố tập cuối: %w", next, len(outline), errs.ErrToolPrecondition)
			}
			// The flat outline contains only expanded arcs, so skeleton arcs are completely invisible to the comparison above; without a separate guard,
			// a book could be declared complete even with a whole volume unexpanded.
			volumes, volErr := t.store.Outline.LoadLayeredOutline()
			if volErr != nil {
				return nil, fmt.Errorf("load layered outline: %w: %w", errs.ErrStoreRead, volErr)
			}
			if skeletons := domain.SkeletonArcs(volumes); len(skeletons) > 0 {
				return nil, fmt.Errorf("Còn %d cung phác thảo chưa mở rộng (như: %s), không thể kết thúc truyện; "+
					"hãy expand_arc mở rộng và viết xong trước, hoặc dùng append_volume kèm \"final\": true để tuyên bố tập cuối: %w",
					len(skeletons), skeletons[0], errs.ErrToolPrecondition)
			}
		} else if progress.TotalChapters > 0 && next <= progress.TotalChapters {
			return nil, fmt.Errorf("Dàn ý còn chương chưa viết (chương kế %d/tổng %d), không thể kết thúc truyện; muốn kết sớm hãy dùng append_volume với JSON tập có \"final\": true ở cấp cao nhất để tuyên bố tập cuối: %w", next, progress.TotalChapters, errs.ErrToolPrecondition)
		}
		// Active long-running threads that are not resolved block completion; the field contract of OpenThreads is "must be resolved before the story can end". This is not a
		// semantic re-judgment: if the model truly believes everything is resolved, it should first update_compass to clear open_threads and then complete the book, turning an
		// "exemption in the argument" into an auditable on-disk action (in practice, when continuing an imported finished book, the architect cited chapter and verse to bypass
		// item 3 of the completion checklist and completed the book directly, locking the user's continuation request behind the completion rule).
		compass, err := t.store.Outline.LoadCompass()
		if err != nil {
			return nil, fmt.Errorf("load compass: %w: %w", errs.ErrStoreRead, err)
		}
		if compass != nil && len(compass.OpenThreads) > 0 {
			return nil, fmt.Errorf("compass còn %d tuyến dài đang hoạt động chưa thu (như: %s), không thể kết thúc truyện. Xác nhận đã thu hết thì update_compass xóa sạch open_threads trước rồi gọi complete_book; vẫn cần mở rộng thì append_volume (có thể kèm \"final\": true để tuyên bố tập cuối): %w",
				len(compass.OpenThreads), compass.OpenThreads[0], errs.ErrToolPrecondition)
		}
		if err := t.store.Progress.MarkComplete(); err != nil {
			return nil, fmt.Errorf("mark complete: %w: %w", errs.ErrStoreWrite, err)
		}
		result["book_complete"] = true
		result["phase"] = string(domain.PhaseComplete)

	case "update_compass":
		var compass domain.StoryCompass
		if err := decode("compass", &compass); err != nil {
			return nil, err
		}
		// The tool layer forcibly overwrites LastUpdated with the current number of completed chapters, and does not trust LLM self-reporting.
		// The LLM usually forgets to fill it or leaves 0, which would make diag.CompassDrift raise false alarms and distort the Router's routing.
		p, err := t.store.Progress.Load()
		if err != nil {
			return nil, fmt.Errorf("load progress: %w: %w", errs.ErrStoreRead, err)
		}
		if p != nil {
			compass.LastUpdated = p.LatestCompleted()
		}
		if err := t.store.Outline.SaveCompass(compass); err != nil {
			return nil, fmt.Errorf("save compass: %w: %w", errs.ErrStoreWrite, err)
		}
		result["ending_direction"] = compass.EndingDirection
		result["last_updated"] = compass.LastUpdated
		if err := t.consumeWriterFeedback(); err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("unknown type %q, expected premise/outline/layered_outline/characters/world_rules/expand_arc/append_volume/update_compass/complete_book: %w", a.Type, errs.ErrToolArgs)
	}

	// checkpoint
	scope := domain.GlobalScope()
	if a.Type == "expand_arc" {
		scope = domain.ArcScope(a.Volume, a.Arc)
	} else if a.Type == "append_volume" {
		scope = domain.GlobalScope()
	}
	if _, err := t.store.Checkpoints.AppendArtifact(scope, a.Type, foundationArtifact(a.Type)); err != nil {
		return nil, fmt.Errorf("checkpoint foundation %s: %w: %w", a.Type, errs.ErrStoreWrite, err)
	}

	if volumeEnd {
		t.recordVolumeEndDecision(a.Type, a.Reason, volumeEndFacts, result)
	}

	// Return the remaining unfinished items. Even when the initial artifacts are complete, foundation_audit remains; entering writing is allowed only after
	// audit_foundation gives ready=true for the version actually on disk.
	remaining, err := t.store.FoundationMissing()
	if err != nil {
		return nil, fmt.Errorf("load foundation state: %w: %w", errs.ErrStoreRead, err)
	}
	ready := len(remaining) == 0
	result["remaining"] = remaining
	result["foundation_ready"] = ready
	return json.Marshal(result)
}

func foundationArtifact(t string) string {
	switch t {
	case "premise":
		return "premise.md"
	case "outline":
		return "outline.json"
	case "layered_outline", "expand_arc", "append_volume":
		return "layered_outline.json"
	case "complete_book":
		return "meta/progress.json"
	case "characters":
		return "characters.json"
	case "world_rules":
		return "world_rules.json"
	case "update_compass":
		return "meta/compass.json"
	default:
		return ""
	}
}

// decodeFoundationJSON parses the content field of save_foundation; on failure it attaches the line/column position
// and the most common fix hint, so the LLM's next retry can pinpoint the problem instead of guessing blindly.
func decodeFoundationJSON(typeName, content string, out any) error {
	err := json.Unmarshal([]byte(content), out)
	if err == nil {
		return nil
	}
	hint := `Nguyên nhân thường gặp: dấu nháy kép trong giá trị chuỗi chưa được escape thành \", xuống dòng chưa escape thành \n, hoặc thiếu dấu phẩy giữa các trường của đối tượng. Hãy sinh lại toàn bộ một lần.`
	if se, ok := err.(*json.SyntaxError); ok {
		line, col := offsetToLineCol(content, int(se.Offset))
		return fmt.Errorf("parse %s JSON (line %d col %d): %w — %s", typeName, line, col, err, hint)
	}
	return fmt.Errorf("parse %s JSON: %w — %s", typeName, err, hint)
}

func offsetToLineCol(s string, offset int) (int, int) {
	if offset < 0 {
		offset = 0
	}
	if offset > len(s) {
		offset = len(s)
	}
	line, col := 1, 1
	for i := 0; i < offset; i++ {
		if s[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}

func normalizeFoundationContent(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", fmt.Errorf("content is required: %w", errs.ErrToolArgs)
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}

	if !json.Valid(raw) {
		return "", fmt.Errorf("invalid content: expected Markdown string or valid JSON value: %w", errs.ErrToolArgs)
	}
	return string(raw), nil
}

// recordVolumeEndDecision writes the rationale of the end-of-volume three-way choice (continue volume / finale / complete) into the arbiter audit.
// Best-effort: the structural change is already on disk, so an audit failure only warns and does not roll back; reporting an error would make the model retry an
// already completed operation (appending the volume twice).
func (t *SaveFoundationTool) recordVolumeEndDecision(action, reason string, facts json.RawMessage, result map[string]any) {
	decision := map[string]any{"action": action}
	if v, ok := result["volume"]; ok {
		decision["volume"] = v
	}
	if _, ok := result["final_volume"]; ok {
		decision["final"] = true
	}
	raw, err := json.Marshal(decision)
	if err != nil {
		slog.Error("serializing volume-end verdict failed", "module", "tools", "action", action, "err", err)
		return
	}
	if _, err := t.store.Decisions.Append(store.DecisionRecord{
		Kind:     "volume_end",
		Decider:  "architect",
		Facts:    facts,
		Decision: raw,
		Reason:   reason,
	}); err != nil {
		slog.Error("saving volume-end verdict audit failed", "module", "tools", "action", action, "err", err)
	}
}

// consumeWriterFeedback clears the processed planning feedback after a structural operation succeeds.
func (t *SaveFoundationTool) consumeWriterFeedback() error {
	if err := t.store.Outline.ClearOutlineFeedback(); err != nil {
		return fmt.Errorf("clear outline feedback: %w: %w", errs.ErrStoreWrite, err)
	}
	return nil
}

// arcPlannedSize gets the actual size of an arc: the detailed chapter count if expanded, or the estimated chapter count if it is still a skeleton.
func arcPlannedSize(arc *domain.ArcOutline) int {
	if n := len(arc.Chapters); n > 0 {
		return n
	}
	return arc.EstimatedChapters
}
