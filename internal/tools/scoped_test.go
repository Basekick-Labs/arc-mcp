package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/basekick-labs/arc-mcp/internal/arc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Arc's answers for the database listing routes.
const (
	listBodyOK    = `{"databases":[{"name":"metrics","measurement_count":2}],"count":1}`
	measBodyOK    = `{"database":"metrics","measurements":[{"name":"cpu"}],"count":1}`
	coarse403Body = `{"success":false,"error":"token does not have 'read' permission"}`
	// What the listing routes' read middleware actually sends; the database
	// name is empty on the list-everything route, which sends none.
	scoped403Body  = `{"success":false,"error":"no permission for read on database 'metrics'"}`
	scopedAllBody  = `{"success":false,"error":"no permission for read on database ''"}`
	unavailable403 = `{"success":false,"error":"permission data unavailable"}`
	unauth401Body  = `{"success":false,"error":"Authentication required"}`
	notFound404    = `{"error":"Database 'metrics' not found"}`
)

// callTool stands up the two listing tools against a fake Arc that
// answers every request with one status and body, then calls one tool
// over a real in-memory MCP session. This exercises the registered
// handler, not a helper next to it.
func callTool(t *testing.T, status int, body, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()

	arcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(arcSrv.Close)

	client := arc.NewClient(arcSrv.URL, "tok", 5*time.Second)
	server := mcp.NewServer(&mcp.Implementation{Name: "arc-mcp-test", Version: "test"}, nil)
	RegisterListDatabases(server, client)
	RegisterListMeasurements(server, client)

	serverT, clientT := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	serverSession, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	session, err := mcpClient.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", tool, err)
	}
	return res
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// decodeScoped reads the structured content back as a ScopedOutcome.
// Over the wire it arrives as JSON, so this is what a model's host
// actually sees.
func decodeScoped(t *testing.T, res *mcp.CallToolResult) ScopedOutcome {
	t.Helper()
	if res.StructuredContent == nil {
		t.Fatalf("result carries no structured content; text was %q", resultText(t, res))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out ScopedOutcome
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode structured content %s: %v", raw, err)
	}
	return out
}

// A scoped token asking for every database gets a result the model can
// act on — ask the user for a database — not a tool error it can only
// retry or report as a fault.
func TestListDatabases_ScopedIsActionableNotAnError(t *testing.T) {
	res := callTool(t, http.StatusForbidden, scopedAllBody, "list_databases", nil)

	if res.IsError {
		t.Errorf("IsError = true; a scoped token is an expected state, not a tool failure")
	}
	out := decodeScoped(t, res)
	if out.Status != ScopedStatus {
		t.Errorf("status = %q; want %q", out.Status, ScopedStatus)
	}
	if out.NextStep != "ask_user_for_database" {
		t.Errorf("next_step = %q; want ask_user_for_database", out.NextStep)
	}
	if out.Retryable {
		t.Error("retryable = true; Arc answers an identical call identically")
	}
	if out.Database != "" {
		t.Errorf("database = %q; the list-everything call names none", out.Database)
	}
	if text := resultText(t, res); !strings.Contains(text, "Ask the user which database") {
		t.Errorf("text does not tell the model to ask: %q", text)
	}
}

// Same for a named database the token cannot read: the database it was
// refused for must be named so the model does not re-ask for it.
func TestListMeasurements_ScopedIsActionableNotAnError(t *testing.T) {
	res := callTool(t, http.StatusForbidden, scoped403Body, "list_measurements",
		map[string]any{"database": "metrics"})

	if res.IsError {
		t.Errorf("IsError = true; a scoped token is an expected state, not a tool failure")
	}
	out := decodeScoped(t, res)
	if out.Database != "metrics" {
		t.Errorf("database = %q; want metrics", out.Database)
	}
	if out.NextStep != "ask_user_for_database" {
		t.Errorf("next_step = %q; want ask_user_for_database", out.NextStep)
	}
	if text := resultText(t, res); !strings.Contains(text, "metrics") {
		t.Errorf("text does not name the refused database: %q", text)
	}
}

// 401, the coarse 403 and 404 stay tool errors: none of them is fixed by
// naming a different database, and each must say something different.
func TestListingTools_OtherStatusesStayErrors(t *testing.T) {
	for _, tt := range []struct {
		name     string
		status   int
		body     string
		wantText string
		notText  string
	}{
		{"unauthorized", http.StatusUnauthorized, unauth401Body, "missing, invalid or expired", "which database"},
		{"no read permission", http.StatusForbidden, coarse403Body, "does not carry the permission", "which database"},
		{"grants could not be loaded", http.StatusForbidden, unavailable403, "server-side fault", "which database"},
		{"not found", http.StatusNotFound, notFound404, "not found", "which database"},
		{"server fault", http.StatusInternalServerError, `{"error":"boom"}`, "see server logs", "which database"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, tool := range []struct {
				name string
				args map[string]any
			}{
				{"list_databases", nil},
				{"list_measurements", map[string]any{"database": "metrics"}},
			} {
				res := callTool(t, tt.status, tt.body, tool.name, tool.args)
				if !res.IsError {
					t.Errorf("%s: IsError = false for HTTP %d", tool.name, tt.status)
				}
				if res.StructuredContent != nil {
					t.Errorf("%s: HTTP %d produced structured scoping content", tool.name, tt.status)
				}
				text := resultText(t, res)
				if !strings.Contains(text, tt.wantText) {
					t.Errorf("%s: text %q does not contain %q", tool.name, text, tt.wantText)
				}
				if strings.Contains(text, tt.notText) {
					t.Errorf("%s: text %q misreports HTTP %d as a scoping state", tool.name, text, tt.status)
				}
			}
		})
	}
}

// The success path is unchanged.
func TestListingTools_SuccessUnchanged(t *testing.T) {
	res := callTool(t, http.StatusOK, listBodyOK, "list_databases", nil)
	if res.IsError {
		t.Fatalf("list_databases errored: %q", resultText(t, res))
	}
	if text := resultText(t, res); !strings.Contains(text, "metrics") {
		t.Errorf("list_databases text = %q; want the database named", text)
	}

	res = callTool(t, http.StatusOK, measBodyOK, "list_measurements", map[string]any{"database": "metrics"})
	if res.IsError {
		t.Fatalf("list_measurements errored: %q", resultText(t, res))
	}
	if text := resultText(t, res); !strings.Contains(text, "cpu") {
		t.Errorf("list_measurements text = %q; want the measurement named", text)
	}
}
