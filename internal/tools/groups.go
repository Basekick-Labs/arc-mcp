package tools

import (
	"fmt"
	"strings"
)

// The tool groups --tools / ARC_MCP_TOOLS can name.
const (
	// GroupData is the data tools: list_databases, list_measurements,
	// describe_measurement, query and get_sample_data. The default.
	GroupData = "data"
	// GroupOps is the operations tools, arc_health and arc_metrics. Opt-in.
	GroupOps = "ops"
)

// DefaultGroups is the value --tools has when nothing sets it.
const DefaultGroups = GroupData

// Groups says which tool groups the server registers.
type Groups struct {
	Data bool
	Ops  bool
}

// String lists the enabled groups in registration order, as --tools takes
// them: "data", "ops" or "data,ops".
func (g Groups) String() string {
	var names []string
	if g.Data {
		names = append(names, GroupData)
	}
	if g.Ops {
		names = append(names, GroupOps)
	}
	return strings.Join(names, ",")
}

// ParseGroups reads a --tools / ARC_MCP_TOOLS value: group names separated
// by commas, with case and surrounding spaces ignored and repeats allowed.
// A value that names no group at all (empty, blank, only commas) means the
// default, data, and blank reports that so the caller can say it. A name
// that is not a group is an error naming it and listing the valid ones: a
// typo in an opt-in is the user asking for something, and silently serving
// less would hide it.
func ParseGroups(value string) (g Groups, blank bool, err error) {
	names := 0
	for _, item := range strings.Split(value, ",") {
		name := strings.ToLower(strings.TrimSpace(item))
		switch name {
		case "":
			continue
		case GroupData:
			g.Data = true
		case GroupOps:
			g.Ops = true
		default:
			return Groups{}, false, fmt.Errorf("unknown tool group %q in --tools/ARC_MCP_TOOLS; the groups are %s and %s", strings.TrimSpace(item), GroupData, GroupOps)
		}
		names++
	}
	if names == 0 {
		return Groups{Data: true}, true, nil
	}
	return g, false, nil
}
