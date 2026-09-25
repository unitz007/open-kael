package domain

type SchemaType string

const (
	SchemaTypeString  SchemaType = "string"
	SchemaTypeNumber  SchemaType = "number"
	SchemaTypeInteger SchemaType = "integer"
	SchemaTypeBoolean SchemaType = "boolean"
	SchemaTypeArray   SchemaType = "array"
	SchemaTypeObject  SchemaType = "object"
)

// Schema is a minimal, recursive JSON-Schema-shaped description — enough to
// declare a Tool's or Skill's input/output contract and persist it, without
// pulling in a full JSON-Schema library. Deliberately not the same as
// tools.ToolRequestParameters (kael-platform/tools/tool.go), which is a flat
// per-parameter list on a live ToolSpec — Schema describes one value
// (possibly nested/object-shaped), matching what MCP/OpenAI tool schemas
// actually look like on the wire.
type Schema struct {
	Type        SchemaType        `json:"type"`
	Description string            `json:"description,omitempty"`
	Properties  map[string]Schema `json:"properties,omitempty"`
	Items       *Schema           `json:"items,omitempty"`
	Required    []string          `json:"required,omitempty"`
	Enum        []string          `json:"enum,omitempty"`
}
