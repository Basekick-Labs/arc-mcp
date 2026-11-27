// Package tools implements the MCP tool handlers exposed by arc-mcp.
package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/basekick-labs/arc-mcp/internal/arc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ListDatabasesArgs is the input schema for list_databases (no args needed).
type ListDatabasesArgs struct{}

// RegisterListDatabases registers the list_databases tool.
func RegisterListDatabases(server *mcp.Server, client *arc.Client) {
	mcp.AddTool(server, &mcp.Tool{
		Name:         "list_databases",
		OutputSchema: databasesOutputSchema,
		Title:        "List databases",
		Annotations:  readOnly("List databases"),
		Description:  "List all databases in the Arc instance. Returns database names and measurement counts.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ ListDatabasesArgs) (out *mcp.CallToolResult, _ any, _ error) {
		defer RecoverToolPanic(&out)

		dbResult, err := client.ListDatabases(ctx)
		if arc.Scoped(err) {
			// Arc refuses this route for a token restricted to specific
			// databases rather than returning a filtered list, the same
			// way SHOW DATABASES does. That is the expected answer, not a
			// failure: report it as a result the model can act on, so it
			// asks the user which database to use instead of retrying a
			// call that will be refused identically.
			return ScopedResult("", "list_measurements"), nil, nil
		}
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: client.UserMessage(err)},
				},
				IsError: true,
			}, nil, nil
		}

		structured := DatabasesOutput{Databases: []DatabaseInfo{}, Count: len(dbResult.Databases)}
		for _, db := range dbResult.Databases {
			structured.Databases = append(structured.Databases, DatabaseInfo{Name: db.Name, MeasurementCount: db.MeasurementCount})
		}
		if len(dbResult.Databases) == 0 {
			return withStructured(&mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: "No databases found."},
				},
			}, structured), nil, nil
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "Found %d database(s):\n\n", dbResult.Count)
		for _, db := range dbResult.Databases {
			fmt.Fprintf(&sb, "- **%s** (%d measurements)\n", db.Name, db.MeasurementCount)
		}
		return withStructured(&mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: sb.String()},
			},
		}, structured), nil, nil
	})
}
