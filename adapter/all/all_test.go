package all

import "testing"

// Every entry must carry a DISTINCT tool id. The registry resolves an id by
// returning the first match, so a duplicate silently makes one adapter
// unreachable through Get while both still collect - and one package here
// deliberately registers twice (pi serves Pi and OpenClaw), which is exactly the
// shape a copy-paste mistake takes.
func TestDefaultRegistersEachToolOnce(t *testing.T) {
	seen := map[string]bool{}
	for _, ad := range Default().All() {
		if ad.ID() == "" {
			t.Errorf("%T registers with an empty tool id", ad)
			continue
		}
		if seen[ad.ID()] {
			t.Errorf("tool %q is registered more than once", ad.ID())
		}
		seen[ad.ID()] = true
	}
	if len(seen) == 0 {
		t.Fatal("Default() registered nothing")
	}
}

// A registry is per call. Callers keep one for the life of a command and a
// second caller must not be handed the first one's slice to append to.
func TestDefaultReturnsAFreshRegistry(t *testing.T) {
	if a, b := Default(), Default(); a == b {
		t.Error("Default() handed out the same registry twice")
	}
}
