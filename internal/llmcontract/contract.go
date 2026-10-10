// Package llmcontract is the unified contract and execution layer for direct structured returns: the static Contract
// is the single source of structure, and Execute handles capability selection, prompt preparation, request retry,
// Schema/DTO decoding and feedback self-healing in one place.
// The protocol is decided before the request is sent; when a native request is rejected or violated, it is surfaced as-is, and silently dropping the schema and resending is forbidden.
package llmcontract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/voocel/agentcore"
	"github.com/voocel/agentcore/llm"
)

// Contract is the static contract of one direct structured return, placed next to each boundary DTO definition.
type Contract struct {
	Name        string
	Description string
	Schema      map[string]any
}

// Mode is the structured protocol adopted for this call.
type Mode string

const (
	ModeNativeJSONSchema Mode = "native_json_schema"
	ModePromptContract   Mode = "prompt_contract"
)

// Source is where the capability judgment comes from.
type Source string

const (
	SourceConfig  Source = "config"  // explicitly declared by the user in ModelConfig.json_schema
	SourceAdapter Source = "adapter" // the model-level capability table of the provider adapter
	SourceUnknown Source = "unknown" // no declaration and capability unknown, conservatively use the prompt contract
)

// Resolution is the protocol selection result decided before the request is sent, for caller branching and logging.
type Resolution struct {
	Mode     Mode
	Source   Source
	Strict   bool // whether strict is carried when native
	Provider string
	Model    string
}

// jsonSchemaOverrider is implemented by model wrappers that carry the three-state config override
// (bootstrap.SwappableModel and the wrapper layers that pass through it).
type jsonSchemaOverrider interface {
	JSONSchemaOverride() *bool
}

type modelInfoProvider interface {
	Info() llm.ModelInfo
}

// ModelFacts is a same-instant snapshot needed for one capability resolution. A hot-swap wrapper implements this interface
// so that Resolve never mixes in state from between two swaps when reading capability, config override and model identity separately.
type ModelFacts struct {
	Capabilities       llm.Capabilities
	Info               llm.ModelInfo
	JSONSchemaOverride *bool
}

type modelFactsProvider interface {
	StructuredOutputFacts() ModelFacts
}

// Resolve reads the current model facts fresh on every call (the next call after a hot swap uses the new values):
// the three-state config takes priority, then the adapter's model-level capability, and anything unknown falls back to the prompt contract.
func Resolve(model any) Resolution {
	res := Resolution{Mode: ModePromptContract, Source: SourceUnknown}

	var caps llm.Capabilities
	var info llm.ModelInfo
	var override *bool
	if fp, ok := model.(modelFactsProvider); ok {
		facts := fp.StructuredOutputFacts()
		caps, info, override = facts.Capabilities, facts.Info, facts.JSONSchemaOverride
	} else {
		if cp, ok := model.(llm.CapabilityProvider); ok {
			caps = cp.Capabilities()
		}
		if ip, ok := model.(modelInfoProvider); ok {
			info = ip.Info()
		}
		if o, ok := model.(jsonSchemaOverrider); ok {
			override = o.JSONSchemaOverride()
		}
	}
	res.Provider, res.Model = caps.Provider, caps.Model
	if res.Provider == "" {
		res.Provider = info.Provider
	}
	if res.Model == "" {
		res.Model = info.Name
	}

	if override != nil {
		res.Source = SourceConfig
		if *override {
			res.Mode = ModeNativeJSONSchema
			// If the user declares that the endpoint follows the Structured Outputs contract, strict is the default;
			// only when the adapter explicitly says strict is unsupported is the schema sent without strict.
			res.Strict = caps.Structured.Strict != llm.SupportNo
		}
		return res
	}

	switch caps.Structured.JSONSchema {
	case llm.SupportYes:
		res.Mode = ModeNativeJSONSchema
		res.Source = SourceAdapter
		res.Strict = caps.Structured.Strict == llm.SupportYes
	case llm.SupportNo:
		res.Source = SourceAdapter
	}
	return res
}

// Plan resolves the protocol and, in native mode, builds the call options; in prompt contract mode it returns nil opts.
func Plan(model any, c Contract) ([]agentcore.CallOption, Resolution) {
	res := Resolve(model)
	if res.Mode != ModeNativeJSONSchema {
		return nil, res
	}
	return []agentcore.CallOption{
		agentcore.WithJSONSchema(c.Name, c.Description, c.Schema, res.Strict),
	}, res
}

// PreparePrompt keeps exactly one copy of the business-semantic prompt: in native mode it returns the original text as-is; in prompt
// contract mode it auto-generates the format suffix from the same Schema. Callers do not maintain a second template, and field
// changes cannot make the prompt diverge from response_format.
func PreparePrompt(base string, c Contract, res Resolution) (string, error) {
	if res.Mode != ModePromptContract {
		return base, nil
	}
	schemaJSON, err := json.Marshal(c.Schema)
	if err != nil {
		return "", fmt.Errorf("llmcontract: marshal %s prompt schema: %w", c.Name, err)
	}
	contract := "## Hợp đồng đầu ra\n\n" +
		"Chỉ xuất ra một đối tượng JSON phù hợp với JSON Schema dưới đây, không xuất giải thích, rào Markdown hay chính nhãn tiêu đề.\n\n" +
		"<output-json-schema>\n" + string(schemaJSON) + "\n</output-json-schema>"
	if strings.TrimSpace(base) == "" {
		return contract, nil
	}
	return strings.TrimSpace(base) + "\n\n" + contract, nil
}

// Nullable extends a schema's type to a nullable union (["<t>","null"]), used to express
// "all fields required, optional semantics via null" in strict mode. It returns a copy and does not modify the input map.
func Nullable(s map[string]any) map[string]any {
	out := maps.Clone(s)
	if t, ok := out["type"].(string); ok {
		out["type"] = []string{t, "null"}
	}
	switch values := out["enum"].(type) {
	case []string:
		enum := make([]any, 0, len(values)+1)
		for _, value := range values {
			enum = append(enum, value)
		}
		out["enum"] = append(enum, nil)
	case []any:
		enum := slices.Clone(values)
		for _, value := range enum {
			if value == nil {
				return out
			}
		}
		out["enum"] = append(enum, nil)
	}
	return out
}

// ValidateStrictReady recursively checks that a schema meets the structural precondition of the OpenAI strict subset:
// the properties of every object must be listed in required (optional semantics expressed as a null union). litellm
// performs the same check at request time and automatically adds additionalProperties:false; contract tests use this function
// as an up-front assertion (RFC §11.1), so structural problems are not left to runtime.
func ValidateStrictReady(s map[string]any) error {
	return validateStrictReady(s, "$")
}

func validateStrictReady(s map[string]any, path string) error {
	if typeIncludes(s["type"], "object") {
		props, _ := s["properties"].(map[string]any)
		required, _ := s["required"].([]string)
		for name, sub := range props {
			if !slices.Contains(required, name) {
				return fmt.Errorf("%s.%s chưa được liệt kê trong required (strict yêu cầu mọi thuộc tính đều required)", path, name)
			}
			if subMap, ok := sub.(map[string]any); ok {
				if err := validateStrictReady(subMap, path+"."+name); err != nil {
					return err
				}
			}
		}
	}
	if items, ok := s["items"].(map[string]any); ok {
		return validateStrictReady(items, path+"[]")
	}
	return nil
}

func typeIncludes(t any, want string) bool {
	switch v := t.(type) {
	case string:
		return v == want
	case []string:
		return slices.Contains(v, want)
	}
	return false
}

// Fingerprint returns the first 12 hex characters of the sha256 of the schema's canonical JSON, for log correlation;
// encoding/json sorts map keys, so the same contract is naturally stable.
func (c Contract) Fingerprint() string {
	data, err := json.Marshal(c.Schema)
	if err != nil {
		return "unmarshalable"
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:12]
}
