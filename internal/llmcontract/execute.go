package llmcontract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/llmretry"
)

// FailureKind 区分不可由同一次结构化反馈修复的失败边界。
type FailureKind string

const (
	FailureRequest  FailureKind = "request"
	FailureProtocol FailureKind = "protocol"
	FailureLength   FailureKind = "length"
	FailureSafety   FailureKind = "safety"
	FailureContract FailureKind = "contract"
)

// Failure 保留失败类别和模型原始输出，供调用方决定日志、工件和 UI 表达。
type Failure struct {
	Kind     FailureKind
	Contract string
	Raw      string
	Err      error
}

func (e *Failure) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err == nil {
		return e.Contract
	}
	if e.Contract == "" {
		return e.Err.Error()
	}
	return fmt.Sprintf("%s: %v", e.Contract, e.Err)
}

func (e *Failure) Unwrap() error { return e.Err }

// Correction 描述一次模型可修复的输出错误。Attempt 是刚失败的调用序号。
type Correction struct {
	Attempt int
	Layer   string
	Mode    Mode
	Raw     string
	Err     error
}

// Hooks 只负责可观测性，不改变执行语义。
type Hooks struct {
	Resolved     func(Resolution)
	RequestRetry func(llmretry.Event)
	Correction   func(Correction)
}

// Request 定义一次直接结构化返回。Contract 是结构的单一来源，Validate 只处理
// JSON Schema 无法表达的业务约束。
type Request[T any] struct {
	Contract     Contract
	SystemPrompt string
	Payload      string
	Options      []agentcore.CallOption
	Validate     func(*T) error
	Agent        string
	Hooks        Hooks
}

const promptCorrection = "Đầu ra ở trên không khớp JSON Schema. Hãy sửa theo lỗi và chỉ xuất đối tượng JSON hoàn chỉnh, không giải thích hay rào Markdown."
const semanticCorrection = "JSON ở trên hợp lệ về cấu trúc nhưng giá trị trường không qua kiểm tra nghiệp vụ. Hãy sửa theo lỗi và xuất lại đối tượng JSON hoàn chỉnh."

// Execute 统一完成协议选择、提示词准备、请求重试、停止原因分类、Schema/DTO
// 解码和业务反馈自愈。prompt 模式的格式/Schema 错误以及两种模式的业务错误会
// 持续反馈给模型，直到成功或 context 结束；原生契约违约会立即暴露。
func Execute[T any](ctx context.Context, model llmretry.Generator, req Request[T]) (T, error) {
	var zero T
	if model == nil {
		return zero, &Failure{Kind: FailureProtocol, Contract: req.Contract.Name, Err: errors.New("Chưa cấu hình model")}
	}

	schemaOptions, resolution := Plan(model, req.Contract)
	systemPrompt, err := PreparePrompt(req.SystemPrompt, req.Contract, resolution)
	if err != nil {
		return zero, &Failure{Kind: FailureContract, Contract: req.Contract.Name, Err: fmt.Errorf("Chuẩn bị hợp đồng đầu ra: %w", err)}
	}
	if req.Hooks.Resolved != nil {
		req.Hooks.Resolved(resolution)
	}

	messages := []agentcore.Message{
		agentcore.SystemMsg(systemPrompt),
		agentcore.UserMsg(req.Payload),
	}
	options := append(schemaOptions, req.Options...)
	native := resolution.Mode == ModeNativeJSONSchema

	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		resp, err := llmretry.Generate(ctx, model, llmretry.Config{
			Agent:   req.Agent,
			OnRetry: req.Hooks.RequestRetry,
		}, messages, options...)
		if err != nil {
			if ctx.Err() != nil {
				return zero, ctx.Err()
			}
			return zero, &Failure{Kind: FailureRequest, Contract: req.Contract.Name, Err: err}
		}
		if resp == nil {
			return zero, &Failure{Kind: FailureProtocol, Contract: req.Contract.Name, Err: errors.New("Model trả về phản hồi rỗng")}
		}

		raw := resp.Message.TextContent()
		switch resp.Message.StopReason {
		case agentcore.StopReasonLength:
			return zero, &Failure{Kind: FailureLength, Contract: req.Contract.Name, Raw: raw, Err: errors.New("Đầu ra của model bị cắt do giới hạn độ dài (stop_reason=length)")}
		case agentcore.StopReasonSafety:
			return zero, &Failure{Kind: FailureSafety, Contract: req.Contract.Name, Raw: raw, Err: errors.New("Model từ chối trả lời hoặc kích hoạt bộ lọc nội dung (stop_reason=safety)")}
		case agentcore.StopReasonError:
			return zero, &Failure{Kind: FailureProtocol, Contract: req.Contract.Name, Raw: raw, Err: errors.New("Model kết thúc ở trạng thái lỗi (stop_reason=error)")}
		case agentcore.StopReasonToolUse:
			return zero, &Failure{Kind: FailureProtocol, Contract: req.Contract.Name, Raw: raw, Err: errors.New("Lệnh gọi có cấu trúc bất ngờ trả về lệnh gọi công cụ (stop_reason=tool_use)")}
		case agentcore.StopReasonAborted:
			return zero, &Failure{Kind: FailureProtocol, Contract: req.Contract.Name, Raw: raw, Err: errors.New("Lệnh gọi model bị hủy giữa chừng (stop_reason=aborted)")}
		}

		body := strings.TrimSpace(raw)
		if native {
			if body == "" {
				return zero, &Failure{Kind: FailureContract, Contract: req.Contract.Name, Raw: raw, Err: errors.New("Native schema trả về nội dung rỗng")}
			}
		} else {
			body = ExtractJSONObject(raw)
		}

		layer := "schema"
		var cause error
		if body == "" {
			layer, cause = "decode", errors.New("Không tìm thấy đối tượng JSON trong đầu ra")
		} else if err := ValidateJSON(req.Contract.Schema, []byte(body)); err != nil {
			cause = err
		} else {
			var out T
			if err := json.Unmarshal([]byte(body), &out); err != nil {
				// Schema 已通过而 DTO 无法解码，说明静态契约与 Go 类型不一致，
				// 继续要求模型重写无法修复代码缺陷。
				return zero, &Failure{Kind: FailureContract, Contract: req.Contract.Name, Raw: raw, Err: fmt.Errorf("schema và DTO không khớp nhau: %w", err)}
			}
			if req.Validate == nil {
				return out, nil
			}
			if err := req.Validate(&out); err == nil {
				return out, nil
			} else {
				layer, cause = "semantic", err
			}
		}

		if native && layer != "semantic" {
			return zero, &Failure{Kind: FailureContract, Contract: req.Contract.Name, Raw: raw, Err: fmt.Errorf("Native schema vi phạm hợp đồng: %w", cause)}
		}
		correction := Correction{Attempt: attempt, Layer: layer, Mode: resolution.Mode, Raw: raw, Err: cause}
		if req.Hooks.Correction != nil {
			req.Hooks.Correction(correction)
		}
		hint := promptCorrection
		if layer == "semantic" {
			hint = semanticCorrection
		}
		messages = append(messages,
			agentcore.Message{Role: agentcore.RoleAssistant, Content: []agentcore.ContentBlock{agentcore.TextBlock(raw)}},
			agentcore.UserMsg(hint+"\nLỗi: "+cause.Error()),
		)
	}
}

// ExtractJSONObject 返回文本中的第一个平衡 JSON 对象，字符串中的花括号不计入层级。
func ExtractJSONObject(raw string) string {
	start := strings.IndexByte(raw, '{')
	if start < 0 {
		return ""
	}
	depth, inString, escaped := 0, false, false
	for i := start; i < len(raw); i++ {
		switch c := raw[i]; {
		case inString && escaped:
			escaped = false
		case inString && c == '\\':
			escaped = true
		case c == '"':
			inString = !inString
		case !inString && c == '{':
			depth++
		case !inString && c == '}':
			depth--
			if depth == 0 {
				return raw[start : i+1]
			}
		}
	}
	return ""
}
