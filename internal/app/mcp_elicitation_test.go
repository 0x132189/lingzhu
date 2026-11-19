package app

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestElicitationValueMapsEnumsAndTypes(t *testing.T) {
	numeric := elicitationProp{
		Type:      "integer",
		Enum:      []any{float64(1), float64(2)},
		EnumNames: []string{"One", "Two"},
	}
	if got := elicitationValue(numeric, []string{"Two"}); got != float64(2) {
		t.Fatalf("enum name did not map back to its value: %#v", got)
	}
	if got := elicitationValue(numeric, []string{"1"}); got != float64(1) {
		t.Fatalf("raw enum value did not survive: %#v", got)
	}

	oneOf := elicitationProp{
		Type: "string",
		OneOf: []elicitationProp{
			{Const: "monthly", Title: "Monthly"},
			{Const: "yearly", Title: "Yearly"},
		},
	}
	if got := elicitationValue(oneOf, []string{"Yearly"}); got != "yearly" {
		t.Fatalf("oneOf title did not map back to const: %#v", got)
	}

	array := elicitationProp{
		Type: "array",
		Items: &elicitationProp{
			Type: "string",
			Enum: []any{"read", "write"},
		},
	}
	if got := elicitationValue(array, []string{"read", "write"}); !reflect.DeepEqual(got, []any{"read", "write"}) {
		t.Fatalf("array values did not preserve enum values: %#v", got)
	}

	for input, want := range map[string]any{
		"yes":   true,
		"false": false,
	} {
		if got := elicitationValue(elicitationProp{Type: "boolean"}, []string{input}); got != want {
			t.Fatalf("boolean %q -> %#v, want %#v", input, got, want)
		}
	}
	if got := elicitationValue(elicitationProp{Type: "number"}, []string{"2.5"}); got != 2.5 {
		t.Fatalf("number was not parsed: %#v", got)
	}
	if got := elicitationValue(elicitationProp{Type: "integer"}, []string{"7"}); got != int64(7) {
		t.Fatalf("integer was not parsed: %#v", got)
	}
}

func TestElicitationKindMapsSchemaTypes(t *testing.T) {
	cases := []struct {
		property elicitationProp
		want     string
	}{
		{elicitationProp{Type: "boolean"}, "boolean"},
		{elicitationProp{Type: "number"}, "number"},
		{elicitationProp{Type: "integer"}, "integer"},
		{elicitationProp{Type: "string", Enum: []any{"a"}}, "select"},
		{elicitationProp{Type: "string", OneOf: []elicitationProp{{Const: "a"}}}, "select"},
		{elicitationProp{Type: "string", Format: "uri"}, "text"},
		{elicitationProp{Type: "array", Items: &elicitationProp{Type: "string", Enum: []any{"a"}}}, "multiselect"},
		{elicitationProp{Type: "array", Items: &elicitationProp{Type: "string"}}, "text"},
		{elicitationProp{Type: "object"}, "text"},
	}
	for _, tc := range cases {
		if got := elicitationKind(tc.property); got != tc.want {
			t.Fatalf("%+v: got %q, want %q", tc.property, got, tc.want)
		}
	}
}

func TestSamplingTextCollectsContentParts(t *testing.T) {
	single, _ := json.Marshal(map[string]any{"type": "text", "text": "one"})
	if got := samplingText(single); got != "one" {
		t.Fatalf("unexpected single text part: %q", got)
	}
	multiple, _ := json.Marshal([]map[string]any{
		{"type": "text", "text": "one"},
		{"type": "image", "data": "..."},
		{"type": "text", "text": "two"},
	})
	if got := samplingText(multiple); got != "one\ntwo" {
		t.Fatalf("unexpected multi-part text: %q", got)
	}
}
