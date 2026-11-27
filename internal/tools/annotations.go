package tools

import "github.com/modelcontextprotocol/go-sdk/mcp"

// readOnly is the annotation set every arc-mcp tool carries. Clients use
// readOnlyHint to run a tool without asking the user each time; arc-mcp only
// ever reads, so every tool says so, and a new tool built with this helper
// cannot forget. idempotentHint: calling a tool again changes nothing in Arc
// (the answer may differ only because new data arrived). openWorldHint is
// false: the tools reach one Arc instance, not the open internet.
func readOnly(title string) *mcp.ToolAnnotations {
	f := false
	return &mcp.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    true,
		DestructiveHint: &f,
		IdempotentHint:  true,
		OpenWorldHint:   &f,
	}
}
