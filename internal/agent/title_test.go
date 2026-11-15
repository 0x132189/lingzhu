package agent

import (
	"testing"

	"lingzhu/internal/provider"
)

func TestTitleTargetUsesCatalogSmallModel(t *testing.T) {
	fake := &fakeProvider{}
	catalog := provider.Static("p", "P", fake, []provider.ModelSpec{
		{ID: "big", Family: "big"},
		{ID: "mini", Family: "claude-haiku"},
	}, "big")
	got := titleTargetOf(catalog, provider.Target{ProviderID: "p", Model: "big"})
	if got.Model != "mini" {
		t.Fatalf("small model was not selected: %+v", got)
	}
}

func TestTitleTargetFallsBackToLowerVariant(t *testing.T) {
	fake := &fakeProvider{}
	catalog := provider.Static("p", "P", fake, []provider.ModelSpec{
		{ID: "big", Family: "big", Variants: map[string]map[string]any{"low": {"reasoning_effort": "low"}}},
	}, "big")
	got := titleTargetOf(catalog, provider.Target{ProviderID: "p", Model: "big"})
	if got.Model != provider.VariantID("big", "low") {
		t.Fatalf("lower variant was not selected: %+v", got)
	}
}
