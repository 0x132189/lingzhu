package provider

import "testing"

func TestCatalogResolvesDefaultAndExplicitTargets(t *testing.T) {
	catalog, err := NewCatalog([]ProviderSpec{
		{ID: "a", BaseURL: "https://a.test/v1", APIKey: "key-a", Models: []ModelSpec{
			{ID: "m1", ContextWindow: 1000},
			{ID: "m2", ContextWindow: 2000},
		}},
		{ID: "b", BaseURL: "https://b.test/v1", APIKey: "key-b", Models: []ModelSpec{
			{ID: "x", ContextWindow: 4000},
		}},
	}, "a", "m2")
	if err != nil {
		t.Fatal(err)
	}
	if target := catalog.Default(); target.ProviderID != "a" || target.Model != "m2" || target.Window != 2000 {
		t.Fatalf("unexpected default: %+v", target)
	}
	target, err := catalog.Resolve(Selection{Provider: "b", Model: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if target.Window != 4000 || target.ProviderName != "b" {
		t.Fatalf("unexpected resolved target: %+v", target)
	}
}

func TestCatalogGuessesProviderByUniqueModel(t *testing.T) {
	catalog, err := NewCatalog([]ProviderSpec{
		{ID: "a", BaseURL: "https://a.test/v1", APIKey: "key-a", Models: []ModelSpec{{ID: "m1"}}},
		{ID: "b", BaseURL: "https://b.test/v1", APIKey: "key-b", Models: []ModelSpec{{ID: "m2"}}},
	}, "", "m2")
	if err != nil {
		t.Fatal(err)
	}
	if target := catalog.Default(); target.ProviderID != "b" || target.Model != "m2" {
		t.Fatalf("unique model should select its provider: %+v", target)
	}
}

func TestCatalogRejectsMissingKey(t *testing.T) {
	catalog, err := NewCatalog([]ProviderSpec{
		{ID: "a", BaseURL: "https://a.test/v1", Models: []ModelSpec{{ID: "m1"}}},
	}, "a", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Resolve(Selection{}); err == nil {
		t.Fatal("want error for a provider without a key")
	}
	if catalog.Providers()[0].Ready {
		t.Fatal("provider without a key must not be ready")
	}
}

func TestCatalogValidatesSpecs(t *testing.T) {
	if _, err := NewCatalog([]ProviderSpec{{ID: "a", BaseURL: ""}}, "a", ""); err == nil {
		t.Fatal("want base URL error")
	}
	if _, err := NewCatalog([]ProviderSpec{
		{ID: "a", BaseURL: "https://a.test/v1", APIKey: "k", Models: []ModelSpec{{ID: "m"}, {ID: "m"}}},
	}, "a", "m"); err == nil {
		t.Fatal("want duplicate model error")
	}
	if _, err := NewCatalog([]ProviderSpec{
		{ID: "a", BaseURL: "https://a.test/v1", APIKey: "k", Models: []ModelSpec{{ID: "m"}}},
	}, "a", "other"); err == nil {
		t.Fatal("want unknown model error")
	}
}
