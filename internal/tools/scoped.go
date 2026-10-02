package tools

import (
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ScopedStatus is the marker ScopedResult puts in its structured
// content. A model reading the result can branch on it without parsing
// prose.
const ScopedStatus = "scoped"

// ScopedOutcome is the structured body of a scoped result.
type ScopedOutcome struct {
	// Status is always ScopedStatus.
	Status string `json:"status"`
	// Database is the database that was refused, or "" when the call
	// asked for every database.
	Database string `json:"database,omitempty"`
	// NextStep names what has to happen for the call to succeed. It is a
	// fixed vocabulary so a model does not have to interpret the reason
	// text: "ask_user_for_database".
	NextStep string `json:"next_step"`
	// Retryable is always false: Arc will answer an identical call the
	// same way. Only a different database changes the outcome.
	Retryable bool `json:"retryable"`
	// Reason is a one-line explanation suitable for showing the user.
	Reason string `json:"reason"`
	// Tool names the tool to call once a database is known.
	Tool string `json:"tool,omitempty"`
}

// ScopedResult builds the answer for Arc's per-database permission
// denial: the token may read, but not what was asked for.
//
// This is deliberately NOT a tool error. The call failed for a reason no
// retry can fix and no credential arc-mcp holds can widen — the only way
// forward is for the user to name a database the token is granted. A
// tool error would invite the model to retry or to report a fault;
// a result carrying next_step tells it to ask.
//
// database is the database that was refused, or "" when the caller asked
// for every database. tool names the tool to call once a database is
// known, and may be "".
func ScopedResult(database, tool string) *mcp.CallToolResult {
	out := ScopedOutcome{
		Status:    ScopedStatus,
		Database:  database,
		NextStep:  "ask_user_for_database",
		Retryable: false,
		Tool:      tool,
	}
	var text string
	if database == "" {
		out.Reason = "This Arc token is restricted to specific databases, so Arc will not list them all."
		text = "This Arc token is restricted to specific databases, so Arc will not list them all — " +
			"it refuses the request rather than returning a partial list. " +
			"Ask the user which database to work with, then call list_measurements with that database. " +
			"Do not retry this call: the answer will not change."
	} else {
		out.Reason = fmt.Sprintf("This Arc token has no read access to database %q.", database)
		text = fmt.Sprintf(
			"This Arc token has no read access to database %q. It may be able to read other databases. "+
				"Ask the user which database to use, then call list_measurements with that one. "+
				"Do not retry with %q: the answer will not change.",
			database, database)
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: text}},
		StructuredContent: out,
	}
}
