package meta

import (
	"database/sql/driver"
	"encoding/hex"
	"fmt"
	"strings"

	"modernc.org/sqlite"
)

// Registered before any store opens, so migrations and every pooled writer
// use identical Unicode folding/token generation. No IO or mutable state is
// reachable from these SQL functions.
func init() {
	for name, fn := range map[string]func(string) string{
		"cloudfs_short_terms": shortNameTerms,
		"cloudfs_search_fold": strings.ToLower,
	} {
		if err := sqlite.RegisterDeterministicScalarFunction(name, 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			value, ok := args[0].(string)
			if !ok {
				return nil, fmt.Errorf("search function requires text")
			}
			return fn(value), nil
		}); err != nil {
			panic(err)
		}
	}
}

func shortNameToken(s string) string { return "x" + hex.EncodeToString([]byte(s)) }

func shortNameTerms(name string) string {
	runes := []rune(strings.ToLower(name))
	seen := make(map[string]bool, len(runes)*2)
	var terms []string
	for i := range runes {
		for n := 1; n <= 2 && i+n <= len(runes); n++ {
			term := shortNameToken(string(runes[i : i+n]))
			if !seen[term] {
				seen[term] = true
				terms = append(terms, term)
			}
		}
	}
	return strings.Join(terms, " ")
}
