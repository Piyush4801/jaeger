// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package tracestore

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"

	expression "github.com/jaegertracing/jaeger-idl/query/expression/v1"
	"github.com/jaegertracing/jaeger/internal/jptrace"
	"github.com/jaegertracing/jaeger/internal/storage/v2/api/tracestore"
)

// FilterCapabilities declares the part of the RFC 0005 filter model this reader evaluates.
func FilterCapabilities() tracestore.FilterCapabilities {
	return tracestore.FilterCapabilities{
		Levels: []expression.Level{
			expression.LevelSpan,
			expression.LevelResource,
			expression.LevelScope,
			expression.LevelEvent,
			expression.LevelLink,
		},
		Operators: []expression.Operator{
			expression.OpAnd,
			expression.OpOr,
			expression.OpNot,
			expression.OpEq,
			expression.OpNe,
			expression.OpGt,
			expression.OpLt,
			expression.OpGte,
			expression.OpLte,
			expression.OpRegex,
			expression.OpExists,
			expression.OpIn,
			expression.OpNotIn,
		},
	}
}

type reference struct {
	name      string
	level     expression.Level
	attribute bool
}

func asReference(term expression.Expression) (reference, bool) {
	switch ref := term.(type) {
	case *expression.AttributeRef:
		if ref != nil {
			return reference{name: ref.Key, level: ref.Level, attribute: true}, true
		}
	case *expression.FieldRef:
		if ref != nil {
			return reference{name: ref.Name, level: ref.Level}, true
		}
	default:
	}
	return reference{}, false
}

func (r reference) isField(level expression.Level, name string) bool {
	return !r.attribute && r.level == level && r.name == name
}

func extractAttributeKeys(predicate *expression.Call) []string {
	if predicate == nil {
		return nil
	}
	var keys []string
	seen := make(map[string]bool)
	var walk func(term expression.Expression)
	walk = func(term expression.Expression) {
		if term == nil {
			return
		}
		switch n := term.(type) {
		case *expression.Call:
			if n == nil {
				return
			}
			for _, arg := range n.Args {
				walk(arg)
			}
		case *expression.AttributeRef:
			if n != nil && n.Key != "" && !seen[n.Key] {
				seen[n.Key] = true
				keys = append(keys, n.Key)
			}
		default:
		}
	}
	walk(predicate)
	return keys
}

func buildFilterQuery(predicate *expression.Call, meta attributeMetadata) (string, []any, error) {
	switch predicate.Op {
	case expression.OpAnd, expression.OpOr:
		if len(predicate.Args) == 0 {
			return "", nil, errArity(predicate)
		}
		var parts []string
		var args []any
		for _, arg := range predicate.Args {
			call, ok := arg.(*expression.Call)
			if !ok {
				return "", nil, fmt.Errorf("%w: %q combines predicates, not values",
					tracestore.ErrFilterInvalid, predicate.Op)
			}
			sqlPart, partArgs, err := buildFilterQuery(call, meta)
			if err != nil {
				return "", nil, err
			}
			parts = append(parts, sqlPart)
			args = append(args, partArgs...)
		}
		if len(parts) == 1 {
			return parts[0], args, nil
		}
		joiner := " AND "
		if predicate.Op == expression.OpOr {
			joiner = " OR "
		}
		return "(" + strings.Join(parts, joiner) + ")", args, nil

	case expression.OpNot:
		if len(predicate.Args) != 1 {
			return "", nil, errArity(predicate)
		}
		call, ok := predicate.Args[0].(*expression.Call)
		if !ok {
			return "", nil, fmt.Errorf("%w: %q negates a predicate, not a value",
				tracestore.ErrFilterInvalid, predicate.Op)
		}
		sqlPart, partArgs, err := buildFilterQuery(call, meta)
		if err != nil {
			return "", nil, err
		}
		return "NOT (" + sqlPart + ")", partArgs, nil

	case expression.OpEq, expression.OpRegex,
		expression.OpGt, expression.OpLt, expression.OpGte, expression.OpLte:
		ref, value, err := refAndConstantArgs(predicate)
		if err != nil {
			return "", nil, err
		}
		return buildComparison(predicate.Op, ref, value, meta)

	case expression.OpNe:
		ref, value, err := refAndConstantArgs(predicate)
		if err != nil {
			return "", nil, err
		}
		presentSQL, presentArgs, err := buildExists(ref, meta)
		if err != nil {
			return "", nil, err
		}
		equalSQL, equalArgs, err := buildComparison(expression.OpEq, ref, value, meta)
		if err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("(%s AND NOT (%s))", presentSQL, equalSQL), append(presentArgs, equalArgs...), nil

	case expression.OpIn, expression.OpNotIn:
		ref, list, err := refAndListArgs(predicate)
		if err != nil {
			return "", nil, err
		}
		return buildMembership(predicate.Op, ref, list, meta)

	case expression.OpExists:
		ref, err := refArg(predicate)
		if err != nil {
			return "", nil, err
		}
		return buildExists(ref, meta)

	default:
		return "", nil, fmt.Errorf("%w: it does not support the operator %q",
			tracestore.ErrFilterUnsupported, predicate.Op)
	}
}

func buildMembership(op expression.Operator, ref reference, list *expression.List, meta attributeMetadata) (string, []any, error) {
	if len(list.Values) == 0 {
		return "", nil, fmt.Errorf("%w: %q compares against an empty list",
			tracestore.ErrFilterInvalid, op)
	}
	presentSQL, presentArgs, err := buildExists(ref, meta)
	if err != nil {
		return "", nil, err
	}

	var fieldType expression.FieldType
	if !ref.attribute {
		if field, found := expression.LookupField(ref.level, ref.name); found {
			fieldType = field.Type
		}
	}

	var memberSQLs []string
	var memberArgs []any
	for _, val := range list.Values {
		element, err := tracestore.ReadFilterElement(list, fieldType, val)
		if err != nil {
			return "", nil, fmt.Errorf("%w: %w", tracestore.ErrFilterInvalid, err)
		}
		sqlPart, partArgs, err := buildComparison(expression.OpEq, ref, element, meta)
		if err != nil {
			return "", nil, err
		}
		memberSQLs = append(memberSQLs, sqlPart)
		memberArgs = append(memberArgs, partArgs...)
	}

	disjunction := "(" + strings.Join(memberSQLs, " OR ") + ")"
	if op == expression.OpIn {
		return disjunction, memberArgs, nil
	}
	return fmt.Sprintf("(%s AND NOT %s)", presentSQL, disjunction), append(presentArgs, memberArgs...), nil
}

func buildComparison(op expression.Operator, ref reference, value expression.Expression, meta attributeMetadata) (string, []any, error) {
	if ref.isField(expression.LevelSpan, expression.SpanFieldDuration) {
		duration, err := lengthOfTime(value)
		if err != nil {
			return "", nil, err
		}
		sqlOp, err := sqlComparisonOp(op)
		if err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("s.duration %s ?", sqlOp), []any{duration.Nanoseconds()}, nil
	}

	if ref.attribute {
		if _, isString := value.(*expression.StringValue); isString && ordersValues(op) {
			return "", nil, errOrderedString(op, ref)
		}
		return buildAttributeComparison(op, ref, value, meta)
	}

	text, err := constantText(value)
	if err != nil {
		return "", nil, err
	}

	switch {
	case ref.isField(expression.LevelSpan, expression.SpanFieldName):
		if op == expression.OpRegex {
			return "match(s.name, ?)", []any{text}, nil
		}
		sqlOp, err := sqlComparisonOp(op)
		if err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("s.name %s ?", sqlOp), []any{text}, nil

	case ref.isField(expression.LevelResource, expression.ResourceFieldService):
		if op == expression.OpRegex {
			return "match(s.service_name, ?)", []any{text}, nil
		}
		sqlOp, err := sqlComparisonOp(op)
		if err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("s.service_name %s ?", sqlOp), []any{text}, nil

	case ref.isField(expression.LevelSpan, expression.SpanFieldTraceID):
		if ordersValues(op) || op == expression.OpRegex {
			return "", nil, errUnorderedValue(op, ref)
		}
		sqlOp, err := sqlComparisonOp(op)
		if err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("s.trace_id %s ?", sqlOp), []any{strings.ToLower(text)}, nil

	case ref.isField(expression.LevelSpan, expression.SpanFieldSpanID):
		if ordersValues(op) || op == expression.OpRegex {
			return "", nil, errUnorderedValue(op, ref)
		}
		sqlOp, err := sqlComparisonOp(op)
		if err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("s.id %s ?", sqlOp), []any{strings.ToLower(text)}, nil

	case ref.isField(expression.LevelSpan, expression.SpanFieldKind):
		if ordersValues(op) {
			return "", nil, errUnorderedValue(op, ref)
		}
		if op == expression.OpRegex {
			return "match(s.kind, ?)", []any{text}, nil
		}
		sqlOp, err := sqlComparisonOp(op)
		if err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("s.kind %s ?", sqlOp), []any{normalizeSpanKind(text)}, nil

	case ref.isField(expression.LevelSpan, expression.SpanFieldStatus):
		if ordersValues(op) {
			return "", nil, errUnorderedValue(op, ref)
		}
		if op == expression.OpRegex {
			return "match(s.status_code, ?)", []any{text}, nil
		}
		sqlOp, err := sqlComparisonOp(op)
		if err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("s.status_code %s ?", sqlOp), []any{normalizeSpanStatus(text)}, nil

	case ref.isField(expression.LevelScope, expression.ScopeFieldName):
		if op == expression.OpRegex {
			return "match(s.scope_name, ?)", []any{text}, nil
		}
		sqlOp, err := sqlComparisonOp(op)
		if err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("s.scope_name %s ?", sqlOp), []any{text}, nil

	case ref.isField(expression.LevelScope, expression.ScopeFieldVersion):
		if op == expression.OpRegex {
			return "match(s.scope_version, ?)", []any{text}, nil
		}
		sqlOp, err := sqlComparisonOp(op)
		if err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("s.scope_version %s ?", sqlOp), []any{text}, nil

	case ref.isField(expression.LevelEvent, expression.EventFieldName):
		if op == expression.OpEq {
			return "arrayExists(x -> x.name = ?, s.events)", []any{text}, nil
		}
		if op == expression.OpRegex {
			return "arrayExists(x -> match(x.name, ?), s.events)", []any{text}, nil
		}
		sqlOp, err := sqlComparisonOp(op)
		if err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("arrayExists(x -> x.name %s ?, s.events)", sqlOp), []any{text}, nil

	default:
		return "", nil, errUnsupportedField(ref)
	}
}

func buildAttributeComparison(op expression.Operator, ref reference, value expression.Expression, meta attributeMetadata) (string, []any, error) {
	levels, err := targetLevels(ref.level)
	if err != nil {
		return "", nil, err
	}

	var clauses []string
	var args []any

	switch v := value.(type) {
	case *expression.IntValue:
		for _, lvl := range levels {
			clause, clauseArgs, err := buildSingleAttributeCondition(lvl, ref.name, pcommon.ValueTypeInt, v.Value, op)
			if err != nil {
				return "", nil, err
			}
			clauses = append(clauses, clause)
			args = append(args, clauseArgs...)
		}

	case *expression.DoubleValue:
		for _, lvl := range levels {
			clause, clauseArgs, err := buildSingleAttributeCondition(lvl, ref.name, pcommon.ValueTypeDouble, v.Value, op)
			if err != nil {
				return "", nil, err
			}
			clauses = append(clauses, clause)
			args = append(args, clauseArgs...)
		}

	case *expression.BoolValue:
		if ordersValues(op) {
			return "", nil, errUnorderedValue(op, ref)
		}
		for _, lvl := range levels {
			clause, clauseArgs, err := buildSingleAttributeCondition(lvl, ref.name, pcommon.ValueTypeBool, v.Value, op)
			if err != nil {
				return "", nil, err
			}
			clauses = append(clauses, clause)
			args = append(args, clauseArgs...)
		}

	case *expression.StringValue, *expression.AnyValue:
		text, err := constantText(value)
		if err != nil {
			return "", nil, err
		}

		attrMetaTypes := meta[ref.name]
		for _, lvl := range levels {
			types := attrMetaTypes.forLevel(lvl)
			lvlClauses, lvlArgs, err := buildStringAttributeComparisonForLevel(lvl, ref, text, op, types)
			if err != nil {
				return "", nil, err
			}
			clauses = append(clauses, lvlClauses...)
			args = append(args, lvlArgs...)
		}

	default:
		return "", nil, errTypedConstant(value)
	}

	if len(clauses) == 0 {
		return "", nil, fmt.Errorf("%w: no conditions generated for attribute %q", tracestore.ErrFilterInvalid, ref.name)
	}
	if len(clauses) == 1 {
		return clauses[0], args, nil
	}
	return "(" + strings.Join(clauses, " OR ") + ")", args, nil
}

func buildStringAttributeComparisonForLevel(
	lvl expression.Level,
	ref reference,
	text string,
	op expression.Operator,
	types []pcommon.ValueType,
) ([]string, []any, error) {
	if ordersValues(op) {
		return buildOrderedStringAttribute(lvl, ref, text, op, types)
	}
	return buildUnorderedStringAttribute(lvl, ref, text, op, types)
}

func buildOrderedStringAttribute(
	lvl expression.Level,
	ref reference,
	text string,
	op expression.Operator,
	types []pcommon.ValueType,
) ([]string, []any, error) {
	if len(types) > 0 {
		var hasNumeric bool
		for _, t := range types {
			if t == pcommon.ValueTypeInt || t == pcommon.ValueTypeDouble {
				hasNumeric = true
				break
			}
		}
		if !hasNumeric {
			return nil, nil, errUnorderedValue(op, ref)
		}
		var clauses []string
		var args []any
		for _, t := range types {
			switch t {
			case pcommon.ValueTypeInt:
				if i, err := strconv.ParseInt(text, 10, 64); err == nil {
					clause, clauseArgs, err := buildSingleAttributeCondition(lvl, ref.name, pcommon.ValueTypeInt, i, op)
					if err != nil {
						return nil, nil, err
					}
					clauses = append(clauses, clause)
					args = append(args, clauseArgs...)
				}
			case pcommon.ValueTypeDouble:
				if f, err := strconv.ParseFloat(text, 64); err == nil {
					clause, clauseArgs, err := buildSingleAttributeCondition(lvl, ref.name, pcommon.ValueTypeDouble, f, op)
					if err != nil {
						return nil, nil, err
					}
					clauses = append(clauses, clause)
					args = append(args, clauseArgs...)
				}
			default:
			}
		}
		if len(clauses) == 0 {
			return nil, nil, errNotANumber(op, ref, text)
		}
		return clauses, args, nil
	}

	i, errInt := strconv.ParseInt(text, 10, 64)
	f, errFloat := strconv.ParseFloat(text, 64)
	if errInt != nil && errFloat != nil {
		return nil, nil, errNotANumber(op, ref, text)
	}
	if errInt == nil {
		clause, clauseArgs, err := buildSingleAttributeCondition(lvl, ref.name, pcommon.ValueTypeInt, i, op)
		if err != nil {
			return nil, nil, err
		}
		return []string{clause}, clauseArgs, nil
	}
	clause, clauseArgs, err := buildSingleAttributeCondition(lvl, ref.name, pcommon.ValueTypeDouble, f, op)
	if err != nil {
		return nil, nil, err
	}
	return []string{clause}, clauseArgs, nil
}

func buildUnorderedStringAttribute(
	lvl expression.Level,
	ref reference,
	text string,
	op expression.Operator,
	types []pcommon.ValueType,
) ([]string, []any, error) {
	if len(types) == 0 {
		clause, clauseArgs, err := buildSingleAttributeCondition(lvl, ref.name, pcommon.ValueTypeStr, text, op)
		if err != nil {
			return nil, nil, err
		}
		return []string{clause}, clauseArgs, nil
	}

	var clauses []string
	var args []any
	for _, t := range types {
		switch t {
		case pcommon.ValueTypeInt:
			if i, err := strconv.ParseInt(text, 10, 64); err == nil {
				clause, clauseArgs, err := buildSingleAttributeCondition(lvl, ref.name, pcommon.ValueTypeInt, i, op)
				if err != nil {
					return nil, nil, err
				}
				clauses = append(clauses, clause)
				args = append(args, clauseArgs...)
			}
		case pcommon.ValueTypeDouble:
			if f, err := strconv.ParseFloat(text, 64); err == nil {
				clause, clauseArgs, err := buildSingleAttributeCondition(lvl, ref.name, pcommon.ValueTypeDouble, f, op)
				if err != nil {
					return nil, nil, err
				}
				clauses = append(clauses, clause)
				args = append(args, clauseArgs...)
			}
		case pcommon.ValueTypeBool:
			if b, err := strconv.ParseBool(text); err == nil {
				clause, clauseArgs, err := buildSingleAttributeCondition(lvl, ref.name, pcommon.ValueTypeBool, b, op)
				if err != nil {
					return nil, nil, err
				}
				clauses = append(clauses, clause)
				args = append(args, clauseArgs...)
			}
		default:
			clause, clauseArgs, err := buildSingleAttributeCondition(lvl, ref.name, t, text, op)
			if err != nil {
				return nil, nil, err
			}
			clauses = append(clauses, clause)
			args = append(args, clauseArgs...)
		}
	}

	if len(clauses) == 0 {
		clause, clauseArgs, err := buildSingleAttributeCondition(lvl, ref.name, pcommon.ValueTypeStr, text, op)
		if err != nil {
			return nil, nil, err
		}
		return []string{clause}, clauseArgs, nil
	}
	return clauses, args, nil
}

func buildExists(ref reference, meta attributeMetadata) (string, []any, error) {
	if ref.attribute {
		levels, err := targetLevels(ref.level)
		if err != nil {
			return "", nil, err
		}
		var clauses []string
		var args []any
		attrMetaTypes := meta[ref.name]

		for _, lvl := range levels {
			types := attrMetaTypes.forLevel(lvl)
			if len(types) > 0 {
				for _, t := range types {
					clause, clauseArgs := buildSingleAttributeExists(lvl, ref.name, t)
					clauses = append(clauses, clause)
					args = append(args, clauseArgs...)
				}
			} else {
				clause, clauseArgs := buildSingleAttributeExists(lvl, ref.name, pcommon.ValueTypeStr)
				clauses = append(clauses, clause)
				args = append(args, clauseArgs...)
			}
		}

		if len(clauses) == 1 {
			return clauses[0], args, nil
		}
		return "(" + strings.Join(clauses, " OR ") + ")", args, nil
	}

	switch {
	case ref.isField(expression.LevelSpan, expression.SpanFieldName):
		return "s.name != ''", nil, nil
	case ref.isField(expression.LevelResource, expression.ResourceFieldService):
		return "s.service_name != ''", nil, nil
	case ref.isField(expression.LevelSpan, expression.SpanFieldDuration):
		return "s.duration > 0", nil, nil
	case ref.isField(expression.LevelSpan, expression.SpanFieldTraceID):
		return "s.trace_id != ''", nil, nil
	case ref.isField(expression.LevelSpan, expression.SpanFieldSpanID):
		return "s.id != ''", nil, nil
	case ref.isField(expression.LevelSpan, expression.SpanFieldKind):
		return "s.kind != ''", nil, nil
	case ref.isField(expression.LevelSpan, expression.SpanFieldStatus):
		return "s.status_code != ''", nil, nil
	case ref.isField(expression.LevelScope, expression.ScopeFieldName):
		return "s.scope_name != ''", nil, nil
	case ref.isField(expression.LevelScope, expression.ScopeFieldVersion):
		return "s.scope_version != ''", nil, nil
	case ref.isField(expression.LevelEvent, expression.EventFieldName):
		return "arrayExists(x -> x.name != '', s.events)", nil, nil
	default:
		return "", nil, errUnsupportedField(ref)
	}
}

func targetLevels(lvl expression.Level) ([]expression.Level, error) {
	switch lvl {
	case expression.LevelSpan, expression.LevelResource, expression.LevelScope, expression.LevelEvent, expression.LevelLink:
		return []expression.Level{lvl}, nil
	case "":
		return []expression.Level{expression.LevelSpan, expression.LevelResource}, nil
	default:
		return nil, errUnsupportedLevel(lvl)
	}
}

func buildSingleAttributeCondition(lvl expression.Level, key string, valType pcommon.ValueType, val any, op expression.Operator) (string, []any, error) {
	colType := jptrace.ValueTypeToString(valType)
	if valType == pcommon.ValueTypeBytes || valType == pcommon.ValueTypeMap || valType == pcommon.ValueTypeSlice {
		colType = "complex"
	}

	if op == expression.OpRegex {
		switch lvl {
		case expression.LevelSpan:
			return fmt.Sprintf("arrayExists((key, value) -> key = ? AND match(value, ?), s.%s_attributes.key, s.%s_attributes.value)", colType, colType), []any{key, val}, nil
		case expression.LevelResource:
			return fmt.Sprintf("arrayExists((key, value) -> key = ? AND match(value, ?), s.resource_%s_attributes.key, s.resource_%s_attributes.value)", colType, colType), []any{key, val}, nil
		case expression.LevelScope:
			return fmt.Sprintf("arrayExists((key, value) -> key = ? AND match(value, ?), s.scope_%s_attributes.key, s.scope_%s_attributes.value)", colType, colType), []any{key, val}, nil
		case expression.LevelEvent:
			return fmt.Sprintf("arrayExists(x -> arrayExists((key, value) -> key = ? AND match(value, ?), x.%s_attributes.key, x.%s_attributes.value), s.events)", colType, colType), []any{key, val}, nil
		case expression.LevelLink:
			return fmt.Sprintf("arrayExists(x -> arrayExists((key, value) -> key = ? AND match(value, ?), x.%s_attributes.key, x.%s_attributes.value), s.links)", colType, colType), []any{key, val}, nil
		default:
			return "", nil, errUnsupportedLevel(lvl)
		}
	}

	sqlOp, err := sqlComparisonOp(op)
	if err != nil {
		return "", nil, err
	}

	switch lvl {
	case expression.LevelSpan:
		return fmt.Sprintf("arrayExists((key, value) -> key = ? AND value %s ?, s.%s_attributes.key, s.%s_attributes.value)", sqlOp, colType, colType), []any{key, val}, nil
	case expression.LevelResource:
		return fmt.Sprintf("arrayExists((key, value) -> key = ? AND value %s ?, s.resource_%s_attributes.key, s.resource_%s_attributes.value)", sqlOp, colType, colType), []any{key, val}, nil
	case expression.LevelScope:
		return fmt.Sprintf("arrayExists((key, value) -> key = ? AND value %s ?, s.scope_%s_attributes.key, s.scope_%s_attributes.value)", sqlOp, colType, colType), []any{key, val}, nil
	case expression.LevelEvent:
		return fmt.Sprintf("arrayExists(x -> arrayExists((key, value) -> key = ? AND value %s ?, x.%s_attributes.key, x.%s_attributes.value), s.events)", sqlOp, colType, colType), []any{key, val}, nil
	case expression.LevelLink:
		return fmt.Sprintf("arrayExists(x -> arrayExists((key, value) -> key = ? AND value %s ?, x.%s_attributes.key, x.%s_attributes.value), s.links)", sqlOp, colType, colType), []any{key, val}, nil
	default:
		return "", nil, errUnsupportedLevel(lvl)
	}
}

func buildSingleAttributeExists(lvl expression.Level, key string, valType pcommon.ValueType) (string, []any) {
	colType := jptrace.ValueTypeToString(valType)
	if valType == pcommon.ValueTypeBytes || valType == pcommon.ValueTypeMap || valType == pcommon.ValueTypeSlice {
		colType = "complex"
	}

	switch lvl {
	case expression.LevelSpan:
		return fmt.Sprintf("has(s.%s_attributes.key, ?)", colType), []any{key}
	case expression.LevelResource:
		return fmt.Sprintf("has(s.resource_%s_attributes.key, ?)", colType), []any{key}
	case expression.LevelScope:
		return fmt.Sprintf("has(s.scope_%s_attributes.key, ?)", colType), []any{key}
	case expression.LevelEvent:
		return fmt.Sprintf("arrayExists(x -> has(x.%s_attributes.key, ?), s.events)", colType), []any{key}
	case expression.LevelLink:
		return fmt.Sprintf("arrayExists(x -> has(x.%s_attributes.key, ?), s.links)", colType), []any{key}
	default:
		return "", nil
	}
}

func (a attrTypes) forLevel(lvl expression.Level) []pcommon.ValueType {
	switch lvl {
	case expression.LevelSpan:
		return a.span
	case expression.LevelResource:
		return a.resource
	case expression.LevelScope:
		return a.scope
	case expression.LevelEvent:
		return a.event
	case expression.LevelLink:
		return a.link
	default:
		return nil
	}
}

func sqlComparisonOp(op expression.Operator) (string, error) {
	switch op {
	case expression.OpEq:
		return "=", nil
	case expression.OpNe:
		return "!=", nil
	case expression.OpGt:
		return ">", nil
	case expression.OpLt:
		return "<", nil
	case expression.OpGte:
		return ">=", nil
	case expression.OpLte:
		return "<=", nil
	default:
		return "", fmt.Errorf("%w: operator %q is not a comparison operator", tracestore.ErrFilterUnsupported, op)
	}
}

func constantText(value expression.Expression) (string, error) {
	switch constant := value.(type) {
	case *expression.AnyValue:
		if constant != nil {
			return constant.Value, nil
		}
	case *expression.StringValue:
		if constant != nil {
			return constant.Value, nil
		}
	default:
	}
	return "", errTypedConstant(value)
}

func lengthOfTime(value expression.Expression) (time.Duration, error) {
	switch constant := value.(type) {
	case *expression.DurationValue:
		if constant != nil {
			return constant.Value, nil
		}
	case *expression.AnyValue:
		if constant == nil {
			break
		}
		read, err := tracestore.ReadFilterConstant(expression.FieldTypeDuration, constant.Value)
		if err != nil {
			return 0, fmt.Errorf(`%w: %q is not a duration such as "2s": %w`,
				tracestore.ErrFilterInvalid, constant.Value, err)
		}
		if duration, ok := read.(*expression.DurationValue); ok {
			return duration.Value, nil
		}
	default:
	}
	return 0, errTypedConstant(value)
}

func normalizeSpanKind(k string) string {
	switch strings.ToLower(k) {
	case "server":
		return "Server"
	case "client":
		return "Client"
	case "producer":
		return "Producer"
	case "consumer":
		return "Consumer"
	case "internal":
		return "Internal"
	case "unspecified":
		return "Unspecified"
	default:
		return k
	}
}

func normalizeSpanStatus(s string) string {
	switch strings.ToLower(s) {
	case "ok":
		return "Ok"
	case "error":
		return "Error"
	case "unset":
		return "Unset"
	default:
		return s
	}
}

func refArg(predicate *expression.Call) (reference, error) {
	if len(predicate.Args) != 1 {
		return reference{}, errArity(predicate)
	}
	ref, ok := asReference(predicate.Args[0])
	if !ok {
		return reference{}, fmt.Errorf("%w: %q reads a value on the span",
			tracestore.ErrFilterInvalid, predicate.Op)
	}
	return ref, nil
}

func refAndConstantArgs(predicate *expression.Call) (reference, expression.Expression, error) {
	if len(predicate.Args) != 2 {
		return reference{}, nil, errArity(predicate)
	}
	ref, refOK := asReference(predicate.Args[0])
	if !refOK || isReference(predicate.Args[1]) {
		return reference{}, nil, errRefAgainstConstant(predicate)
	}
	return ref, predicate.Args[1], nil
}

func refAndListArgs(predicate *expression.Call) (reference, *expression.List, error) {
	if len(predicate.Args) != 2 {
		return reference{}, nil, errArity(predicate)
	}
	ref, refOK := asReference(predicate.Args[0])
	list, listOK := predicate.Args[1].(*expression.List)
	if !refOK || !listOK || list == nil {
		return reference{}, nil, errRefAgainstConstant(predicate)
	}
	return ref, list, nil
}

func isReference(term expression.Expression) bool {
	_, ok := asReference(term)
	return ok
}

func errArity(predicate *expression.Call) error {
	return fmt.Errorf("%w: %q cannot take %d arguments",
		tracestore.ErrFilterInvalid, predicate.Op, len(predicate.Args))
}

func errRefAgainstConstant(predicate *expression.Call) error {
	return fmt.Errorf("%w: it evaluates %q against a constant only",
		tracestore.ErrFilterUnsupported, predicate.Op)
}

func errUnsupportedLevel(level expression.Level) error {
	return fmt.Errorf("%w: it does not index the %q level", tracestore.ErrFilterUnsupported, level)
}

func errUnsupportedField(ref reference) error {
	return fmt.Errorf("%w: it does not support the built-in field %q of the %q level",
		tracestore.ErrFilterUnsupported, ref.name, ref.level)
}

func ordersValues(op expression.Operator) bool {
	switch op {
	case expression.OpGt, expression.OpLt, expression.OpGte, expression.OpLte:
		return true
	default:
		return false
	}
}

func errOrderedString(op expression.Operator, ref reference) error {
	return fmt.Errorf("%w: it orders %q only as a number, so it cannot evaluate %q against a string constant",
		tracestore.ErrFilterUnsupported, ref.name, op)
}

func errUnorderedValue(op expression.Operator, ref reference) error {
	return fmt.Errorf("%w: it indexes %q as a keyword rather than a number, so it cannot evaluate %q on it",
		tracestore.ErrFilterUnsupported, ref.name, op)
}

func errNotANumber(op expression.Operator, ref reference, value string) error {
	return fmt.Errorf("%w: %q on %q compares against a number, and %q is not one",
		tracestore.ErrFilterInvalid, op, ref.name, value)
}

func errTypedConstant(value expression.Expression) error {
	return fmt.Errorf("%w: it does not support typed constants of type %T",
		tracestore.ErrFilterUnsupported, value)
}
