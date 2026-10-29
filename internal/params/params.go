// Package params resolves `name`/`query` style aliases so one wrong guess is not fatal.
package params

import (
	"fmt"
	"strings"

	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

// Alias is one candidate parameter.
type Alias struct{ Name, Value string }

// ResolveName returns the preferred value, else the first non-empty alias.
func ResolveName(example string, preferred Alias, others ...Alias) (string, error) {
	if preferred.Value != "" {
		return preferred.Value, nil
	}
	var names []string
	for _, o := range others {
		if o.Value != "" {
			return o.Value, nil
		}
		names = append(names, "`"+o.Name+"`")
	}
	msg := fmt.Sprintf("Pass `%s` (e.g. %s=%q)", preferred.Name, preferred.Name, example)
	switch len(names) {
	case 0:
	case 1:
		msg += "; " + names[0] + " is accepted as an alias"
	default:
		msg += "; aliases accepted: " + strings.Join(names, ", ")
	}
	return "", toolerr.Inputf("%s.", msg)
}
