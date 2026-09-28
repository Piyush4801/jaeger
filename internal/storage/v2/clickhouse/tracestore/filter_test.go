// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package tracestore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"

	expression "github.com/jaegertracing/jaeger-idl/query/expression/v1"
	builder "github.com/jaegertracing/jaeger/internal/expression"
	"github.com/jaegertracing/jaeger/internal/storage/v2/api/tracestore"
)

func TestFilterCapabilities(t *testing.T) {
	caps := FilterCapabilities()
	assert.ElementsMatch(t, []expression.Level{
		expression.LevelSpan,
		expression.LevelResource,
		expression.LevelScope,
		expression.LevelEvent,
		expression.LevelLink,
	}, caps.Levels)

	assert.ElementsMatch(t, []expression.Operator{
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
	}, caps.Operators)
}

func TestBuildFilterQuery_BooleanCombinators(t *testing.T) {
	var p builder.Predicate

	t.Run("conjunction", func(t *testing.T) {
		call := p.And(
			p.Resource().Service.Eq("checkout"),
			p.Span().Name.Eq("GET /cart"),
		)
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "(s.service_name = ? AND s.name = ?)", sqlStr)
		assert.Equal(t, []any{"checkout", "GET /cart"}, args)
	})

	t.Run("disjunction", func(t *testing.T) {
		call := p.Or(
			p.Resource().Service.Eq("checkout"),
			p.Resource().Service.Eq("cart"),
		)
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "(s.service_name = ? OR s.service_name = ?)", sqlStr)
		assert.Equal(t, []any{"checkout", "cart"}, args)
	})

	t.Run("negation", func(t *testing.T) {
		call := p.Not(p.Resource().Service.Eq("checkout"))
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "NOT (s.service_name = ?)", sqlStr)
		assert.Equal(t, []any{"checkout"}, args)
	})

	t.Run("nested boolean tree", func(t *testing.T) {
		call := p.And(
			p.Resource().Service.Eq("checkout"),
			p.Or(
				p.Span().Name.Eq("GET /cart"),
				p.Not(p.Span().Name.Eq("POST /cart")),
			),
		)
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "(s.service_name = ? AND (s.name = ? OR NOT (s.name = ?)))", sqlStr)
		assert.Equal(t, []any{"checkout", "GET /cart", "POST /cart"}, args)
	})
}

func TestBuildFilterQuery_BuiltInFields(t *testing.T) {
	var p builder.Predicate

	t.Run("span.duration gt", func(t *testing.T) {
		call := p.Span().Duration.Gt(2 * time.Second)
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "s.duration > ?", sqlStr)
		assert.Equal(t, []any{int64(2000000000)}, args)
	})

	t.Run("span.duration lte", func(t *testing.T) {
		call := p.Span().Duration.Lte(5 * time.Millisecond)
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "s.duration <= ?", sqlStr)
		assert.Equal(t, []any{int64(5000000)}, args)
	})

	t.Run("span.name regex", func(t *testing.T) {
		call := p.Span().Name.Matches("cart.*")
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "match(s.name, ?)", sqlStr)
		assert.Equal(t, []any{"cart.*"}, args)
	})

	t.Run("resource.service eq", func(t *testing.T) {
		call := p.Resource().Service.Eq("order-svc")
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "s.service_name = ?", sqlStr)
		assert.Equal(t, []any{"order-svc"}, args)
	})

	t.Run("span.kind normalized", func(t *testing.T) {
		call := p.Span().Kind.Eq("server")
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "s.kind = ?", sqlStr)
		assert.Equal(t, []any{"Server"}, args)
	})

	t.Run("span.status normalized", func(t *testing.T) {
		call := p.Span().Status.Eq("error")
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "s.status_code = ?", sqlStr)
		assert.Equal(t, []any{"Error"}, args)
	})

	t.Run("span.traceID", func(t *testing.T) {
		call := p.Span().TraceID.Eq("00000000000000000000000000000F01")
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "s.trace_id = ?", sqlStr)
		assert.Equal(t, []any{"00000000000000000000000000000f01"}, args)
	})

	t.Run("event.name eq", func(t *testing.T) {
		call := p.Event().Name.Eq("exception")
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "arrayExists(x -> x.name = ?, s.events)", sqlStr)
		assert.Equal(t, []any{"exception"}, args)
	})

	t.Run("event.name regex", func(t *testing.T) {
		call := p.Event().Name.Matches("except.*")
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "arrayExists(x -> match(x.name, ?), s.events)", sqlStr)
		assert.Equal(t, []any{"except.*"}, args)
	})

	t.Run("scope.name eq", func(t *testing.T) {
		call := p.Scope().Name.Eq("tracer-lib")
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "s.scope_name = ?", sqlStr)
		assert.Equal(t, []any{"tracer-lib"}, args)
	})
}

func TestBuildFilterQuery_Attributes(t *testing.T) {
	var p builder.Predicate

	t.Run("span attribute with metadata int", func(t *testing.T) {
		meta := attributeMetadata{
			"http.status_code": attrTypes{
				span: []pcommon.ValueType{pcommon.ValueTypeInt},
			},
		}
		call := p.Span().Attr("http.status_code").Eq("500")
		sqlStr, args, err := buildFilterQuery(call, meta)
		require.NoError(t, err)
		assert.Equal(t, "arrayExists((key, value) -> key = ? AND value = ?, s.int_attributes.key, s.int_attributes.value)", sqlStr)
		assert.Equal(t, []any{"http.status_code", int64(500)}, args)
	})

	t.Run("span attribute fallback string", func(t *testing.T) {
		call := p.Span().Attr("zone").Eq("us-east")
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "arrayExists((key, value) -> key = ? AND value = ?, s.str_attributes.key, s.str_attributes.value)", sqlStr)
		assert.Equal(t, []any{"zone", "us-east"}, args)
	})

	t.Run("resource attribute", func(t *testing.T) {
		call := p.Resource().Attr("env").Eq("prod")
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "arrayExists((key, value) -> key = ? AND value = ?, s.resource_str_attributes.key, s.resource_str_attributes.value)", sqlStr)
		assert.Equal(t, []any{"env", "prod"}, args)
	})

	t.Run("scope attribute", func(t *testing.T) {
		call := p.Scope().Attr("lib.version").Eq("v1.2.3")
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "arrayExists((key, value) -> key = ? AND value = ?, s.scope_str_attributes.key, s.scope_str_attributes.value)", sqlStr)
		assert.Equal(t, []any{"lib.version", "v1.2.3"}, args)
	})

	t.Run("event attribute", func(t *testing.T) {
		call := p.Event().Attr("zone").Eq("eu-west")
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "arrayExists(x -> arrayExists((key, value) -> key = ? AND value = ?, x.str_attributes.key, x.str_attributes.value), s.events)", sqlStr)
		assert.Equal(t, []any{"zone", "eu-west"}, args)
	})

	t.Run("link attribute", func(t *testing.T) {
		call := p.Link().Attr("kind").Eq("parent")
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "arrayExists(x -> arrayExists((key, value) -> key = ? AND value = ?, x.str_attributes.key, x.str_attributes.value), s.links)", sqlStr)
		assert.Equal(t, []any{"kind", "parent"}, args)
	})

	t.Run("unqualified attribute expands to span and resource only", func(t *testing.T) {
		call := p.Attr("zone").Eq("us-east")
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "(arrayExists((key, value) -> key = ? AND value = ?, s.str_attributes.key, s.str_attributes.value) OR arrayExists((key, value) -> key = ? AND value = ?, s.resource_str_attributes.key, s.resource_str_attributes.value))", sqlStr)
		assert.Equal(t, []any{"zone", "us-east", "zone", "us-east"}, args)
	})

	t.Run("unqualified attribute exists expands to span and resource only", func(t *testing.T) {
		call := p.Attr("zone").Exists()
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "(has(s.str_attributes.key, ?) OR has(s.resource_str_attributes.key, ?))", sqlStr)
		assert.Equal(t, []any{"zone", "zone"}, args)
	})

	t.Run("attribute regex", func(t *testing.T) {
		call := p.Span().Attr("http.status_code").Matches("5..")
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "arrayExists((key, value) -> key = ? AND match(value, ?), s.str_attributes.key, s.str_attributes.value)", sqlStr)
		assert.Equal(t, []any{"http.status_code", "5.."}, args)
	})

	t.Run("attribute ne holds something else", func(t *testing.T) {
		call := p.Span().Attr("http.status_code").Ne("200")
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "(has(s.str_attributes.key, ?) AND NOT (arrayExists((key, value) -> key = ? AND value = ?, s.str_attributes.key, s.str_attributes.value)))", sqlStr)
		assert.Equal(t, []any{"http.status_code", "http.status_code", "200"}, args)
	})

	t.Run("numeric attribute comparison with typed int constant", func(t *testing.T) {
		call := p.Span().Attr("retry.count").Gt(10)
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "arrayExists((key, value) -> key = ? AND value > ?, s.int_attributes.key, s.int_attributes.value)", sqlStr)
		assert.Equal(t, []any{"retry.count", int64(10)}, args)
	})
}

func TestBuildFilterQuery_Membership(t *testing.T) {
	var p builder.Predicate

	t.Run("in on built-in field", func(t *testing.T) {
		call := p.Resource().Service.In("svc-a", "svc-b")
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "(s.service_name = ? OR s.service_name = ?)", sqlStr)
		assert.Equal(t, []any{"svc-a", "svc-b"}, args)
	})

	t.Run("not in on built-in field", func(t *testing.T) {
		call := p.Resource().Service.NotIn("svc-a", "svc-b")
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "(s.service_name != '' AND NOT (s.service_name = ? OR s.service_name = ?))", sqlStr)
		assert.Equal(t, []any{"svc-a", "svc-b"}, args)
	})

	t.Run("in on attribute", func(t *testing.T) {
		call := p.Span().Attr("env").In("dev", "stage")
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "(arrayExists((key, value) -> key = ? AND value = ?, s.str_attributes.key, s.str_attributes.value) OR arrayExists((key, value) -> key = ? AND value = ?, s.str_attributes.key, s.str_attributes.value))", sqlStr)
		assert.Equal(t, []any{"env", "dev", "env", "stage"}, args)
	})
}

func TestBuildFilterQuery_Refusals(t *testing.T) {
	var p builder.Predicate

	t.Run("some quantifier is refused", func(t *testing.T) {
		call := p.Some(p.Event(), p.Event().Name.Eq("exception"))
		_, _, err := buildFilterQuery(call, nil)
		require.ErrorIs(t, err, tracestore.ErrFilterUnsupported)
		assert.Contains(t, err.Error(), "some")
	})

	t.Run("unsupported level is refused", func(t *testing.T) {
		call := &expression.Call{
			Op: expression.OpEq,
			Args: []expression.Expression{
				&expression.AttributeRef{Key: "foo", Level: expression.Level("unknown_level")},
				&expression.StringValue{Value: "bar"},
			},
		}
		_, _, err := buildFilterQuery(call, nil)
		require.ErrorIs(t, err, tracestore.ErrFilterUnsupported)
	})

	t.Run("unsupported field is refused", func(t *testing.T) {
		call := &expression.Call{
			Op: expression.OpEq,
			Args: []expression.Expression{
				&expression.FieldRef{Name: "unsupportedField", Level: expression.LevelSpan},
				&expression.StringValue{Value: "bar"},
			},
		}
		_, _, err := buildFilterQuery(call, nil)
		require.ErrorIs(t, err, tracestore.ErrFilterUnsupported)
	})

	t.Run("ordering string constant on attribute is refused", func(t *testing.T) {
		call := &expression.Call{
			Op: expression.OpGt,
			Args: []expression.Expression{
				&expression.AttributeRef{Key: "foo", Level: expression.LevelSpan},
				&expression.StringValue{Value: "bar"},
			},
		}
		_, _, err := buildFilterQuery(call, nil)
		require.ErrorIs(t, err, tracestore.ErrFilterUnsupported)
	})

	t.Run("ordering non-number on attribute is refused", func(t *testing.T) {
		call := &expression.Call{
			Op: expression.OpGt,
			Args: []expression.Expression{
				&expression.AttributeRef{Key: "foo", Level: expression.LevelSpan},
				&expression.AnyValue{Value: "not-a-number"},
			},
		}
		_, _, err := buildFilterQuery(call, nil)
		require.ErrorIs(t, err, tracestore.ErrFilterInvalid)
	})
}

func TestExtractAttributeKeys(t *testing.T) {
	var p builder.Predicate
	filter := p.And(
		p.Span().Attr("http.status_code").Eq("200"),
		p.Resource().Attr("env").Eq("prod"),
		p.Span().Attr("http.status_code").Eq("500"), // duplicate
		p.Resource().Service.Eq("frontend"),         // built-in field, not attribute
	)
	keys := extractAttributeKeys(filter)
	assert.Equal(t, []string{"http.status_code", "env"}, keys)
	assert.Empty(t, extractAttributeKeys(nil))
}

func TestBuildFilterQuery_MoreTypesAndFields(t *testing.T) {
	var p builder.Predicate

	t.Run("span.spanID", func(t *testing.T) {
		call := p.Span().SpanID.Eq("0000000000000001")
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "s.id = ?", sqlStr)
		assert.Equal(t, []any{"0000000000000001"}, args)
	})

	t.Run("scope.version", func(t *testing.T) {
		call := p.Scope().Version.Eq("v1.0.0")
		sqlStr, args, err := buildFilterQuery(call, nil)
		require.NoError(t, err)
		assert.Equal(t, "s.scope_version = ?", sqlStr)
		assert.Equal(t, []any{"v1.0.0"}, args)
	})

	t.Run("span.kind all variants", func(t *testing.T) {
		kinds := map[string]string{
			"CLIENT":      "Client",
			"producer":    "Producer",
			"CONSUMER":    "Consumer",
			"internal":    "Internal",
			"unspecified": "Unspecified",
			"custom":      "custom",
		}
		for in, expected := range kinds {
			call := p.Span().Kind.Eq(in)
			sqlStr, args, err := buildFilterQuery(call, nil)
			require.NoError(t, err)
			assert.Equal(t, "s.kind = ?", sqlStr)
			assert.Equal(t, []any{expected}, args)
		}
	})

	t.Run("span.status all variants", func(t *testing.T) {
		statuses := map[string]string{
			"ok":      "Ok",
			"ERROR":   "Error",
			"unset":   "Unset",
			"unknown": "unknown",
		}
		for in, expected := range statuses {
			call := p.Span().Status.Eq(in)
			sqlStr, args, err := buildFilterQuery(call, nil)
			require.NoError(t, err)
			assert.Equal(t, "s.status_code = ?", sqlStr)
			assert.Equal(t, []any{expected}, args)
		}
	})

	t.Run("exists on all built-in fields", func(t *testing.T) {
		assertExists := func(call *expression.Call, expectedSQL string) {
			sqlStr, args, err := buildFilterQuery(call, nil)
			require.NoError(t, err)
			assert.Equal(t, expectedSQL, sqlStr)
			assert.Empty(t, args)
		}
		assertExists(p.Span().Name.Exists(), "s.name != ''")
		assertExists(p.Resource().Service.Exists(), "s.service_name != ''")
		assertExists(p.Span().Duration.Exists(), "s.duration > 0")
		assertExists(p.Span().TraceID.Exists(), "s.trace_id != ''")
		assertExists(p.Span().SpanID.Exists(), "s.id != ''")
		assertExists(p.Span().Kind.Exists(), "s.kind != ''")
		assertExists(p.Span().Status.Exists(), "s.status_code != ''")
		assertExists(p.Scope().Name.Exists(), "s.scope_name != ''")
		assertExists(p.Scope().Version.Exists(), "s.scope_version != ''")
		assertExists(p.Event().Name.Exists(), "arrayExists(x -> x.name != '', s.events)")
	})

	t.Run("exists on attributes with metadata types across levels", func(t *testing.T) {
		meta := attributeMetadata{
			"tag": {
				span:     []pcommon.ValueType{pcommon.ValueTypeInt},
				resource: []pcommon.ValueType{pcommon.ValueTypeDouble},
				scope:    []pcommon.ValueType{pcommon.ValueTypeBool},
				event:    []pcommon.ValueType{pcommon.ValueTypeBytes},
				link:     []pcommon.ValueType{pcommon.ValueTypeStr},
			},
		}
		levels := []struct {
			call     *expression.Call
			expected string
		}{
			{p.Span().Attr("tag").Exists(), "has(s.int_attributes.key, ?)"},
			{p.Resource().Attr("tag").Exists(), "has(s.resource_double_attributes.key, ?)"},
			{p.Scope().Attr("tag").Exists(), "has(s.scope_bool_attributes.key, ?)"},
			{p.Event().Attr("tag").Exists(), "arrayExists(x -> has(x.complex_attributes.key, ?), s.events)"},
			{p.Link().Attr("tag").Exists(), "arrayExists(x -> has(x.str_attributes.key, ?), s.links)"},
		}
		for _, tc := range levels {
			sqlStr, args, err := buildFilterQuery(tc.call, meta)
			require.NoError(t, err)
			assert.Equal(t, tc.expected, sqlStr)
			assert.Equal(t, []any{"tag"}, args)
		}
	})

	t.Run("typed comparisons for double and bool", func(t *testing.T) {
		callDouble := &expression.Call{
			Op: expression.OpGt,
			Args: []expression.Expression{
				&expression.AttributeRef{Key: "score", Level: expression.LevelSpan},
				&expression.DoubleValue{Value: 3.14},
			},
		}
		sqlDouble, argsDouble, err := buildFilterQuery(callDouble, nil)
		require.NoError(t, err)
		assert.Equal(t, "arrayExists((key, value) -> key = ? AND value > ?, s.double_attributes.key, s.double_attributes.value)", sqlDouble)
		assert.Equal(t, []any{"score", 3.14}, argsDouble)

		callBool := &expression.Call{
			Op: expression.OpEq,
			Args: []expression.Expression{
				&expression.AttributeRef{Key: "active", Level: expression.LevelSpan},
				&expression.BoolValue{Value: true},
			},
		}
		sqlBool, argsBool, err := buildFilterQuery(callBool, nil)
		require.NoError(t, err)
		assert.Equal(t, "arrayExists((key, value) -> key = ? AND value = ?, s.bool_attributes.key, s.bool_attributes.value)", sqlBool)
		assert.Equal(t, []any{"active", true}, argsBool)
	})

	t.Run("comparison operators lt, lte, gte, ne", func(t *testing.T) {
		ops := []struct {
			call     *expression.Call
			expected string
		}{
			{p.Span().Attr("count").Lt(5), "arrayExists((key, value) -> key = ? AND value < ?, s.int_attributes.key, s.int_attributes.value)"},
			{p.Span().Attr("count").Lte(5), "arrayExists((key, value) -> key = ? AND value <= ?, s.int_attributes.key, s.int_attributes.value)"},
			{p.Span().Attr("count").Gte(5), "arrayExists((key, value) -> key = ? AND value >= ?, s.int_attributes.key, s.int_attributes.value)"},
		}
		for _, tc := range ops {
			sqlStr, args, err := buildFilterQuery(tc.call, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.expected, sqlStr)
			assert.Equal(t, []any{"count", int64(5)}, args)
		}
	})

	t.Run("ordered string attribute with numeric metadata", func(t *testing.T) {
		meta := attributeMetadata{
			"num": {
				span: []pcommon.ValueType{pcommon.ValueTypeInt, pcommon.ValueTypeDouble},
			},
		}
		call := &expression.Call{
			Op: expression.OpGt,
			Args: []expression.Expression{
				&expression.AttributeRef{Key: "num", Level: expression.LevelSpan},
				&expression.AnyValue{Value: "42"},
			},
		}
		sqlStr, args, err := buildFilterQuery(call, meta)
		require.NoError(t, err)
		assert.Contains(t, sqlStr, "s.int_attributes.value")
		assert.Contains(t, sqlStr, "s.double_attributes.value")
		assert.Equal(t, []any{"num", int64(42), "num", float64(42)}, args)
	})

	t.Run("unordered string attribute with multiple metadata types", func(t *testing.T) {
		meta := attributeMetadata{
			"flex": {
				span: []pcommon.ValueType{pcommon.ValueTypeInt, pcommon.ValueTypeBool, pcommon.ValueTypeStr},
			},
		}
		call := p.Span().Attr("flex").Eq("true")
		sqlStr, args, err := buildFilterQuery(call, meta)
		require.NoError(t, err)
		assert.Contains(t, sqlStr, "s.bool_attributes")
		assert.Contains(t, sqlStr, "s.str_attributes")
		assert.Contains(t, args, true)
		assert.Contains(t, args, "true")
	})
}

func TestBuildFilterQuery_ErrorsAndRejections(t *testing.T) {
	t.Run("arity error on comparison", func(t *testing.T) {
		call := &expression.Call{
			Op:   expression.OpEq,
			Args: []expression.Expression{&expression.AttributeRef{Key: "foo"}},
		}
		_, _, err := buildFilterQuery(call, nil)
		require.ErrorIs(t, err, tracestore.ErrFilterInvalid)
		assert.Contains(t, err.Error(), "cannot take 1 arguments")
	})

	t.Run("arity error on exists", func(t *testing.T) {
		call := &expression.Call{
			Op: expression.OpExists,
			Args: []expression.Expression{
				&expression.AttributeRef{Key: "foo"},
				&expression.StringValue{Value: "extra"},
			},
		}
		_, _, err := buildFilterQuery(call, nil)
		require.ErrorIs(t, err, tracestore.ErrFilterInvalid)
	})

	t.Run("ref against ref is rejected", func(t *testing.T) {
		call := &expression.Call{
			Op: expression.OpEq,
			Args: []expression.Expression{
				&expression.AttributeRef{Key: "foo"},
				&expression.AttributeRef{Key: "bar"},
			},
		}
		_, _, err := buildFilterQuery(call, nil)
		require.ErrorIs(t, err, tracestore.ErrFilterUnsupported)
	})

	t.Run("ordering on boolean attribute is refused", func(t *testing.T) {
		call := &expression.Call{
			Op: expression.OpGt,
			Args: []expression.Expression{
				&expression.AttributeRef{Key: "flag", Level: expression.LevelSpan},
				&expression.BoolValue{Value: true},
			},
		}
		_, _, err := buildFilterQuery(call, nil)
		require.ErrorIs(t, err, tracestore.ErrFilterUnsupported)
	})

	t.Run("ordering on non-numeric metadata attribute is refused", func(t *testing.T) {
		meta := attributeMetadata{
			"label": {
				span: []pcommon.ValueType{pcommon.ValueTypeStr},
			},
		}
		call := &expression.Call{
			Op: expression.OpGt,
			Args: []expression.Expression{
				&expression.AttributeRef{Key: "label", Level: expression.LevelSpan},
				&expression.AnyValue{Value: "123"},
			},
		}
		_, _, err := buildFilterQuery(call, meta)
		require.ErrorIs(t, err, tracestore.ErrFilterUnsupported)
	})

	t.Run("invalid duration constant string", func(t *testing.T) {
		call := &expression.Call{
			Op: expression.OpGt,
			Args: []expression.Expression{
				&expression.FieldRef{Name: expression.SpanFieldDuration, Level: expression.LevelSpan},
				&expression.AnyValue{Value: "not-a-duration"},
			},
		}
		_, _, err := buildFilterQuery(call, nil)
		require.ErrorIs(t, err, tracestore.ErrFilterInvalid)
	})

	t.Run("unsupported constant type for text comparison", func(t *testing.T) {
		call := &expression.Call{
			Op: expression.OpEq,
			Args: []expression.Expression{
				&expression.FieldRef{Name: expression.SpanFieldName, Level: expression.LevelSpan},
				&expression.IntValue{Value: 123},
			},
		}
		_, _, err := buildFilterQuery(call, nil)
		require.ErrorIs(t, err, tracestore.ErrFilterUnsupported)
	})

	t.Run("membership arity error", func(t *testing.T) {
		call := &expression.Call{
			Op:   expression.OpIn,
			Args: []expression.Expression{&expression.AttributeRef{Key: "foo"}},
		}
		_, _, err := buildFilterQuery(call, nil)
		require.ErrorIs(t, err, tracestore.ErrFilterInvalid)
	})

	t.Run("membership non-list argument", func(t *testing.T) {
		call := &expression.Call{
			Op: expression.OpIn,
			Args: []expression.Expression{
				&expression.AttributeRef{Key: "foo"},
				&expression.StringValue{Value: "not-a-list"},
			},
		}
		_, _, err := buildFilterQuery(call, nil)
		require.ErrorIs(t, err, tracestore.ErrFilterUnsupported)
	})

	t.Run("empty membership list is invalid", func(t *testing.T) {
		call := &expression.Call{
			Op: expression.OpIn,
			Args: []expression.Expression{
				&expression.AttributeRef{Key: "foo"},
				&expression.List{Values: []string{}},
			},
		}
		_, _, err := buildFilterQuery(call, nil)
		require.ErrorIs(t, err, tracestore.ErrFilterInvalid)
	})

	t.Run("unknown operator is refused", func(t *testing.T) {
		call := &expression.Call{
			Op: expression.Operator("unknown_op"),
			Args: []expression.Expression{
				&expression.AttributeRef{Key: "foo"},
				&expression.StringValue{Value: "bar"},
			},
		}
		_, _, err := buildFilterQuery(call, nil)
		require.ErrorIs(t, err, tracestore.ErrFilterUnsupported)
	})

	t.Run("empty conjunction is invalid", func(t *testing.T) {
		call := &expression.Call{
			Op:   expression.OpAnd,
			Args: []expression.Expression{},
		}
		_, _, err := buildFilterQuery(call, nil)
		require.ErrorIs(t, err, tracestore.ErrFilterInvalid)
	})

	t.Run("negation invalid arity", func(t *testing.T) {
		call := &expression.Call{
			Op:   expression.OpNot,
			Args: []expression.Expression{},
		}
		_, _, err := buildFilterQuery(call, nil)
		require.ErrorIs(t, err, tracestore.ErrFilterInvalid)
	})

	t.Run("exists on unsupported field", func(t *testing.T) {
		call := &expression.Call{
			Op:   expression.OpExists,
			Args: []expression.Expression{&expression.FieldRef{Name: "bad", Level: expression.LevelSpan}},
		}
		_, _, err := buildFilterQuery(call, nil)
		require.ErrorIs(t, err, tracestore.ErrFilterUnsupported)
	})
}
