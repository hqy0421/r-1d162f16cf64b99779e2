package pricing

import "math"

// addCents adds two cent values, reporting false on overflow.
func addCents(a, b int64) (int64, bool) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, false
	}
	return a + b, true
}

// mulCents multiplies a cent amount by a quantity, reporting false on
// overflow. Magnitudes are computed in uint64 so that MinInt64 is handled.
func mulCents(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	negative := (a < 0) != (b < 0)

	ua := uint64(a)
	if a < 0 {
		ua = uint64(-(a + 1)) + 1
	}
	ub := uint64(b)
	if b < 0 {
		ub = uint64(-(b + 1)) + 1
	}

	if ua > math.MaxUint64/ub {
		return 0, false
	}
	product := ua * ub

	if !negative {
		if product > math.MaxInt64 {
			return 0, false
		}
		return int64(product), true
	}
	if product > uint64(math.MaxInt64)+1 {
		return 0, false
	}
	if product == uint64(math.MaxInt64)+1 {
		return math.MinInt64, true
	}
	return -int64(product), true
}
