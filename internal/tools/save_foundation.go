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

// SaveFoundationTool 保存基础设定（premise/outline/characters），Architect 专用。
type SaveFoundationTool struct {
	store *store.Store
}

func NewSaveFoundationTool(store *store.Store) *SaveFoundationTool {
	return &SaveFoundationTool{store: store}
}

func (t *SaveFoundationTool) Name() string { return "save_foundation" }
func (t *SaveFoundationTool) Description() string {
	return "Lưu thiết lập nền tảng của tiểu thuyết (premise/outline/characters/world_rules/compass, v.v.). **Đây là cổng lưu bền duy nhất**: nội dung không lưu qua công cụ này sẽ không vào store, chỉ xuất Markdown/JSON trong tin nhắn là coi như mất. Tham số cố định là {type, content, scale?, volume?, arc?}. type chọn một trong premise / outline / layered_outline / characters / world_rules / expand_arc / append_volume / update_compass / complete_book (không có type=compass; cập nhật compass dùng update_compass). Với premise thì content phải là chuỗi Markdown; các type khác ưu tiên truyền trực tiếp mảng hoặc đối tượng JSON. expand_arc hiệu chỉnh và mở rộng một cung khung xương chưa viết (một cung không được quá 8 chương chi tiết — thẩm duyệt cuối cung phải đọc trọn cả cung, quá dài thì không thẩm duyệt nổi; vượt giới hạn thì tách thành nhiều cung; cần volume + arc, content là {title, goal, chapters}, có thể sửa mục tiêu khung xương ban đầu dựa trên chính văn đã hoàn thành); append_volume thêm tập mới (content là VolumeOutline JSON đầy đủ, gồm cấu trúc cung; cấp cao nhất có \"final\": true là tuyên bố tập khép lại — cả truyện kết thúc ở tập này, viết xong mọi chương sẽ tự hoàn kết, không cần gọi complete_book); update_compass cập nhật hướng kết cục (content là StoryCompass JSON, chỉ có các trường {ending_direction: string bắt buộc, open_threads?: string[], estimated_scale?: string}, mọi trường khác đều không nhận); complete_book tuyên bố hoàn kết cả truyện (content truyền đối tượng rỗng {}, đẩy thẳng Phase=Complete; công cụ sẽ kiểm tra: các chương trong đại cương đã viết hết, không có hàng đợi viết lại, compass không còn open_threads chưa thu — xác nhận tuyến dài đã thu thì phải update_compass xóa sạch open_threads và lưu trước; muốn kết sớm thì dùng append_volume với tập khép lại final). append_volume / complete_book bắt buộc kèm tham số reason (lý do phán định một câu, đối chiếu danh sách kiểm tra hoàn kết, ghi vào kiểm toán tài phán). scale tùy chọn, chỉ cho phép short / mid / long."
}
func (t *SaveFoundationTool) Label() string { return "Lưu thiết lập" }

// 写工具（跨域更新 Outline/Progress/Characters），禁止并发。
func (t *SaveFoundationTool) ReadOnly(_ json.RawMessage) bool        { return false }
func (t *SaveFoundationTool) ConcurrencySafe(_ json.RawMessage) bool { return false }

func (t *SaveFoundationTool) Schema() map[string]any {
	return schema.Object(
		schema.Property("type", schema.Enum("Loại thiết lập", "premise", "outline", "layered_outline", "characters", "world_rules", "expand_arc", "append_volume", "update_compass", "complete_book")).Required(),
		schema.Property("content", map[string]any{
			"description": "Nội dung. premise truyền chuỗi Markdown; các type khác truyền thẳng mảng hoặc đối tượng JSON, cũng chấp nhận chuỗi JSON. Với expand_arc truyền {title, goal, chapters}, title/goal là quy hoạch cung mục tiêu đã hiệu chỉnh theo các sự thật đã hoàn thành.",
		}).Required(),
		schema.Property("scale", schema.Enum("Cấp quy hoạch", "short", "mid", "long")),
		schema.Property("volume", schema.Int("Số thứ tự tập mục tiêu, tính từ 1 (chỉ bắt buộc khi expand_arc)")),
		schema.Property("arc", schema.Int("Số thứ tự cung mục tiêu trong tập, tính từ 1 (chỉ bắt buộc khi expand_arc)")),
		schema.Property("reason", schema.String("Lý do phán định cuối tập (bắt buộc khi append_volume / complete_book): đối chiếu danh sách kiểm tra hoàn kết, một câu nói rõ vì sao viết tiếp tập, tuyên bố tập khép lại hay hoàn kết")),
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

	// 全量大纲只属于规划期。写作期必须用受保护的增量操作，完结后必须先重开；
	// 否则会绕过已完成章节保护，破坏 Progress 与章节事实的一致性。
	progress, err := t.store.Progress.Load()
	if err != nil {
		return nil, fmt.Errorf("check foundation phase: %w: %w", errs.ErrStoreRead, err)
	}
	if (a.Type == "outline" || a.Type == "layered_outline") && progress != nil {
		switch progress.Phase {
		case domain.PhaseWriting:
			return nil, fmt.Errorf(
				"Giai đoạn viết cấm dùng %s để ghi đè toàn bộ đại cương. Hãy dùng revise_outline để sửa các chương chưa xảy ra, expand_arc để mở rộng cung khung xương, hoặc append_volume để thêm tập mới: %w",
				a.Type, errs.ErrToolPrecondition)
		case domain.PhaseComplete:
			return nil, fmt.Errorf(
				"Cả truyện đã hoàn kết, cấm dùng %s để ghi đè toàn bộ đại cương. Hãy mở lại tác phẩm trước, rồi dùng thao tác sửa đại cương hoặc viết tiếp có bảo vệ: %w",
				a.Type, errs.ErrToolPrecondition)
		}
	}
	if a.Scale != "" {
		if err := t.store.RunMeta.SetPlanningTier(domain.PlanningTier(a.Scale)); err != nil {
			return nil, fmt.Errorf("save planning tier: %w: %w", errs.ErrStoreWrite, err)
		}
	}

	// 卷末三选一（续卷/收官/完结）是全书最重的语义判断，理由必须成为审计事实
	// （decisions.jsonl，与 plan_start/intervention 同一条流水），否则收官过早/
	// 续卷失当只能翻会话日志排障。事实快照取判定时刻（变更落盘前）的进度。
	volumeEnd := a.Type == "append_volume" || a.Type == "complete_book"
	if volumeEnd && strings.TrimSpace(a.Reason) == "" {
		return nil, fmt.Errorf("%s bắt buộc kèm tham số reason: đối chiếu danh sách kiểm tra hoàn kết, một câu nói rõ lần này vì sao viết tiếp tập, tuyên bố tập khép lại hay hoàn kết: %w", a.Type, errs.ErrToolArgs)
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
			return nil, fmt.Errorf("Cả truyện đã hoàn kết (phase=complete), không cho phép thêm tập mới: %w", errs.ErrToolPrecondition)
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
			// 事实回显：此前宣告的收官态因追加普通新卷而解除（新卷成为末卷）
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
		// 全书完结的唯一入口：直接推 Phase=Complete。
		// 仅 Writing 阶段允许，防止规划阶段误调跳过整本写作。
		// 拒绝有返工队列时调用——保证 PendingRewrites 跑完才能结束。
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
		// 可枚举的完本前置校验必须在代码层(三分法),不能只依赖提示词里的
		// "完结判定清单"——真实事故:规划刚落盘 phase 翻到 writing,弱模型顺手
		// 误调 complete_book,0/68 章被直接标记完本。
		if len(progress.CompletedChapters) == 0 {
			return nil, fmt.Errorf("Chưa viết chương nào thì không thể hoàn kết; sau khi quy hoạch xong hệ thống tự đẩy phần viết, không cần gọi complete_book: %w", errs.ErrToolPrecondition)
		}
		next := progress.NextChapter()
		if progress.Layered {
			outline, outlineErr := t.store.Outline.LoadOutline()
			if outlineErr != nil {
				return nil, fmt.Errorf("load outlined chapters: %w: %w", errs.ErrStoreRead, outlineErr)
			}
			if next <= len(outline) {
				return nil, fmt.Errorf("Đại cương chi tiết hiện còn chương chưa viết (chương kế %d/hiện đã chi tiết hóa %d), không thể hoàn kết; muốn kết sớm hãy dùng append_volume với JSON tập có \"final\": true ở cấp cao nhất để tuyên bố tập khép lại: %w", next, len(outline), errs.ErrToolPrecondition)
			}
			// 扁平大纲只含已展开的弧，骨架弧对上面的比对完全隐形；不单独拦一次，
			// 整卷未展开也能宣告完本。
			volumes, volErr := t.store.Outline.LoadLayeredOutline()
			if volErr != nil {
				return nil, fmt.Errorf("load layered outline: %w: %w", errs.ErrStoreRead, volErr)
			}
			if skeletons := domain.SkeletonArcs(volumes); len(skeletons) > 0 {
				return nil, fmt.Errorf("Còn %d cung khung xương chưa mở rộng (như: %s), không thể hoàn kết; "+
					"hãy expand_arc mở rộng và viết xong trước, hoặc dùng append_volume kèm \"final\": true để tuyên bố tập khép lại: %w",
					len(skeletons), skeletons[0], errs.ErrToolPrecondition)
			}
		} else if progress.TotalChapters > 0 && next <= progress.TotalChapters {
			return nil, fmt.Errorf("Đại cương còn chương chưa viết (chương kế %d/tổng %d), không thể hoàn kết; muốn kết sớm hãy dùng append_volume với JSON tập có \"final\": true ở cấp cao nhất để tuyên bố tập khép lại: %w", next, progress.TotalChapters, errs.ErrToolPrecondition)
		}
		// 活跃长线未收束不可完本——OpenThreads 的字段契约即"需收束才能结局"。这不是
		// 语义复判：真认为已全部收束，先 update_compass 清空 open_threads 再完本，把
		// "论述里豁免"变成可审计的落盘动作（实测导入完本书续写时，架构师引经据典绕过
		// 完结清单第 3 条直接完本，用户的续写诉求被完本规则锁死）。
		compass, err := t.store.Outline.LoadCompass()
		if err != nil {
			return nil, fmt.Errorf("load compass: %w: %w", errs.ErrStoreRead, err)
		}
		if compass != nil && len(compass.OpenThreads) > 0 {
			return nil, fmt.Errorf("compass còn %d tuyến dài đang hoạt động chưa thu (như: %s), không thể hoàn kết. Xác nhận đã thu hết thì update_compass xóa sạch open_threads trước rồi gọi complete_book; vẫn cần mở rộng thì append_volume (có thể kèm \"final\": true để tuyên bố tập khép lại): %w",
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
		// 工具层强制覆盖 LastUpdated 为当前已完成章节数，不信任 LLM 自填。
		// LLM 通常忘填或留 0，会让 diag.CompassDrift 误报、Router 路由失真。
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

	// 返回剩余未完成项。初始工件齐全后仍会剩 foundation_audit；只有
	// audit_foundation 对实际落盘版本给出 ready=true，才允许进入 writing。
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

// decodeFoundationJSON 解析 save_foundation 的 content 字段，失败时附上行列位置
// 和最常见的修复提示，让 LLM 下一次重试能直接定位而不是盲猜。
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

// recordVolumeEndDecision 把卷末三选一（续卷/收官/完结）的判定理由落进裁定审计。
// best-effort：结构变更已落盘，审计失败只告警不回滚——报错会让模型重试已完成
// 的操作（重复追加卷）。
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
		slog.Error("Tuần tự hóa phán định cuối tập thất bại", "module", "tools", "action", action, "err", err)
		return
	}
	if _, err := t.store.Decisions.Append(store.DecisionRecord{
		Kind:     "volume_end",
		Decider:  "architect",
		Facts:    facts,
		Decision: raw,
		Reason:   reason,
	}); err != nil {
		slog.Error("Lưu kiểm toán phán định cuối tập thất bại", "module", "tools", "action", action, "err", err)
	}
}

// consumeWriterFeedback 在结构操作成功后清除已处理的规划反馈。
func (t *SaveFoundationTool) consumeWriterFeedback() error {
	if err := t.store.Outline.ClearOutlineFeedback(); err != nil {
		return fmt.Errorf("clear outline feedback: %w: %w", errs.ErrStoreWrite, err)
	}
	return nil
}

// arcPlannedSize 取弧的实际规模：已展开用详细章节数，仍是骨架则用预估章数。
func arcPlannedSize(arc *domain.ArcOutline) int {
	if n := len(arc.Chapters); n > 0 {
		return n
	}
	return arc.EstimatedChapters
}
