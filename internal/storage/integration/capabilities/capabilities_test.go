// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package capabilities

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestAttributeOrderingOutcome checks that exactly one of the three attribute-ordering cases
// runs for every deployment the constructors and the two modifiers describe.
func TestAttributeOrderingOutcome(t *testing.T) {
	tests := []struct {
		name string
		caps Capabilities
		runs string
	}{
		{name: "Elasticsearch", caps: Elasticsearch(), runs: attributeOrderingTest},
		{name: "OpenSearch", caps: OpenSearch(), runs: attributeOrderingTest},
		{name: "Elasticsearch without numeric attributes", caps: Elasticsearch().WithoutNumericAttributes(), runs: attributeRefusedTest},
		{name: "OpenSearch without numeric attributes", caps: OpenSearch().WithoutNumericAttributes(), runs: attributeRefusedTest},
		{name: "Elasticsearch with the mapping over older indices", caps: Elasticsearch().WithNumericAttributesNotYetIndexed(), runs: attributeUnindexedTest},
		{name: "modifiers replace each other", caps: Elasticsearch().WithoutNumericAttributes().WithNumericAttributesNotYetIndexed(), runs: attributeUnindexedTest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			skipped := test.caps.SkipList()
			for _, outcome := range attributeOrderingTests {
				count := 0
				for _, name := range skipped {
					if name == outcome {
						count++
					}
				}
				if outcome == test.runs {
					assert.Zero(t, count, "%s runs", outcome)
				} else {
					assert.Equal(t, 1, count, "%s is skipped once", outcome)
				}
			}
		})
	}
}

func TestFilterCapabilities(t *testing.T) {
	ch := ClickHouse().SkipList()
	assert.NotContains(t, ch, structuredFilterTest, "ClickHouse runs FindTracesWithFilter")
	assert.Contains(t, ch, filterUnqualifiedEventTest)
	assert.Contains(t, ch, filterDurationGtTest)
	assert.Contains(t, ch, filterUnindexedLevelTest)
	assert.NotContains(t, ch, filterScopeLevelTest, "ClickHouse supports scope level")
	assert.NotContains(t, ch, filterLinkLevelTest, "ClickHouse supports link level")
	assert.NotContains(t, ch, filterSpanKindTest, "ClickHouse supports span.kind")
	assert.NotContains(t, ch, filterSpanStatusTest, "ClickHouse supports span.status")

	es := Elasticsearch().SkipList()
	assert.NotContains(t, es, structuredFilterTest, "Elasticsearch runs FindTracesWithFilter")
	assert.Contains(t, es, filterScopeLevelTest)
	assert.Contains(t, es, filterLinkLevelTest)
	assert.Contains(t, es, filterSpanKindTest)
	assert.Contains(t, es, filterSpanStatusTest)
	assert.Contains(t, es, filterStringTypedConstTest)

	os := OpenSearch().SkipList()
	assert.NotContains(t, os, structuredFilterTest, "OpenSearch runs FindTracesWithFilter")
	assert.Contains(t, os, filterScopeLevelTest)
	assert.Contains(t, os, filterLinkLevelTest)
	assert.Contains(t, os, filterSpanKindTest)
	assert.Contains(t, os, filterSpanStatusTest)
	assert.Contains(t, os, filterStringTypedConstTest)
}
