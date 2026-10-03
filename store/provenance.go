package store

import (
	"strings"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// provenance.go builds the ONE SQL predicate that separates a vendor-reported
// cost from a computed one, from model's vocabulary rather than from a copy of
// it. The vocabulary is model's because report and tui are peers that both need
// it (see model.PriceProvenance); the SQL is here because model holds no SQL.
//
// Every aggregate that sums cost_micro_usd carries a count of the rows in the
// sum that were COMPUTED — valued off a public rate card rather than reported by
// the harness — so a surface can mark the figure as an estimate without
// re-reading the rows. It is a count and not a flag for the same reason
// UnpricedEvents is: "some of this is an estimate" and "how much of it" are
// different questions, and a caller that only needs the first can compare
// against zero.

// vendorPriceSourceSQL renders the "this cost came from the harness" predicate
// for a price_source column. The exact stamps go in an IN list; each family
// matches its bare name or the name followed by "-", which is a LIKE — safe
// because model.VendorPriceSourceFamilies is asserted to hold no LIKE
// metacharacter.
//
// The values are inlined rather than bound as parameters on purpose: this
// expression is concatenated into select lists that are themselves reused by
// several queries, and threading a growing tail of args through every one of
// them would put the arg ORDER at the mercy of where the expression happened to
// be spliced. The inputs are compile-time constants from model, never user
// input, so there is nothing here to inject.
func vendorPriceSourceSQL(srcCol string) string {
	exact := model.VendorPriceSources()
	quoted := make([]string, len(exact))
	for i, v := range exact {
		quoted[i] = "'" + strings.ReplaceAll(v, "'", "''") + "'"
	}
	parts := make([]string, 0, len(model.VendorPriceSourceFamilies())+1)
	if len(quoted) > 0 {
		parts = append(parts, srcCol+" IN ("+strings.Join(quoted, ",")+")")
	}
	for _, f := range model.VendorPriceSourceFamilies() {
		esc := strings.ReplaceAll(f, "'", "''")
		parts = append(parts, "("+srcCol+" = '"+esc+"' OR "+srcCol+" LIKE '"+esc+"-%')")
	}
	if len(parts) == 0 {
		// No vendor vocabulary at all: nothing is vendor-reported, so every
		// priced row is computed. "0" is the false literal SQLite understands.
		return "0"
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

// computedCostCountSQL counts the rows of a group whose cost was COMPUTED: it
// has a cost at all, and that cost is not one an adapter stamped. A row with no
// cost contributes to UnpricedEvents instead, and a row with no joined usage
// event at all (the LEFT JOINs in activity.go and turncontext.go) has a NULL
// cost and so lands in neither.
func computedCostCountSQL(costCol, srcCol string) string {
	return `COALESCE(SUM(CASE WHEN ` + costCol + ` IS NOT NULL AND NOT ` +
		vendorPriceSourceSQL(srcCol) + ` THEN 1 ELSE 0 END),0)`
}
