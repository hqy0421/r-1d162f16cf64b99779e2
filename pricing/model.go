package pricing

import (
	"sort"
	"time"
)

// PriceRevision is one immutable revision of a supplier price. A revision is
// identified by the pair (PriceID, Revision). The pair never changes meaning:
// once a revision has been ingested, reading it again with different contents
// is a conflict.
type PriceRevision struct {
	// PriceID identifies the price, for example a supplier SKU.
	PriceID string
	// Revision is the supplier's revision number for this price.
	Revision int
	// AmountCents is the price in cents. It is a signed whole number of
	// cents and may be zero, which is a real price and not a missing value.
	AmountCents int64
	// ValidFrom is the inclusive start of the business validity interval.
	ValidFrom time.Time
	// ValidTo is the exclusive end of the business validity interval.
	ValidTo time.Time
	// KnownAt is the moment the revision became known. It is immutable once
	// recorded.
	KnownAt time.Time
	// Deleted reports whether the supplier has deleted this revision.
	Deleted bool
	// DeletedAt is the moment the deletion became known. It is only
	// meaningful when Deleted is true.
	DeletedAt time.Time
}

// Covers reports whether the revision's business interval contains at.
// The interval is half open: ValidFrom is included, ValidTo is excluded.
func (r PriceRevision) Covers(at time.Time) bool {
	return !at.Before(r.ValidFrom) && at.Before(r.ValidTo)
}

// key identifies a revision across the two time axes.
func (r PriceRevision) key() revisionKey {
	return revisionKey{PriceID: r.PriceID, Revision: r.Revision}
}

type revisionKey struct {
	PriceID  string
	Revision int
}

// conflictReason compares the immutable content of two revisions, ignoring
// the deletion fields and the reader-assigned knowledge time, and reports the
// first difference. It returns "" when the contents match.
func (r PriceRevision) conflictReason(other PriceRevision) string {
	switch {
	case r.AmountCents != other.AmountCents:
		return "amount differs"
	case !r.ValidFrom.Equal(other.ValidFrom):
		return "valid-from differs"
	case !r.ValidTo.Equal(other.ValidTo):
		return "valid-to differs"
	}
	return ""
}

// OrderLine is one ordered item. Qty is a whole number of units.
type OrderLine struct {
	PriceID string
	Qty     int64
}

// Order is a supplier order whose lines must be repriced. BusinessAt is the
// business moment used to pick the applicable price; it is the order's
// placement time in the supplier's books, not the time it was read.
type Order struct {
	OrderID    string
	BusinessAt time.Time
	Lines      []OrderLine
}

// PricedLine is the repricing result for one order line.
type PricedLine struct {
	PriceID string
	Qty     int64
	// Revision, ValidFrom, ValidTo and UnitCents describe the price chosen
	// for this line. They are zero when the line could not be priced.
	Revision  int
	ValidFrom time.Time
	ValidTo   time.Time
	UnitCents int64
	// AmountCents is UnitCents*Qty, recomputed in cents.
	AmountCents int64
	// Priced reports whether a visible price applied to the order's
	// business time. When false the line is retained in the report as
	// unrepriced and AmountCents is zero.
	Priced bool
}

// OrderPricing is the repricing result for one order.
type OrderPricing struct {
	OrderID    string
	BusinessAt time.Time
	Lines      []PricedLine
	// TotalCents is the sum of AmountCents over priced lines.
	TotalCents int64
	// Priced reports whether every line was priced.
	Priced bool
	// Missing lists the price IDs of lines that could not be priced, in
	// order, without duplicates.
	Missing []string
}

// View is a consistent, immutable snapshot of the warehouse that answers
// queries as of one knowledge time.
type View struct {
	revisions []PriceRevision
	orders    []Order
}

// Now returns a view that sees every price revision known as of the current
// clock time. This is the "as known now" view: it includes revisions learned
// after the orders were placed.
func (w *Warehouse) Now() *View {
	return w.KnownAt(time.Now().UTC())
}

// KnownAt returns a view that only sees price revisions known at asOf.
// Revisions learned after asOf are invisible, and revisions that were only
// deleted after asOf remain visible. Orders are not filtered by knowledge
// time: an order is the fact being repriced, and its BusinessAt is what
// selects a price. This is the "as known then" view.
func (w *Warehouse) KnownAt(asOf time.Time) *View {
	asOf = asOf.UTC()
	view := &View{}
	for _, record := range w.revisions {
		if record.knownAt.After(asOf) {
			continue
		}
		revision := record.revision
		revision.Deleted = false
		revision.DeletedAt = time.Time{}
		if record.deleted && !record.deletedAt.After(asOf) {
			revision.Deleted = true
			revision.DeletedAt = record.deletedAt
		}
		view.revisions = append(view.revisions, revision)
	}
	for _, record := range w.orders {
		view.orders = append(view.orders, record.order.clone())
	}
	return view
}

// Revisions returns every revision visible in the view, sorted by price ID
// and revision number.
func (v *View) Revisions() []PriceRevision {
	revisions := append([]PriceRevision(nil), v.revisions...)
	sort.Slice(revisions, func(i, j int) bool {
		if revisions[i].PriceID == revisions[j].PriceID {
			return revisions[i].Revision < revisions[j].Revision
		}
		return revisions[i].PriceID < revisions[j].PriceID
	})
	return revisions
}

// Orders returns every order visible in the view, sorted by order ID.
func (v *View) Orders() []Order {
	orders := make([]Order, 0, len(v.orders))
	for _, order := range v.orders {
		orders = append(orders, order.clone())
	}
	sort.Slice(orders, func(i, j int) bool { return orders[i].OrderID < orders[j].OrderID })
	return orders
}

// Order returns the visible order with the given ID.
func (v *View) Order(orderID string) (Order, error) {
	for _, order := range v.orders {
		if order.OrderID == orderID {
			return order.clone(), nil
		}
	}
	return Order{}, &OrderNotFoundError{OrderID: orderID}
}

// Reprice reprices one stored order against the prices visible in the view.
// Lines whose price is missing are retained as unrepriced. If more than one
// visible revision with equal standing covers a line's business time, Reprice
// returns an *AmbiguousPriceError and no partial result.
func (v *View) Reprice(orderID string) (*OrderPricing, error) {
	order, err := v.Order(orderID)
	if err != nil {
		return nil, err
	}
	return v.RepriceOrder(order)
}

// RepriceOrder reprices an arbitrary order against the prices visible in the
// view. It is useful for repricing an order that has not been stored.
func (v *View) RepriceOrder(order Order) (*OrderPricing, error) {
	if err := validateOrder(order); err != nil {
		return nil, err
	}
	pricing := &OrderPricing{
		OrderID:    order.OrderID,
		BusinessAt: order.BusinessAt,
		Lines:      make([]PricedLine, 0, len(order.Lines)),
		Priced:     true,
	}
	seenMissing := map[string]bool{}
	for index, line := range order.Lines {
		price, found, err := v.applicablePrice(line.PriceID, order.BusinessAt)
		if err != nil {
			err.(*AmbiguousPriceError).OrderID = order.OrderID
			err.(*AmbiguousPriceError).LineIndex = index
			return nil, err
		}
		priced := PricedLine{PriceID: line.PriceID, Qty: line.Qty}
		if !found {
			pricing.Priced = false
			if !seenMissing[line.PriceID] {
				seenMissing[line.PriceID] = true
				pricing.Missing = append(pricing.Missing, line.PriceID)
			}
			pricing.Lines = append(pricing.Lines, priced)
			continue
		}
		amount, ok := mulCents(price.AmountCents, line.Qty)
		if !ok {
			return nil, &OverflowError{Detail: "line amount for " + line.PriceID}
		}
		priced.Revision = price.Revision
		priced.ValidFrom = price.ValidFrom
		priced.ValidTo = price.ValidTo
		priced.UnitCents = price.AmountCents
		priced.AmountCents = amount
		priced.Priced = true
		pricing.Lines = append(pricing.Lines, priced)
	}
	for _, line := range pricing.Lines {
		if !line.Priced {
			continue
		}
		total, ok := addCents(pricing.TotalCents, line.AmountCents)
		if !ok {
			return nil, &OverflowError{Detail: "order total for " + order.OrderID}
		}
		pricing.TotalCents = total
	}
	return pricing, nil
}

// RepriceAll reprices every order visible in the view. It fails without a
// partial result if any order is ambiguous.
func (v *View) RepriceAll() ([]*OrderPricing, error) {
	orders := v.Orders()
	results := make([]*OrderPricing, 0, len(orders))
	for _, order := range orders {
		pricing, err := v.RepriceOrder(order)
		if err != nil {
			return nil, err
		}
		results = append(results, pricing)
	}
	return results, nil
}

// applicablePrice selects the single price that applies to priceID at the
// business instant at.
//
// Only revisions whose business interval covers at are candidates. Revisions
// supersede each other by knowledge: a revision learned later overrides an
// earlier one for the time it covers. That is what makes a retroactive
// correction visible in the "as known now" view while leaving an earlier
// "as known then" view untouched.
//
// The candidates are grouped by knowledge time and the latest group wins:
//
//   - if the latest group holds exactly one live revision, that is the price;
//   - if every revision in the latest group is deleted, there is no price and
//     an older, superseded revision is not brought back to life;
//   - if two or more live revisions share the latest knowledge time, the
//     ledger cannot say which applies and an *AmbiguousPriceError is returned.
func (v *View) applicablePrice(priceID string, at time.Time) (PriceRevision, bool, error) {
	var covering []PriceRevision
	for _, revision := range v.revisions {
		if revision.PriceID != priceID || !revision.Covers(at) {
			continue
		}
		covering = append(covering, revision)
	}
	if len(covering) == 0 {
		return PriceRevision{}, false, nil
	}

	latest := covering[0].KnownAt
	for _, revision := range covering[1:] {
		if revision.KnownAt.After(latest) {
			latest = revision.KnownAt
		}
	}
	var latestLive []PriceRevision
	latestHasDeletion := false
	for _, revision := range covering {
		if !revision.KnownAt.Equal(latest) {
			continue
		}
		if revision.Deleted {
			latestHasDeletion = true
			continue
		}
		latestLive = append(latestLive, revision)
	}
	switch {
	case len(latestLive) > 1:
		return PriceRevision{}, false, &AmbiguousPriceError{
			PriceID:   priceID,
			At:        at,
			Revisions: revisionNumbers(latestLive),
		}
	case len(latestLive) == 1:
		return latestLive[0], true, nil
	case latestHasDeletion:
		// The newest thing known is that this price was removed. Do not fall
		// back to a superseded revision: deletion must not resurrect a price.
		return PriceRevision{}, false, nil
	default:
		return PriceRevision{}, false, nil
	}
}

// revisionNumbers returns the sorted revision numbers of revisions.
func revisionNumbers(revisions []PriceRevision) []int {
	numbers := make([]int, 0, len(revisions))
	for _, revision := range revisions {
		numbers = append(numbers, revision.Revision)
	}
	sort.Ints(numbers)
	return numbers
}

func (o Order) clone() Order {
	lines := append([]OrderLine(nil), o.Lines...)
	o.Lines = lines
	return o
}
