package goose

import (
	"database/sql"
	"math"
	"testing"
)

func TestCostRequiresPositiveRepresentableMicroUSD(t *testing.T) {
	for label, tc := range map[string]struct {
		cost  sql.NullFloat64
		want  int64
		known bool
	}{
		"absent": {}, "zero": {sql.NullFloat64{Float64: 0, Valid: true}, 0, false},
		"negative":          {sql.NullFloat64{Float64: -1, Valid: true}, 0, false},
		"nan":               {sql.NullFloat64{Float64: math.NaN(), Valid: true}, 0, false},
		"positive-infinity": {sql.NullFloat64{Float64: math.Inf(1), Valid: true}, 0, false},
		"negative-infinity": {sql.NullFloat64{Float64: math.Inf(-1), Valid: true}, 0, false},
		"submicro":          {sql.NullFloat64{Float64: 0.0000009, Valid: true}, 0, false},
		"micro":             {sql.NullFloat64{Float64: 0.000001, Valid: true}, 1, true},
		"rounding":          {sql.NullFloat64{Float64: 0.0123456, Valid: true}, 12346, true},
		"finite-overflow":   {sql.NullFloat64{Float64: math.MaxFloat64, Valid: true}, 0, false},
		"int64-boundary":    {sql.NullFloat64{Float64: float64(math.MaxInt64) / 1e6, Valid: true}, 0, false},
	} {
		t.Run(label, func(t *testing.T) {
			row := ledgerRow{id: 1, sessionID: "s", createdTS: 1780000000, model: sql.NullString{String: "model", Valid: true}, input: sql.NullInt64{Int64: 10, Valid: true}, cost: tc.cost, costSource: sql.NullString{String: "provider_reported", Valid: true}}
			ev, ok, consumed := buildEvent(row, "source.db")
			got, known := ev.Cost()
			if !ok || !consumed || known != tc.known || got != tc.want {
				t.Fatalf("cost=%d,%v event=%+v ok=%v consumed=%v", got, known, ev, ok, consumed)
			}
			if tc.known && ev.PriceSource != "goose-provider_reported" {
				t.Fatalf("source stamp=%q", ev.PriceSource)
			}
			if !tc.known && ev.PriceSource != "" {
				t.Fatalf("unknown cost has a source stamp: %q", ev.PriceSource)
			}
		})
	}
}
