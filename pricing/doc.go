// Package pricing ingests supplier price revisions and orders from an
// authenticated Agouti page and reprices historical orders against the prices
// that were visible at a chosen knowledge time.
//
// # Bitemporal model
//
// Every price revision carries two independent time axes:
//
//   - a business validity interval [ValidFrom, ValidTo) that says when the
//     price applies to an order, and
//   - a knowledge time (KnownAt) that says when the supplier made that
//     revision known.
//
// The business interval is half open: ValidFrom is included and ValidTo is
// excluded, so a price ending at midnight and the next price starting at
// midnight never overlap.
//
// A revision may also be deleted. Deletions are recorded as tombstones: the
// revision data is retained forever but is no longer visible once the
// deletion became known. Deleting a revision does not make an older, already
// superseded price valid again, and re-reading a revision that has been
// deleted does not revive it.
//
// Revisions supersede each other by knowledge. When several revisions for the
// same price cover an order's business instant, the one learned last wins.
// That is how a retroactive correction is represented: the correction
// overrides the old price for the time it covers, so it is visible in the
// "as known now" view while an earlier "as known then" view is unaffected.
//
// # Views
//
// Repricing happens through a View. Now() sees every revision known as of the
// current clock time, including revisions learned after the orders were
// placed; this is the "as known now" view. KnownAt(t) only sees revisions
// that were already known at t and ignores revisions that were deleted only
// after t; this is the "as known then" view, so price changes learned later
// cannot pollute a historical view. Orders themselves are treated as facts
// and are visible in both views; their BusinessAt is what selects a price.
//
// With either view, an order line is priced by the single visible revision
// that covers the order's business time. If two revisions of equal standing
// cover that instant, the data is ambiguous and Reprice fails explicitly with
// an *AmbiguousPriceError. If no revision covers it, the line is reported as
// unrepriced and is retained in the report; a revision whose amount is zero
// is a real price and is not treated as missing.
//
// # Ingestion
//
// A completed ledger version is read in one of two ways: Reader reads it from
// the live DOM of a logged-in Agouti page, and StaticSource replays a snapshot
// captured elsewhere. Refresh merges a snapshot atomically. Re-reading an
// unchanged revision or order is not an error and does not double count it.
//
// An incomplete page (no version marker, no completion marker, or more than
// one ledger root) yields an *IncompleteLedgerError. A revision that was
// already known with different contents yields a *RevisionConflictError.
// Two live revisions for the same price that overlap at the same knowledge
// time yield an *OverlappingPriceError. Any of these aborts the whole
// refresh, leaving stored history exactly as it was.
//
// # Durability
//
// A Warehouse stores every revision (including tombstones) and every order in
// a JSON file. Open loads such a file and Save writes it; a warehouse opened
// from a path persists automatically on later refreshes. Writes are atomic,
// so a failed refresh, including a failed write, leaves the previous file and
// the previous in-memory results untouched. Reopening restores both the
// history and the orders, so both views remain queryable without another page
// read.
package pricing
