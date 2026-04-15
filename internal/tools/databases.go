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
	}, func(ctx context.Context, req *mcp.CallToolRequest, args ListDatabasesArgs) (*mcp.CallToolResult, any, error) {
		result, err := client.ListDatabases(ctx)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: fmt.Sprintf("Error listing databases: %v", err)},
				},
				IsError: true,
			}, nil, nil
		}

		if len(result.Databases) == 0 {
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: "No databases found."},
				},
			}, nil, nil
		}

		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Found %d database(s):\n\n", result.Count))
		for _, db := range result.Databases {
			sb.WriteString(fmt.Sprintf("- **%s** (%d measurements)\n", db.Name, db.MeasurementCount))
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: sb.String()},
			},
		}, nil, nil
	})
}
