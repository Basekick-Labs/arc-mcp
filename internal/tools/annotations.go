package tools

import (
	"github.com/basekick-labs/arc-mcp/internal/arc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// readOnly is the annotation set every arc-mcp tool carries. Clients use
// readOnlyHint to run a tool without asking the user each time; arc-mcp only
// ever reads, so every tool says so. idempotentHint: calling a tool again changes nothing in Arc
// (the answer may differ only because new data arrived). openWorldHint is
// false: the tools reach one Arc instance, not the open internet. Register
// tools through RegisterAll, so the annotation test covers every one.
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

// RegisterAll registers the tools of every group in groups, in a fixed order:
// the data tools, the operations tools, then arc_enterprise. main and the tests both use it,
// so a test over its tools covers every tool the server exposes.
func RegisterAll(server *mcp.Server, client *arc.Client, groups Groups, maxRows, maxResponseChars int) {
	if groups.Data {
		RegisterListDatabases(server, client)
		RegisterListMeasurements(server, client)
		RegisterDescribeMeasurement(server, client, maxResponseChars)
		RegisterQuery(server, client, maxRows, maxResponseChars)
		RegisterGetSampleData(server, client, maxResponseChars)
	}
	if groups.Ops {
		RegisterHealth(server, client, maxResponseChars)
		RegisterMetrics(server, client, maxResponseChars)
	}
	if groups.Enterprise {
		RegisterEnterprise(server, client, maxResponseChars)
	}
}
