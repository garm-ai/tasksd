package tasks

import (
	"strings"

	toolv1 "github.com/garm-ai/contracts/garm/tool/v1"
)

// clearanceValue reads a clearance written either way: the enum's own name
// (CLEARANCE_RESTRICTED), which is how a stored predicate spells it, or the
// short form (RESTRICTED), which is how a token claim does.
//
// A name this build does not know reads as the unspecified zero, which
// denies rather than admits: the absence of a clearance is not "public".
func clearanceValue(s string) toolv1.Clearance {
	if s == "" {
		return toolv1.Clearance_CLEARANCE_UNSPECIFIED
	}
	name := strings.ToUpper(s)
	if !strings.HasPrefix(name, "CLEARANCE_") {
		name = "CLEARANCE_" + name
	}
	return toolv1.Clearance(toolv1.Clearance_value[name])
}
