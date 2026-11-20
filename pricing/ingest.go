package pricing

import (
	"fmt"
	"time"
)

// Snapshot is a complete, internally consistent version of the supplier
// ledger read from a page. A snapshot is applied atomically: either every
// revision and order in it is stored, or nothing changes.
type Snapshot struct {
	// Version identifies the completed ledger version the snapshot was read
	// from. It must be the same for every record and must not be empty.
	Version string
	// ReadAt is when the reader observed the page. It is the default
	// knowledge time for revisions and the default deletion time for
	// tombstones.
	ReadAt time.Time
	// Revisions are the price revisions visible in this version.
	Revisions []PriceRevision
	// Orders are the orders visible in this version.
	Orders []Order
}

// RefreshResult summarises what a Refresh changed. Re-reading unchanged data
// leaves every counter at zero.
type RefreshResult struct {
	// Version is the version that was ingested.
	Version string
	// NewRevisions counts revisions that were not previously known.
	NewRevisions int
	// NewDeletions counts revisions whose deletion became known for the first
	// time.
	NewDeletions int
	// NewOrders counts orders that were not previously stored.
	NewOrders int
	// NewLines counts order lines appended to previously stored orders.
	NewLines int
}

// Refresh reads the current complete ledger version from the page and merges
// it into the warehouse. Re-reading an unchanged revision or an unchanged
// order is not an error and does not duplicate anything. Any conflict or
// malformed record aborts the whole refresh, leaving stored history as it was.
func (w *Warehouse) Refresh(source Source) (*RefreshResult, error) {
	snapshot, err := source.Read()
	if err != nil {
		return nil, err
	}
	return w.Ingest(snapshot)
}

// Ingest merges a snapshot into the warehouse atomically. It is the unit
// Refresh is built from and is useful for replaying a snapshot captured
// elsewhere.
func (w *Warehouse) Ingest(snapshot Snapshot) (*RefreshResult, error) {
	if err := validateSnapshot(snapshot); err != nil {
		return nil, err
	}

	// Work on deep copies so that a failure at any later step never mutates
	// the warehouse's stored history.
	revisions := cloneRevisionRecords(w.revisions)
	orders := cloneOrderRecords(w.orders)
	result := &RefreshResult{Version: snapshot.Version}
	readAt := snapshot.ReadAt.UTC()

	index := make(map[revisionKey]*revisionRecord, len(revisions))
	for _, record := range revisions {
		index[record.revision.key()] = record
	}

	for _, revision := range snapshot.Revisions {
		knownAt := readAt
		if !revision.KnownAt.IsZero() {
			knownAt = revision.KnownAt.UTC()
		}
		deletedAt := readAt
		if !revision.DeletedAt.IsZero() {
			deletedAt = revision.DeletedAt.UTC()
		}

		storedRevision := revision
		storedRevision.KnownAt = knownAt
		storedRevision.ValidFrom = revision.ValidFrom.UTC()
		storedRevision.ValidTo = revision.ValidTo.UTC()
		storedRevision.Deleted = false
		storedRevision.DeletedAt = time.Time{}

		existing, ok := index[revision.key()]
		if !ok {
			record := &revisionRecord{
				revision:  storedRevision,
				knownAt:   knownAt,
				deleted:   revision.Deleted,
				deletedAt: deletedAt,
			}
			revisions = append(revisions, record)
			index[record.revision.key()] = record
			result.NewRevisions++
			if record.deleted {
				result.NewDeletions++
			}
			continue
		}

		if reason := existing.revision.conflictReason(revision); reason != "" {
			return nil, &RevisionConflictError{
				PriceID:  revision.PriceID,
				Revision: revision.Revision,
				Reason:   reason,
			}
		}

		if !revision.Deleted {
			// A live read of a revision that is already tombstoned is
			// ignored: the tombstone wins and the old price does not come
			// back to life.
			continue
		}

		switch {
		case !existing.deleted:
			if deletedAt.Before(existing.knownAt) {
				return nil, &DeletionConflictError{
					PriceID:  revision.PriceID,
					Revision: revision.Revision,
					Reason: fmt.Sprintf(
						"deletion at %s is before the revision was known at %s",
						fmtRevisionTime(deletedAt), fmtRevisionTime(existing.knownAt),
					),
				}
			}
			record := *existing
			record.deleted = true
			record.deletedAt = deletedAt
			replaceRevision(revisions, &record)
			index[record.revision.key()] = &record
			result.NewDeletions++
		case !existing.deletedAt.Equal(deletedAt):
			return nil, &DeletionConflictError{
				PriceID:  revision.PriceID,
				Revision: revision.Revision,
				Reason: fmt.Sprintf(
					"already deleted at %s, read deletion at %s",
					fmtRevisionTime(existing.deletedAt), fmtRevisionTime(deletedAt),
				),
			}
		}
		// An identical deletion re-read is idempotent.
	}

	if err := validateRevisionRecords(revisions); err != nil {
		return nil, err
	}

	orderIndex := make(map[string]*orderRecord, len(orders))
	for _, record := range orders {
		orderIndex[record.order.OrderID] = record
	}
	newOrders, newLines, err := mergeOrders(&orders, orderIndex, snapshot.Orders, readAt)
	if err != nil {
		return nil, err
	}
	result.NewOrders = newOrders
	result.NewLines = newLines

	if err := validateOrderRecords(orders); err != nil {
		return nil, err
	}

	// Persist before adopting the new in-memory state so that a failed write
	// leaves the previous results intact.
	if w.path != "" {
		if err := writeWarehouseFile(w.path, revisions, orders); err != nil {
			return nil, err
		}
	}
	w.revisions = revisions
	w.orders = orders
	return result, nil
}

// replaceRevision swaps the record with the same key in place.
func replaceRevision(records []*revisionRecord, replacement *revisionRecord) {
	for position, record := range records {
		if record.revision.key() == replacement.revision.key() {
			records[position] = replacement
			return
		}
	}
}

// cloneRevisionRecords returns deep copies of the records so that a merge can
// be attempted without risking the stored history.
func cloneRevisionRecords(records []*revisionRecord) []*revisionRecord {
	clones := make([]*revisionRecord, 0, len(records))
	for _, record := range records {
		copied := *record
		clones = append(clones, &copied)
	}
	return clones
}

// cloneOrderRecords returns deep copies of the records, including their line
// slices.
func cloneOrderRecords(records []*orderRecord) []*orderRecord {
	clones := make([]*orderRecord, 0, len(records))
	for _, record := range records {
		copied := *record
		copied.order = record.order.clone()
		clones = append(clones, &copied)
	}
	return clones
}

// mergeOrders merges snapshot orders into *orders without changing already
// stored lines. It returns the number of newly stored orders and newly added
// lines.
func mergeOrders(orders *[]*orderRecord, index map[string]*orderRecord, incoming []Order, readAt time.Time) (int, int, error) {
	newOrders := 0
	newLines := 0
	for _, order := range incoming {
		existing, ok := index[order.OrderID]
		if !ok {
			record := &orderRecord{order: order.clone(), recordedAt: readAt}
			*orders = append(*orders, record)
			index[order.OrderID] = record
			newOrders++
			newLines += len(order.Lines)
			continue
		}
		if !existing.order.BusinessAt.Equal(order.BusinessAt) {
			return 0, 0, &OrderConflictError{
				OrderID: order.OrderID,
				Reason: fmt.Sprintf(
					"business time moved from %s to %s",
					fmtRevisionTime(existing.order.BusinessAt), fmtRevisionTime(order.BusinessAt),
				),
			}
		}
		for _, line := range order.Lines {
			stored, found := findLine(existing.order.Lines, line.PriceID)
			if !found {
				existing.order.Lines = append(existing.order.Lines, line)
				newLines++
				continue
			}
			if stored.Qty != line.Qty {
				return 0, 0, &OrderConflictError{
					OrderID: order.OrderID,
					Reason: fmt.Sprintf(
						"line %s quantity changed from %d to %d",
						line.PriceID, stored.Qty, line.Qty,
					),
				}
			}
		}
	}
	return newOrders, newLines, nil
}

func findLine(lines []OrderLine, priceID string) (OrderLine, bool) {
	for _, line := range lines {
		if line.PriceID == priceID {
			return line, true
		}
	}
	return OrderLine{}, false
}

func validateSnapshot(snapshot Snapshot) error {
	if snapshot.Version == "" {
		return &IncompleteLedgerError{Reason: "no completed ledger version"}
	}
	if snapshot.ReadAt.IsZero() {
		return &IncompleteLedgerError{Reason: "no read time for ledger version " + snapshot.Version}
	}
	for _, revision := range snapshot.Revisions {
		knownAt := snapshot.ReadAt
		if !revision.KnownAt.IsZero() {
			knownAt = revision.KnownAt
		}
		if err := validateRevision(revision, knownAt); err != nil {
			return err
		}
		if revision.Deleted {
			deletedAt := snapshot.ReadAt
			if !revision.DeletedAt.IsZero() {
				deletedAt = revision.DeletedAt
			}
			if deletedAt.Before(knownAt) {
				return &InvalidDataError{
					Field:  fmt.Sprintf("price-revision[%s#%d].deleted-at", revision.PriceID, revision.Revision),
					Value:  fmtRevisionTime(deletedAt),
					Reason: "deletion happened before the revision was known",
				}
			}
		}
	}
	for _, order := range snapshot.Orders {
		if err := validateOrder(order); err != nil {
			return err
		}
	}
	return nil
}

func validateRevisionRecords(records []*revisionRecord) error {
	return (&Warehouse{}).validateRevisions(records)
}

func validateOrderRecords(records []*orderRecord) error {
	for _, record := range records {
		if err := validateOrder(record.order); err != nil {
			return err
		}
	}
	return nil
}
