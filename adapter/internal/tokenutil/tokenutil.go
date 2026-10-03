// Package tokenutil holds the shared token-accounting helpers used by adapters
// to normalise provider token counts into the project's UsageEvent model.
//
// It depends on nothing else in the project.
package tokenutil

// ApplyTotalFallback reconciles itemised token counts against a
// provider-reported total, replicating ccusage's apply_total_token_fallback
// semantics (see plan section 2).
//
// The provider sometimes reports a grand total that exceeds the sum of the
// components it itemised (input/output/cache/extra). When that happens we must
// attribute the unexplained remainder somewhere so that the stored total stays
// authoritative and SUM(components) stays consistent — without ever reducing a
// known component.
//
// Algorithm:
//
//	known   = input + output + cacheCreation + cacheRead + extra
//	missing = max(0, total - known)
//	if missing > 0:
//	    if output == 0 -> output  = missing   (fill the empty output gap)
//	    else           -> extra  += missing   (overflow into the extra bucket)
//
// Only output and extra are ever adjusted; all other components are left
// untouched. When the components already account for the total (or exceed it),
// the inputs are returned unchanged.
func ApplyTotalFallback(input, output, cacheCreation, cacheRead, extra, total int64) (newOutput, newExtra int64) {
	known := input + output + cacheCreation + cacheRead + extra
	missing := total - known
	if missing > 0 {
		if output == 0 {
			output = missing
		} else {
			extra += missing
		}
	}
	return output, extra
}
