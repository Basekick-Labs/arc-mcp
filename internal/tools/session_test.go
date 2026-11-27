package tools

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/basekick-labs/arc-mcp/internal/arc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newSession stands up arcHandler as a fake Arc, registers tools against
// it, and returns a client session over a real in-memory MCP transport, so
// tests exercise the registered handlers, not helpers next to them.
func newSession(t *testing.T, arcHandler http.Handler, register func(*mcp.Server, *arc.Client)) *mcp.ClientSession {
	t.Helper()
	return newSessionTimeout(t, arcHandler, 5*time.Second, register)
}

// newSessionTimeout is newSession with the client's --timeout set.
func newSessionTimeout(t *testing.T, arcHandler http.Handler, timeout time.Duration, register func(*mcp.Server, *arc.Client)) *mcp.ClientSession {
	t.Helper()
	arcSrv := httptest.NewServer(arcHandler)
	t.Cleanup(arcSrv.Close)
	return newSessionClient(t, arc.NewClient(arcSrv.URL, "tok", timeout), register)
}

// newSessionClient registers tools against client and returns a client
// session over a real in-memory MCP transport.
func newSessionClient(t *testing.T, client *arc.Client, register func(*mcp.Server, *arc.Client)) *mcp.ClientSession {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "arc-mcp-test", Version: "test"}, nil)
	register(server, client)

	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil).Connect(t.Context(), clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}
