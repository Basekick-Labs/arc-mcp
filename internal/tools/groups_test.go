package tools

import (
	"strings"
	"testing"
)

func TestParseGroups(t *testing.T) {
	tests := []struct {
		value     string
		want      Groups
		wantBlank bool
	}{
		{"data", Groups{Data: true}, false},
		{"ops", Groups{Ops: true}, false},
		{"data,ops", Groups{Data: true, Ops: true}, false},
		{" Data , OPS ", Groups{Data: true, Ops: true}, false},
		{"ops,data,ops", Groups{Data: true, Ops: true}, false},
		{"data,,", Groups{Data: true}, false},
		{"enterprise", Groups{Enterprise: true}, false},
		{"data,ops,enterprise", Groups{Data: true, Ops: true, Enterprise: true}, false},
		{" ENTERPRISE ,data", Groups{Data: true, Enterprise: true}, false},
		// Naming no group at all is the default, never an error and never
		// less than the default.
		{"", Groups{Data: true}, true},
		{" ", Groups{Data: true}, true},
		{",", Groups{Data: true}, true},
		{" , ,", Groups{Data: true}, true},
	}
	for _, tt := range tests {
		got, blank, err := ParseGroups(tt.value)
		if err != nil || got != tt.want || blank != tt.wantBlank {
			t.Errorf("ParseGroups(%q) = %+v, blank=%v, %v; want %+v, blank=%v", tt.value, got, blank, err, tt.want, tt.wantBlank)
		}
	}
}

// A name that is not a group is refused, naming it and the valid groups:
// a typo in an opt-in must not quietly serve less than was asked for.
func TestParseGroups_UnknownNameIsRefused(t *testing.T) {
	for _, value := range []string{"opps", "data,logs", "all", "ops, Metrics ", "enterprise-admin", "data,entreprise"} {
		got, _, err := ParseGroups(value)
		if err == nil {
			t.Errorf("ParseGroups(%q) = %+v, want an error", value, got)
			continue
		}
		bad := strings.TrimSpace(value[strings.LastIndex(value, ",")+1:])
		if msg := err.Error(); !strings.Contains(msg, `"`+bad+`"`) || !strings.Contains(msg, "data, ops and enterprise") {
			t.Errorf("ParseGroups(%q) error %q should name %q and the valid groups", value, msg, bad)
		}
	}
}

func TestGroupsString(t *testing.T) {
	for _, tt := range []struct {
		g    Groups
		want string
	}{
		{Groups{Data: true}, "data"},
		{Groups{Ops: true}, "ops"},
		{Groups{Data: true, Ops: true}, "data,ops"},
		{Groups{Enterprise: true}, "enterprise"},
		{Groups{Data: true, Ops: true, Enterprise: true}, "data,ops,enterprise"},
	} {
		if got := tt.g.String(); got != tt.want {
			t.Errorf("%+v.String() = %q, want %q", tt.g, got, tt.want)
		}
	}
}
