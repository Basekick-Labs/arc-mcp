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
		Name:        "list_databases",
		Description: "List all databases in the Arc instance. Returns database names and measurement counts.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ ListDatabasesArgs) (out *mcp.CallToolResult, _ any, _ error) {
		defer RecoverToolPanic(&out)

		dbResult, err := client.ListDatabases(ctx)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: arc.UserMessage(err)},
				},
				IsError: true,
			}, nil, nil
		}

		if len(dbResult.Databases) == 0 {
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: "No databases found."},
				},
			}, nil, nil
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "Found %d database(s):\n\n", dbResult.Count)
		for _, db := range dbResult.Databases {
			fmt.Fprintf(&sb, "- **%s** (%d measurements)\n", db.Name, db.MeasurementCount)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: sb.String()},
			},
		}, nil, nil
	})
}
