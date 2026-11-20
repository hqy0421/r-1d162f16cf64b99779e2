package pricing

import (
	"fmt"
	"sort"
	"time"
)

// revisionRecord is the warehouse's permanent record of one revision. The
// revision content never changes once recorded; only the deletion tombstone
// can be appended once. knownAt is the moment the warehouse first learned the
// revision and never moves.
type revisionRecord struct {
	revision  PriceRevision
	knownAt   time.Time
	deleted   bool
	deletedAt time.Time
}

// orderRecord is the warehouse's permanent record of one order, together with
// the moment the warehouse first learned about it. Orders may gain lines
// between reads; existing lines are immutable.
type orderRecord struct {
	order      Order
	recordedAt time.Time
}

// Warehouse holds the complete price history and order book. It is not safe
// for concurrent use.
//
// A Warehouse is normally created with Open or NewWarehouse and refreshed with
// Refresh. It is saved with Save and reloaded with Open.
type Warehouse struct {
	path      string
	revisions []*revisionRecord
	orders    []*orderRecord
	savedAt   time.Time
}

// NewWarehouse returns an empty, in-memory warehouse that does not persist to
// disk until Save is called.
func NewWarehouse() *Warehouse {
	return &Warehouse{}
}

// Path returns the file the warehouse persists to, or "" when the warehouse
// has not been saved to disk.
func (w *Warehouse) Path() string {
	return w.path
}

// revisionIndex maps revision keys to stored records.
func (w *Warehouse) revisionIndex() map[revisionKey]*revisionRecord {
	index := make(map[revisionKey]*revisionRecord, len(w.revisions))
	for _, record := range w.revisions {
		index[record.revision.key()] = record
	}
	return index
}

// Validate checks every stored revision and order for internal consistency,
// including overlapping prices that are ambiguous at a single knowledge time.
// It does not modify the warehouse.
func (w *Warehouse) Validate() error {
	if err := validateRevisionRecords(w.revisions); err != nil {
		return err
	}
	return validateOrderRecords(w.orders)
}

// validateRevisions checks a set of revision records for validity and for
// ambiguous overlaps: two live revisions for the same price whose business
// intervals overlap and which were learned at the same moment. Such a pair
// has equal standing, so the ledger cannot say which price applies. A
// revision learned strictly later supersedes an earlier one and is not an
// ambiguity, because it is how a retroactive correction is represented.
func (w *Warehouse) validateRevisions(records []*revisionRecord) error {
	live := map[string][]PriceRevision{}
	for _, record := range records {
		revision := record.revision
		if err := validateRevision(revision, record.knownAt); err != nil {
			return err
		}
		if record.deleted {
			if record.deletedAt.Before(record.knownAt) {
				return &InvalidDataError{
					Field:  fmt.Sprintf("price-revision[%s#%d].deleted-at", revision.PriceID, revision.Revision),
					Value:  fmtRevisionTime(record.deletedAt),
					Reason: "deletion happened before the revision was known",
				}
			}
			continue
		}
		revision.KnownAt = record.knownAt
		live[revision.PriceID] = append(live[revision.PriceID], revision)
	}
	priceIDs := make([]string, 0, len(live))
	for priceID := range live {
		priceIDs = append(priceIDs, priceID)
	}
	sort.Strings(priceIDs)
	for _, priceID := range priceIDs {
		revisions := live[priceID]
		sort.Slice(revisions, func(i, j int) bool {
			return revisions[i].ValidFrom.Before(revisions[j].ValidFrom) ||
				(revisions[i].ValidFrom.Equal(revisions[j].ValidFrom) && revisions[i].Revision < revisions[j].Revision)
		})
		for i := 0; i < len(revisions); i++ {
			for j := i + 1; j < len(revisions); j++ {
				left, right := revisions[i], revisions[j]
				if !left.ValidFrom.Before(right.ValidTo) {
					break
				}
				if !right.ValidFrom.Before(left.ValidTo) {
					continue
				}
				if left.KnownAt.Equal(right.KnownAt) {
					return &OverlappingPriceError{
						PriceID:   priceID,
						Revisions: []int{left.Revision, right.Revision},
						Detail: fmt.Sprintf(
							"revision %d is valid [%s,%s) and revision %d is valid [%s,%s) and both were known at %s",
							left.Revision, fmtRevisionTime(left.ValidFrom), fmtRevisionTime(left.ValidTo),
							right.Revision, fmtRevisionTime(right.ValidFrom), fmtRevisionTime(right.ValidTo),
							fmtRevisionTime(left.KnownAt),
						),
					}
				}
			}
		}
	}
	return nil
}

func validateRevision(revision PriceRevision, knownAt time.Time) error {
	if revision.PriceID == "" {
		return &InvalidDataError{Field: "price-revision.price-id", Value: "", Reason: "must not be empty"}
	}
	if revision.Revision < 0 {
		return &InvalidDataError{
			Field:  fmt.Sprintf("price-revision[%s].revision", revision.PriceID),
			Value:  fmt.Sprint(revision.Revision),
			Reason: "must not be negative",
		}
	}
	if revision.AmountCents < 0 {
		return &InvalidDataError{
			Field:  fmt.Sprintf("price-revision[%s#%d].amount", revision.PriceID, revision.Revision),
			Value:  fmt.Sprint(revision.AmountCents),
			Reason: "must not be negative",
		}
	}
	if knownAt.IsZero() {
		return &InvalidDataError{
			Field:  fmt.Sprintf("price-revision[%s#%d].known-at", revision.PriceID, revision.Revision),
			Value:  "",
			Reason: "must not be zero",
		}
	}
	if revision.ValidFrom.IsZero() {
		return &InvalidDataError{
			Field:  fmt.Sprintf("price-revision[%s#%d].valid-from", revision.PriceID, revision.Revision),
			Value:  "",
			Reason: "must not be zero",
		}
	}
	if revision.ValidTo.IsZero() {
		return &InvalidDataError{
			Field:  fmt.Sprintf("price-revision[%s#%d].valid-to", revision.PriceID, revision.Revision),
			Value:  "",
			Reason: "must not be zero",
		}
	}
	if !revision.ValidTo.After(revision.ValidFrom) {
		return &InvalidDataError{
			Field:  fmt.Sprintf("price-revision[%s#%d].valid-to", revision.PriceID, revision.Revision),
			Value:  fmtRevisionTime(revision.ValidTo),
			Reason: "must be after valid-from",
		}
	}
	return nil
}

func validateOrder(order Order) error {
	if order.OrderID == "" {
		return &InvalidDataError{Field: "order.order-id", Value: "", Reason: "must not be empty"}
	}
	if order.BusinessAt.IsZero() {
		return &InvalidDataError{
			Field:  fmt.Sprintf("order[%s].business-at", order.OrderID),
			Value:  "",
			Reason: "must not be zero",
		}
	}
	for index, line := range order.Lines {
		if line.PriceID == "" {
			return &InvalidDataError{
				Field:  fmt.Sprintf("order[%s].lines[%d].price-id", order.OrderID, index),
				Value:  "",
				Reason: "must not be empty",
			}
		}
		if line.Qty < 0 {
			return &InvalidDataError{
				Field:  fmt.Sprintf("order[%s].lines[%d].qty", order.OrderID, index),
				Value:  fmt.Sprint(line.Qty),
				Reason: "must not be negative",
			}
		}
	}
	return nil
}

func fmtRevisionTime(t time.Time) string {
	if t.IsZero() {
		return "zero"
	}
	return t.UTC().Format(time.RFC3339Nano)
}
