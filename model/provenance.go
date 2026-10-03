package model

import "strings"

// provenance.go answers one question about a stored cost: WHO priced it. The
// ledger records the answer verbatim in usage_events.price_source, whose
// vocabulary is deliberately open (see UsageEvent.PriceSource) — nothing parses
// it, and a new rung may appear without a schema change. This file is the ONE
// place that classifies it, so a surface never has to.
//
// The distinction is not cosmetic. A vendor-reported cost is the harness's own
// accounting of what it was charged — copilot's nano-AIU valuation, crush's
// session cost, goose's provider-reported figure, pi/openclaw's per-request
// amount — and collect.stampCost is forbidden from overwriting one. A computed
// cost is this project's ESTIMATE of the same charge from a public rate card
// (the LiteLLM ladder, the embedded snapshot, a user override), which is a
// different kind of number and must not read as the bill.
//
// It lives in model rather than in tui or report for the usual reason: those two
// are peers in the layering and may not import each other, and two hand-kept
// copies of this vocabulary would drift the first time an adapter learned to
// read a vendor's price.

// CostProvenance names who produced a cost figure.
type CostProvenance string

const (
	// CostAbsent: no cost at all. An empty price_source, which is what an
	// unpriced row carries. Never "free".
	CostAbsent CostProvenance = "none"
	// CostVendor: the harness's own accounting, stamped by its adapter and never
	// overwritten by the price ladder.
	CostVendor CostProvenance = "vendor-reported"
	// CostComputed: valued by this project from a public rate card — the LiteLLM
	// ladder, the embedded snapshot, or a configured override. An estimate.
	CostComputed CostProvenance = "computed"
)

// vendorPriceSources is the exact ALLOW-LIST of adapter-stamped price sources.
// Verified against the adapter sources that stamp them:
//
//	copilot-nano-aiu    adapter/copilot/cost.go  (PriceSourceAIU)
//	crush-session-cost  adapter/crush/crush.go   (PriceSourceReported)
//	pi-reported         adapter/pi/pi.go         (a.tool + "-reported")
//	openclaw-reported   adapter/pi/pi.go         (the same expression,
//	                                                       for the other tool)
//
// It is an allow-list and not a prefix match on tool ids, for the same reason
// the raw payloads are: a rule like "starts with a tool name" would silently
// admit whatever a future stamp happened to be called, and the point of this
// vocabulary is that adding to it is a decision.
var vendorPriceSources = []string{
	"copilot-nano-aiu",
	"crush-session-cost",
	"openclaw-reported",
	"pi-reported",
}

// vendorPriceSourceFamilies are the adapter stamps whose TAIL comes from the
// source rather than from the adapter, so they cannot be enumerated exactly.
// goose is the only one: it labels a cost with the provenance goose itself
// recorded ('provider_reported', 'estimated', 'carried_forward', ...) under a
// "goose-" prefix, and stamps a bare "goose" when the column is empty
// (adapter/goose/goose.go, priceSource). A family matches the bare
// name and the name followed by "-".
//
// Family names must contain no SQL LIKE metacharacter: store builds a LIKE
// pattern from them and an unescaped '%' or '_' there would widen the match
// silently (TestVendorPriceSourceFamiliesAreLiteral).
var vendorPriceSourceFamilies = []string{
	"goose",
}

// VendorPriceSources returns the exact adapter-stamped price sources. The slice
// is a copy: the vocabulary is closed and callers must not extend it in place.
func VendorPriceSources() []string {
	return append([]string(nil), vendorPriceSources...)
}

// VendorPriceSourceFamilies returns the adapter-stamped price-source families.
// A source belongs to family f when it equals f or begins with f+"-". The slice
// is a copy.
func VendorPriceSourceFamilies() []string {
	return append([]string(nil), vendorPriceSourceFamilies...)
}

// PriceProvenance classifies a stored price_source.
//
// The default is CostComputed, and that direction is deliberate: the ladder's
// own rungs are open-ended ("litellm-<date>", "embedded-<date>", "override",
// their "+"-joined composites and the "+long-context" suffix), so enumerating
// them would mean chasing every new one, while the vendor set is closed and
// grows only when an adapter learns to read a price. An unrecognised source is
// therefore reported as an estimate — the reading that understates confidence
// rather than overstating it.
func PriceProvenance(priceSource string) CostProvenance {
	if priceSource == "" {
		return CostAbsent
	}
	for _, v := range vendorPriceSources {
		if priceSource == v {
			return CostVendor
		}
	}
	for _, f := range vendorPriceSourceFamilies {
		if priceSource == f || strings.HasPrefix(priceSource, f+"-") {
			return CostVendor
		}
	}
	return CostComputed
}
