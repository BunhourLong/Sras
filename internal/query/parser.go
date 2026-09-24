// Package query parses filters ($eq, $gt, $lt, $in), plans them and evaluates
// them against documents.
package query

import "encoding/json"

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
