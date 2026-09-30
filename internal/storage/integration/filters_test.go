// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package integration

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	builder "github.com/jaegertracing/jaeger/internal/expression"
	"github.com/jaegertracing/jaeger/internal/storage/integration/capabilities"
)

func TestFilterCorpusAndTestCases(t *testing.T) {
	corpus := BuildCorpus(t, nil, capabilities.Capabilities{}).Filter
	require.NotEmpty(t, corpus)
	assert.Contains(t, corpus, "worker")
	assert.Contains(t, corpus, "cart_get")
	assert.Contains(t, corpus, "cart_post")
	assert.Contains(t, corpus, "checkout")
	assert.Contains(t, corpus, "search")

	start, end := filterCorpusTimeRange(corpus)
	assert.False(t, start.IsZero())
	assert.False(t, end.IsZero())
	assert.True(t, start.Before(end))

	var p builder.Predicate
	scope := filterCorpusScope(p, corpus)
	require.NotNil(t, scope)

	scopeQuery := filterQuery(t, scope, start, end)
	require.NotNil(t, scopeQuery)
	require.NotNil(t, scopeQuery.Filter)

	cases := filterTestCases(p)
	require.NotEmpty(t, cases)
	for _, tc := range cases {
		t.Run(tc.caption, func(t *testing.T) {
			assert.NotEmpty(t, tc.expected)
			for _, name := range tc.expected {
				assert.Contains(t, corpus, name, "expected fixture %q must exist in corpus", name)
			}
			query := filterQuery(t, p.And(scope, tc.filter), start, end)
			assert.NotNil(t, query)
			assert.NotNil(t, query.Filter)
		})
	}
}
