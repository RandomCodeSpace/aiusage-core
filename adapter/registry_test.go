package adapter

import (
	"context"
	"testing"

	"github.com/RandomCodeSpace/aiusage-core/model"
)

// declaringAdapter is the minimum an adapter can be: an id and a declaration.
// The two are given SEPARATELY on purpose, so a test can build one whose
// declaration disagrees with its id.
type declaringAdapter struct {
	id   string
	decl model.ToolCapability
}

func (a declaringAdapter) ID() string                         { return a.id }
func (a declaringAdapter) DisplayName() string                { return a.id }
func (a declaringAdapter) Capabilities() model.ToolCapability { return a.decl }
func (a declaringAdapter) Discover(context.Context, DiscoverConfig) ([]Source, error) {
	return nil, nil
}
func (a declaringAdapter) Collect(context.Context, Source) (Observation, error) {
	return Observation{}, nil
}

func declaring(id string) declaringAdapter {
	return declaringAdapter{id: id, decl: model.ToolCapability{
		Tool:      id,
		Cost:      model.CostComputed,
		Activity:  model.ActivityNone,
		Reasoning: model.ReasoningReportFor(id),
		Tier:      model.TierFixture,
	}}
}

// TestRegistryCapabilitiesAggregatesDeclarations: the adapter declares and the
// registry aggregates. Nothing else may hold a table of these, which is why the
// map comes from here rather than from a loop each consumer writes.
func TestRegistryCapabilitiesAggregatesDeclarations(t *testing.T) {
	reg := NewRegistry(declaring(model.ToolCodex), declaring(model.ToolClaudeCode))

	caps := reg.Capabilities()
	if len(caps) != 2 {
		t.Fatalf("got %d declarations, want one per registered adapter", len(caps))
	}
	for _, ad := range reg.All() {
		got, ok := caps[ad.ID()]
		if !ok {
			t.Fatalf("%s is registered but absent from Capabilities", ad.ID())
		}
		if got != ad.Capabilities() {
			t.Errorf("%s declaration = %+v, want %+v", ad.ID(), got, ad.Capabilities())
		}
	}

	// An empty registry answers with an empty map, not nil: a caller ranging
	// over the result must not have to check.
	if caps := NewRegistry().Capabilities(); caps == nil {
		t.Error("an empty registry returned a nil map")
	}
}

// TestRegistryCapabilitiesKeysByRegisteredID: the registry is the authority on
// which tool an adapter IS. A declaration naming a different tool does not get
// to move the entry - it would silently overwrite the real adapter for that id,
// which is exactly the drift the required method exists to prevent.
func TestRegistryCapabilitiesKeysByRegisteredID(t *testing.T) {
	liar := declaring(model.ToolCodex)
	liar.decl.Tool = model.ToolClaudeCode

	caps := NewRegistry(liar).Capabilities()
	if _, ok := caps[model.ToolCodex]; !ok {
		t.Fatalf("declaration filed under its own Tool field instead of the registered id: %+v", caps)
	}
	if _, ok := caps[model.ToolClaudeCode]; ok {
		t.Errorf("a mis-declared Tool created an entry for a tool that is not registered")
	}
}
