package query

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// fc builds a FieldCondition that carries a single operand in Value.
func fc(field string, op Operator, value any) FieldCondition {
	return FieldCondition{Field: field, Condition: Condition{Op: op, Value: value}}
}

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    Filter
		wantErr string // substring of the error message; empty means no error
	}{
		{name: "empty", raw: "", want: Filter{}},
		{name: "whitespace", raw: "   ", want: Filter{}},
		{name: "null", raw: "null", want: Filter{}},
		{name: "empty object", raw: "{}", want: Filter{}},
		{name: "eq number", raw: `{"age":30}`, want: Filter{Conditions: []FieldCondition{fc("age", OpEq, json.Number("30"))}}},
		{name: "big number keeps precision", raw: `{"n":12345678901234567890}`, want: Filter{Conditions: []FieldCondition{fc("n", OpEq, json.Number("12345678901234567890"))}}},
		{name: "gt and lt sorted by op", raw: `{"age":{"$lt":9,"$gt":1}}`, want: Filter{Conditions: []FieldCondition{fc("age", OpGt, json.Number("1")), fc("age", OpLt, json.Number("9"))}}},
		{name: "in", raw: `{"role":{"$in":["a","b"]}}`, want: Filter{Conditions: []FieldCondition{{Field: "role", Condition: Condition{Op: OpIn, Values: []any{"a", "b"}}}}}},
		{name: "dotted field and null sorted by field", raw: `{"address.city":"Paris","a":null}`, want: Filter{Conditions: []FieldCondition{fc("a", OpEq, nil), fc("address.city", OpEq, "Paris")}}},
		{name: "array eq", raw: `{"tags":["go","db"]}`, want: Filter{Conditions: []FieldCondition{fc("tags", OpEq, []any{"go", "db"})}}},
		{name: "object literal eq", raw: `{"meta":{"k":1}}`, want: Filter{Conditions: []FieldCondition{fc("meta", OpEq, map[string]any{"k": json.Number("1")})}}},
		{name: "empty object literal eq", raw: `{"meta":{}}`, want: Filter{Conditions: []FieldCondition{fc("meta", OpEq, map[string]any{})}}},
		// Errors.
		{name: "top-level array", raw: `[1]`, wantErr: "filter must be a JSON object"},
		{name: "top-level string", raw: `"x"`, wantErr: "filter must be a JSON object"},
		{name: "unknown operator", raw: `{"age":{"$foo":1}}`, wantErr: "unknown operator $foo at field age"},
		{name: "in needs array", raw: `{"age":{"$in":3}}`, wantErr: "$in at field age needs an array"},
		{name: "top-level operator", raw: `{"$or":[]}`, wantErr: "unsupported top-level operator $or"},
		{name: "empty field", raw: `{"":1}`, wantErr: "empty field path"},
		{name: "empty middle segment", raw: `{"a..b":1}`, wantErr: "empty path segment in field a..b"},
		{name: "empty first segment", raw: `{".a":1}`, wantErr: "empty path segment in field .a"},
		{name: "empty last segment", raw: `{"a.":1}`, wantErr: "empty path segment in field a."},
		{name: "mixed operators and fields", raw: `{"age":{"$gt":1,"x":2}}`, wantErr: "mixed operators and fields at field age"},
		{name: "trailing data", raw: `{"a":1} {"b":2}`, wantErr: "unexpected data after the filter"},
		{name: "syntax error", raw: `{"a":`, wantErr: "invalid filter JSON"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(json.RawMessage(tt.raw))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Parse(%q) error = %v, want nil", tt.raw, err)
				}
				if !reflect.DeepEqual(got, tt.want) {
					t.Fatalf("Parse(%q) = %#v, want %#v", tt.raw, got, tt.want)
				}
				return
			}
			if !errors.Is(err, ErrInvalidFilter) {
				t.Fatalf("Parse(%q) error = %v, want ErrInvalidFilter", tt.raw, err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Parse(%q) error = %q, want message containing %q", tt.raw, err, tt.wantErr)
			}
		})
	}
}
