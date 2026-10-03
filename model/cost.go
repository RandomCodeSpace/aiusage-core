package model

import "fmt"

// UnpricedMark is what a cost renders when nothing behind it could be priced.
// Deliberately not "$0.00": a missing price is not a free request. ASCII so the
// value survives pipes, dumb terminals and CSV round trips.
const UnpricedMark = "-"

// FormatCost renders a micro-USD amount as display copy, and is the single
// definition of that copy for every surface — the report tables, the JSON
// summaries and the dashboard. It lives here because report and tui are peers
// in the layering and may not import each other, and two hand-kept copies of a
// money format drift.
//
// known is false when nothing in the amount could be priced at all, which
// renders as UnpricedMark rather than a zero. approximate marks a figure that
// includes rows valued at display time, or that omits rows nothing could value:
// either way the number is not the bill, and it carries a leading "~" so it
// cannot be read as exact. Sub-cent amounts widen to four decimals, and
// anything smaller than that renders as "<$0.0001" — a real charge must never
// print as $0.00.
func FormatCost(microUSD int64, approximate, known bool) string {
	if !known {
		return UnpricedMark
	}
	prefix := ""
	if approximate {
		prefix = "~"
	}
	if microUSD > 0 && microUSD < 100 {
		return prefix + "<$0.0001"
	}
	return prefix + "$" + formatUSD(microUSD)
}

// formatUSD renders micro-USD as a dollar amount, widening the precision below
// a cent so small-but-real costs stay visible.
func formatUSD(micro int64) string {
	usd := float64(micro) / 1e6
	if micro < 10_000 && micro > 0 {
		return fmt.Sprintf("%.4f", usd)
	}
	return fmt.Sprintf("%.2f", usd)
}
