package quota

import (
	"encoding/json"
	"math"
	"strconv"
	"testing"

	"pgregory.net/rapid"
)

// A page is assembled from independent buckets of synthetic line items. The
// oracle sums only integer-cent USD amounts; malformed and foreign-currency
// entries must be visible through partial, without erasing their valid siblings.
func TestPropertyAnthropicCostBuckets(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		amounts := rapid.SliceOfN(rapid.IntRange(0, 100_000), 0, 25).Draw(t, "cents")
		invalid := rapid.IntRange(0, 3).Draw(t, "invalid-kind")
		position := rapid.IntRange(0, len(amounts)).Draw(t, "invalid-position")
		bucketCount := rapid.IntRange(1, 4).Draw(t, "buckets")
		items := make([]any, 0, len(amounts)+1)
		wantCents := 0
		for i := 0; i <= len(amounts); i++ {
			if i == position {
				switch invalid {
				case 0:
					items = append(items, map[string]any{"amount": "not-a-number", "currency": "USD"})
				case 1:
					items = append(items, map[string]any{"amount": "100", "currency": "EUR"})
				case 2:
					items = append(items, map[string]any{"amount": "-100", "currency": "USD"})
				default:
					items = append(items, []string{"not an item"})
				}
			}
			if i < len(amounts) {
				wantCents += amounts[i]
				items = append(items, map[string]any{"amount": strconv.Itoa(amounts[i]), "currency": "USD"})
			}
		}
		buckets := make([]any, bucketCount)
		for i := range buckets {
			buckets[i] = map[string]any{"results": []any{}}
		}
		for i, item := range items {
			bucket := buckets[i%bucketCount].(map[string]any)
			bucket["results"] = append(bucket["results"].([]any), item)
		}
		body, err := json.Marshal(map[string]any{"data": buckets, "has_more": false})
		if err != nil {
			t.Fatal(err)
		}
		got, partial, next, err := parseAnthropicCostPage(body)
		if err != nil || !partial || next != "" || math.Abs(got-float64(wantCents)/100) > 1e-8 {
			t.Fatalf("cents=%v invalid=%d position=%d buckets=%d got=%v partial=%v next=%q err=%v want=%v", amounts, invalid, position, bucketCount, got, partial, next, err, float64(wantCents)/100)
		}
	})
}
