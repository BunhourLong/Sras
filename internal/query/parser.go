// Package query parses filters ($eq, $gt, $lt, $in), plans them and evaluates
// them against documents.
package query

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Operator is a comparison in a filter.
type Operator string

const (
	OpEq Operator = "$eq"
	OpGt Operator = "$gt"
	OpLt Operator = "$lt"
	OpIn Operator = "$in"
)

// Condition is one operator applied to one value. Values holds the candidates
// for OpIn; Value holds the operand for every other operator.
type Condition struct {
	Op     Operator
	Value  any
	Values []any
}

// FieldCondition binds a condition to a document field.
type FieldCondition struct {
	Field     string
	Condition Condition
}

// Filter is a set of field conditions, ANDed together.
type Filter struct {
	Conditions []FieldCondition
}

// FindRequest is the body of POST /db/{collection}/_find.
type FindRequest struct {
	Filter json.RawMessage `json:"filter"`
	Limit  int             `json:"limit"`
	Skip   int             `json:"skip"`
}

// ErrInvalidFilter is the sentinel wrapped by every error Parse returns, so
// callers can classify a failure with errors.Is without matching on messages.
var ErrInvalidFilter = errors.New("query: invalid filter")

// Parse turns a JSON filter document into a validated Filter. Each top-level
// key is a field path; its value is either a literal (one $eq condition) or an
// operator object whose keys all start with $ ($eq, $gt, $lt, $in). The
// returned conditions are sorted by field, then by operator, so the output is
// deterministic. Empty or whitespace-only input, JSON null, and {} all parse
// to an empty Filter, which matches every document. Every error, including
// malformed JSON, wraps ErrInvalidFilter.
func Parse(raw json.RawMessage) (Filter, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return Filter{}, nil
	}
	// UseNumber keeps numbers as json.Number, so large integers survive
	// without float64 rounding.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return Filter{}, fmt.Errorf("invalid filter JSON (%v): %w", err, ErrInvalidFilter)
	}
	if err := dec.Decode(&v); err != io.EOF {
		return Filter{}, fmt.Errorf("unexpected data after the filter: %w", ErrInvalidFilter)
	}
	if v == nil { // JSON null matches everything.
		return Filter{}, nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return Filter{}, fmt.Errorf("filter must be a JSON object, got %T: %w", v, ErrInvalidFilter)
	}
	// shortcut: duplicate JSON keys resolve to the later one, because the
	// decoder fills a map; upgrading to duplicate detection would need
	// json.Decoder token-level parsing.
	fields := make([]string, 0, len(obj))
	for f := range obj {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	var conds []FieldCondition
	for _, field := range fields {
		c, err := parseField(field, obj[field])
		if err != nil {
			return Filter{}, err
		}
		conds = append(conds, c...)
	}
	// Fields and each field's operators are sorted above, so conds is ordered.
	return Filter{Conditions: conds}, nil
}

// parseField validates one field path and turns its value into the field's
// conditions: a literal becomes one $eq, an all-$ object becomes one condition
// per operator, and an empty or non-$ object becomes one $eq with the object
// itself as an exact-match literal.
func parseField(field string, value any) ([]FieldCondition, error) {
	if field == "" {
		return nil, fmt.Errorf("empty field path: %w", ErrInvalidFilter)
	}
	if strings.HasPrefix(field, "$") {
		return nil, fmt.Errorf("unsupported top-level operator %s: %w", field, ErrInvalidFilter)
	}
	for _, seg := range strings.Split(field, ".") {
		if seg == "" {
			return nil, fmt.Errorf("empty path segment in field %s: %w", field, ErrInvalidFilter)
		}
	}
	val, ok := value.(map[string]any)
	if !ok {
		return []FieldCondition{eq(field, value)}, nil
	}
	dollar, total := 0, len(val)
	for k := range val {
		if strings.HasPrefix(k, "$") {
			dollar++
		}
	}
	if total == 0 || dollar == 0 {
		return []FieldCondition{eq(field, val)}, nil
	}
	if dollar != total {
		return nil, fmt.Errorf("mixed operators and fields at field %s: %w", field, ErrInvalidFilter)
	}
	ops := make([]string, 0, total)
	for k := range val {
		ops = append(ops, k)
	}
	sort.Strings(ops)
	conds := make([]FieldCondition, 0, total)
	for _, op := range ops {
		operand := val[op]
		switch Operator(op) {
		case OpEq, OpGt, OpLt:
			conds = append(conds, FieldCondition{Field: field, Condition: Condition{Op: Operator(op), Value: operand}})
		case OpIn:
			arr, ok := operand.([]any)
			if !ok {
				return nil, fmt.Errorf("$in at field %s needs an array: %w", field, ErrInvalidFilter)
			}
			conds = append(conds, FieldCondition{Field: field, Condition: Condition{Op: OpIn, Values: arr}})
		default:
			return nil, fmt.Errorf("unknown operator %s at field %s: %w", op, field, ErrInvalidFilter)
		}
	}
	return conds, nil
}

// eq builds the single $eq condition for a field compared to a literal value.
func eq(field string, value any) FieldCondition {
	return FieldCondition{Field: field, Condition: Condition{Op: OpEq, Value: value}}
}
