package pricing

import (
	"fmt"
	"time"
)

// InvalidDataError reports a value that cannot be represented in the
// warehouse: a malformed timestamp, a negative amount, a backwards validity
// interval, and so on. No part of the offending snapshot is stored.
type InvalidDataError struct {
	// Field names the offending value, for example "price-revision.amount".
	Field string
	// Value is the rejected input, rendered for humans.
	Value string
	// Reason explains why the value was rejected.
	Reason string
}

func (e *InvalidDataError) Error() string {
	return fmt.Sprintf("pricing: invalid %s %q: %s", e.Field, e.Value, e.Reason)
}

// IncompleteLedgerError reports that a page did not expose a single,
// complete, internally consistent version of the ledger. A refresh that fails
// this way leaves any previously stored history untouched.
type IncompleteLedgerError struct {
	Reason string
}

func (e *IncompleteLedgerError) Error() string {
	return fmt.Sprintf("pricing: ledger page is not a complete version: %s", e.Reason)
}

// RevisionConflictError reports a revision that was already known with
// different contents. Revisions are immutable: the same price ID and revision
// number must always describe the same price and validity interval. A refresh
// that encounters a conflict fails without changing stored history.
type RevisionConflictError struct {
	PriceID  string
	Revision int
	Reason   string
}

func (e *RevisionConflictError) Error() string {
	return fmt.Sprintf("pricing: conflicting revision %s#%d: %s", e.PriceID, e.Revision, e.Reason)
}

// DeletionConflictError reports a tombstone that was already recorded with a
// different deletion time, or an attempt to read a deleted revision as live.
// Deletions are permanent: once a revision has been deleted, later reads
// cannot move that deletion and cannot revive the revision.
type DeletionConflictError struct {
	PriceID  string
	Revision int
	Reason   string
}

func (e *DeletionConflictError) Error() string {
	return fmt.Sprintf("pricing: conflicting deletion of %s#%d: %s", e.PriceID, e.Revision, e.Reason)
}

// OverlappingPriceError reports two live revisions for the same price whose
// business intervals overlap. The ledger cannot say which price applied, so
// the refresh fails rather than storing ambiguous data.
type OverlappingPriceError struct {
	PriceID   string
	Revisions []int
	Detail    string
}

func (e *OverlappingPriceError) Error() string {
	return fmt.Sprintf("pricing: overlapping prices for %s: revisions %v: %s", e.PriceID, e.Revisions, e.Detail)
}

// OrderConflictError reports an order whose immutable contents changed
// between reads. Re-reading an identical order is not an error and does not
// duplicate it.
type OrderConflictError struct {
	OrderID string
	Reason  string
}

func (e *OrderConflictError) Error() string {
	return fmt.Sprintf("pricing: conflicting order %s: %s", e.OrderID, e.Reason)
}

// OrderNotFoundError reports a request to reprice an order that has never
// been ingested.
type OrderNotFoundError struct {
	OrderID string
}

func (e *OrderNotFoundError) Error() string {
	return fmt.Sprintf("pricing: order %s not found", e.OrderID)
}

// AmbiguousPriceError reports that more than one visible revision for the
// same price ID covers a single business instant. The ledger cannot say which
// price applied, so repricing fails instead of silently picking one; no
// partial result is returned.
type AmbiguousPriceError struct {
	PriceID   string
	At        time.Time
	Revisions []int
	OrderID   string
	LineIndex int
}

func (e *AmbiguousPriceError) Error() string {
	return fmt.Sprintf(
		"pricing: ambiguous price for %s at %s: revisions %v all cover that business time (order %s line %d)",
		e.PriceID, e.At.Format(time.RFC3339Nano), e.Revisions, e.OrderID, e.LineIndex,
	)
}

// OverflowError reports that recomputing a line amount or an order total
// would overflow a signed 64-bit cent value.
type OverflowError struct {
	Detail string
}

func (e *OverflowError) Error() string {
	return fmt.Sprintf("pricing: cent amount overflow while computing %s", e.Detail)
}
