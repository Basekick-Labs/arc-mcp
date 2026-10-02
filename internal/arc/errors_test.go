package arc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The bodies Arc's database listing routes answer with. The two 403s
// mean different things and must not collapse onto one kind.
const (
	bodyNoToken  = `{"success":false,"error":"Authentication required"}`
	bodyBadToken = `{"success":false,"error":"Invalid or expired token"}`
	// Coarse refusals: the token carries no read permission at all.
	bodyCoarse403 = `{"success":false,"error":"token does not have 'read' permission"}`
	bodyOSS403    = `{"success":false,"error":"Permission denied: read required"}`
	// The read middleware's RBAC denial, which is what the listing routes
	// return in practice. The database name is empty on the
	// list-everything route, which sends none.
	bodyMWScoped    = `{"success":false,"error":"no permission for read on database 'metrics'"}`
	bodyMWScopedAll = `{"success":false,"error":"no permission for read on database ''"}`
	// The handler gates' own wording, on the listing routes and on the
	// SHOW/measurement paths.
	bodyScoped403    = `{"error":"access denied: no read permission for database 'metrics'"}`
	bodyScopedAll    = `{"error":"access denied: no read permission for database '*'"}`
	bodyScopedMeas   = `{"success":false,"error":"access denied: no read permission for metrics.*"}`
	bodyScopedShowDB = `{"success":false,"error":"access denied: no read permission to list databases"}`
	// Arc could not load the token's grants and refused rather than guess.
	bodyUnavailable = `{"success":false,"error":"permission data unavailable"}`
	bodyNotFound    = `{"error":"Database 'ghost' not found"}`
)

func statusServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestArcErrorFrom_Classification(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantKind string
	}{
		{"no token", 401, bodyNoToken, KindAuth},
		{"bad token", 401, bodyBadToken, KindAuth},
		{"no read permission", 403, bodyCoarse403, KindForbidden},
		{"RBAC not wired on this server", 403, bodyOSS403, KindForbidden},
		{"middleware denial, named database", 403, bodyMWScoped, KindScoped},
		{"middleware denial, no database sent", 403, bodyMWScopedAll, KindScoped},
		{"handler gate, named database", 403, bodyScoped403, KindScoped},
		{"handler gate, list-everything route", 403, bodyScopedAll, KindScoped},
		{"measurement listing denial", 403, bodyScopedMeas, KindScoped},
		{"SHOW DATABASES denial", 403, bodyScopedShowDB, KindScoped},
		{"grants could not be loaded", 403, bodyUnavailable, KindUnavailable},
		{"unknown 403 body", 403, `{"error":"Delete operations are disabled."}`, KindForbidden},
		{"plain-text 403", 403, `forbidden`, KindForbidden},
		{"missing database", 404, bodyNotFound, KindNotFound},
		{"server fault", 500, `{"error":"boom"}`, KindServer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := arcErrorFrom("list databases", tt.status, []byte(tt.body))
			if got.Kind != tt.wantKind {
				t.Errorf("Kind = %q; want %q", got.Kind, tt.wantKind)
			}
			if got.Status != tt.status {
				t.Errorf("Status = %d; want %d", got.Status, tt.status)
			}
			if gotScoped, want := Scoped(got), tt.wantKind == KindScoped; gotScoped != want {
				t.Errorf("Scoped() = %v; want %v", gotScoped, want)
			}
		})
	}
}

// Every permission kind must produce a distinct, actionable message.
// Telling the holder of a scoped token to "check the API token" sends
// them down the wrong path, which is what the single "auth" kind did.
func TestUserMessage_PermissionKindsAreDistinct(t *testing.T) {
	msgs := map[string]string{}
	for _, kind := range []string{KindAuth, KindForbidden, KindScoped, KindUnavailable, KindNotFound} {
		msgs[kind] = UserMessage(&Error{Kind: kind, Status: 403, Detail: "internal detail"})
	}
	seen := map[string]string{}
	for kind, msg := range msgs {
		if msg == "" {
			t.Errorf("kind %q has no message", kind)
		}
		if other, dup := seen[msg]; dup {
			t.Errorf("kinds %q and %q share a message: %q", kind, other, msg)
		}
		seen[msg] = kind
		if strings.Contains(msg, "internal detail") {
			t.Errorf("kind %q leaks the internal detail: %q", kind, msg)
		}
	}
	if !strings.Contains(msgs[KindScoped], "which database") {
		t.Errorf("the scoped message must ask for a database, got %q", msgs[KindScoped])
	}
	if strings.Contains(msgs[KindForbidden], "which database") {
		t.Errorf("a missing permission must not be reported as a scoping state, got %q", msgs[KindForbidden])
	}
	if strings.Contains(msgs[KindUnavailable], "which database") {
		t.Errorf("a server fault must not be reported as a scoping state, got %q", msgs[KindUnavailable])
	}
	if !strings.Contains(msgs[KindUnavailable], "server-side fault") {
		t.Errorf("the unavailable message must say it is not the caller's problem, got %q", msgs[KindUnavailable])
	}
}

func TestListDatabases_ScopedDenialIsClassified(t *testing.T) {
	c := NewClient(statusServer(t, http.StatusForbidden, bodyMWScopedAll).URL, "tok", 2*time.Second)
	_, err := c.ListDatabases(context.Background())
	if !Scoped(err) {
		t.Fatalf("ListDatabases error = %v; want a scoped denial", err)
	}
}

func TestListMeasurements_ScopedDenialIsClassified(t *testing.T) {
	c := NewClient(statusServer(t, http.StatusForbidden, bodyMWScoped).URL, "tok", 2*time.Second)
	_, err := c.ListMeasurements(context.Background(), "metrics")
	if !Scoped(err) {
		t.Fatalf("ListMeasurements error = %v; want a scoped denial", err)
	}
}

// A 401 and a 404 must not be mistaken for the scoping state.
func TestListingRoutes_OtherStatusesAreNotScoped(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
		kind   string
	}{
		{"unauthorized", 401, bodyNoToken, KindAuth},
		{"no read permission", 403, bodyCoarse403, KindForbidden},
		{"not found", 404, bodyNotFound, KindNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := NewClient(statusServer(t, tt.status, tt.body).URL, "tok", 2*time.Second)
			_, err := c.ListDatabases(context.Background())
			if Scoped(err) {
				t.Fatalf("%d classified as scoped: %v", tt.status, err)
			}
			ae, ok := err.(*Error) //nolint:errorlint // the client returns *Error directly
			if !ok {
				t.Fatalf("error is %T; want *Error", err)
			}
			if ae.Kind != tt.kind {
				t.Errorf("Kind = %q; want %q", ae.Kind, tt.kind)
			}
		})
	}
}
