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

func TestCatalogExpandsVariantsAndFiltersModels(t *testing.T) {
	catalog, err := NewCatalog([]ProviderSpec{{
		ID:        "a",
		BaseURL:   "https://a.test/v1",
		APIKey:    "key",
		Blacklist: []string{"hidden"},
		Models: []ModelSpec{
			{
				ID:            "m1",
				Name:          "M1",
				ContextWindow: 1000,
				Options:       map[string]any{"reasoning_effort": "low"},
				Variants:      map[string]map[string]any{"high": {"reasoning_effort": "high", "text_verbosity": "low"}},
			},
			{ID: "hidden"},
		},
	}}, "a", "m1")
	if err != nil {
		t.Fatal(err)
	}
	target, err := catalog.Resolve(Selection{Provider: "a", Model: VariantID("m1", "high")})
	if err != nil {
		t.Fatal(err)
	}
	if target.Model != "m1::high" || target.RequestModel != "m1" {
		t.Fatalf("unexpected variant target: %+v", target)
	}
	if target.Options["reasoning_effort"] != "high" || target.Options["text_verbosity"] != "low" {
		t.Fatalf("unexpected variant options: %+v", target.Options)
	}
	if _, err := catalog.Resolve(Selection{Provider: "a", Model: "hidden"}); err == nil {
		t.Fatal("blacklisted model resolved")
	}
}

func TestCatalogWhitelistKeepsOnlyNamedModels(t *testing.T) {
	catalog, err := NewCatalog([]ProviderSpec{{
		ID:        "a",
		BaseURL:   "https://a.test/v1",
		APIKey:    "key",
		Whitelist: []string{"m2"},
		Models:    []ModelSpec{{ID: "m1"}, {ID: "m2"}},
	}}, "a", "m2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Resolve(Selection{Provider: "a", Model: "m1"}); err == nil {
		t.Fatal("model outside the whitelist resolved")
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
