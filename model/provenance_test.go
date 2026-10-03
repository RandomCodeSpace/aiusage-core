package model

import (
	"strings"
	"testing"
)

// Every price source an adapter actually stamps must classify as vendor. These
// are the literals read off the adapter sources; a stamp this list does not
// name would be reported as an estimate, which understates the fidelity of a
// number the harness itself supplied.
func TestAdapterStampsClassifyAsVendor(t *testing.T) {
	for _, src := range []string{
		"copilot-nano-aiu",   // copilot/cost.go PriceSourceAIU
		"crush-session-cost", // crush/crush.go PriceSourceReported
		"pi-reported",        // pi/pi.go, a.tool + "-reported"
		"openclaw-reported",  // the same expression for the other tool
		"goose",              // goose/goose.go priceSource, empty cost_source
		"goose-provider_reported",
		"goose-estimated",
		"goose-carried_forward",
	} {
		if got := PriceProvenance(src); got != CostVendor {
			t.Errorf("PriceProvenance(%q) = %q, want %q", src, got, CostVendor)
		}
	}
}

// Every rung of the price ladder must classify as computed, including the
// composites and the long-context suffix the open vocabulary allows. These are
// this project's estimate of a charge, not the charge.
func TestLadderRungsClassifyAsComputed(t *testing.T) {
	for _, src := range []string{
		"override",
		"litellm-2026-08-16",
		"embedded-2026-05-01",
		"override+litellm-2026-08-16",
		"litellm-2026-08-16+long-context",
		"something-nobody-has-invented-yet",
	} {
		if got := PriceProvenance(src); got != CostComputed {
			t.Errorf("PriceProvenance(%q) = %q, want %q", src, got, CostComputed)
		}
	}
}

// An empty price source is the absence of a cost, which is neither vendor nor
// estimate. A surface renders it as unknown, never as free.
func TestEmptyPriceSourceIsAbsent(t *testing.T) {
	if got := PriceProvenance(""); got != CostAbsent {
		t.Errorf("PriceProvenance(\"\") = %q, want %q", got, CostAbsent)
	}
}

// A family matches its bare name and its "-" descendants and NOTHING ELSE. A
// prefix match without the separator would classify a hypothetical
// "goosewrangler-estimated" as goose's own number.
func TestVendorFamilyDoesNotMatchAcrossTheSeparator(t *testing.T) {
	for _, src := range []string{"goosewrangler", "goosewrangler-estimated", "not-goose"} {
		if got := PriceProvenance(src); got != CostComputed {
			t.Errorf("PriceProvenance(%q) = %q, want %q — the family must not match past its separator",
				src, got, CostComputed)
		}
	}
}

// store builds a SQL LIKE pattern from each family name. An unescaped '%' or
// '_' there would widen the predicate silently and mark rows vendor-priced that
// are not.
func TestVendorPriceSourceFamiliesAreLiteral(t *testing.T) {
	for _, f := range VendorPriceSourceFamilies() {
		if f == "" {
			t.Error("an empty family name would match every price source")
		}
		if strings.ContainsAny(f, "%_") {
			t.Errorf("family %q holds a LIKE metacharacter; store builds %q+'-%%' from it", f, f)
		}
	}
}

// The exported vocabularies are copies: the sets are closed, and a caller that
// could append to them could widen what counts as a vendor price from outside
// this file.
func TestVendorVocabularyIsCopied(t *testing.T) {
	a := VendorPriceSources()
	if len(a) == 0 {
		t.Fatal("no vendor price sources declared")
	}
	a[0] = "clobbered"
	if VendorPriceSources()[0] == "clobbered" {
		t.Error("VendorPriceSources returned the backing array, not a copy")
	}
	b := VendorPriceSourceFamilies()
	b[0] = "clobbered"
	if VendorPriceSourceFamilies()[0] == "clobbered" {
		t.Error("VendorPriceSourceFamilies returned the backing array, not a copy")
	}
}
