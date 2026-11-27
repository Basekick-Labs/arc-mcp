package main

import (
	"strings"
	"testing"
)

// The instructions carry the row cap the server actually enforces, and the
// habits the README promises: time bounds and aggregation.
func TestServerInstructions(t *testing.T) {
	got := serverInstructions(1234, true, false)
	for _, want := range []string{"time-series lakehouse", "capped at 1234 rows", "WHERE time > now() - INTERVAL", "time_bucket(INTERVAL '5 minutes', time)", `"load"`} {
		if !strings.Contains(got, want) {
			t.Errorf("instructions lack %q", want)
		}
	}
	if strings.Contains(got, "columnar analytical") {
		t.Error("old positioning still present")
	}
}

func TestServerInstructionsMentionOptionalOpsWhenEnabled(t *testing.T) {
	got := serverInstructions(500, true, true)
	for _, want := range []string{"arc_health", "arc_metrics", "read-only"} {
		if !strings.Contains(got, want) {
			t.Errorf("instructions lack %q", want)
		}
	}
}

func TestServerInstructionsSupportOpsOnly(t *testing.T) {
	got := serverInstructions(500, false, true)
	if !strings.Contains(got, "arc_health") || strings.Contains(got, "list_databases") {
		t.Errorf("ops-only instructions describe the wrong tools: %q", got)
	}
}

func TestParseToolGroups(t *testing.T) {
	tests := []struct {
		input     string
		data, ops bool
		wantErr   bool
	}{
		{input: "data", data: true},
		{input: " ops ", ops: true},
		{input: "data,ops", data: true, ops: true},
		{input: "ops,data", data: true, ops: true},
		{input: "", wantErr: true},
		{input: "data,", wantErr: true},
		{input: "data,data", wantErr: true},
		{input: "all", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			data, ops, err := parseToolGroups(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseToolGroups(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if data != tt.data || ops != tt.ops {
				t.Errorf("parseToolGroups(%q) = (%v, %v), want (%v, %v)", tt.input, data, ops, tt.data, tt.ops)
			}
		})
	}
}
