package pricing_test

import (
	"errors"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sclevine/agouti/pricing"
)

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("bad fixture time %q: %s", value, err)
	}
	return parsed
}

func revision(priceID string, number int, amount int64, from, to, known string) pricing.PriceRevision {
	fromTime, _ := time.Parse(time.RFC3339, from)
	toTime, _ := time.Parse(time.RFC3339, to)
	knownTime, _ := time.Parse(time.RFC3339, known)
	return pricing.PriceRevision{
		PriceID:     priceID,
		Revision:    number,
		AmountCents: amount,
		ValidFrom:   fromTime,
		ValidTo:     toTime,
		KnownAt:     knownTime,
	}
}

func snapshotOf(readAt string, revisions []pricing.PriceRevision, orders []pricing.Order) pricing.Snapshot {
	readTime, _ := time.Parse(time.RFC3339, readAt)
	return pricing.Snapshot{Version: "v-" + readAt, ReadAt: readTime, Revisions: revisions, Orders: orders}
}

func TestRepriceMatchesOrderBusinessTimeNotLatestPrice(t *testing.T) {
	warehouse := pricing.NewWarehouse()
	_, err := warehouse.Ingest(snapshotOf("2019-03-01T00:00:00Z", []pricing.PriceRevision{
		revision("SKU-1", 1, 1000, "2019-01-01T00:00:00Z", "2019-02-01T00:00:00Z", "2019-01-01T00:00:00Z"),
		revision("SKU-1", 2, 1200, "2019-02-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-02-01T00:00:00Z"),
	}, []pricing.Order{
		{OrderID: "A", BusinessAt: mustTime(t, "2019-01-15T00:00:00Z"), Lines: []pricing.OrderLine{{PriceID: "SKU-1", Qty: 3}}},
	}))
	if err != nil {
		t.Fatalf("ingest: %s", err)
	}

	result, err := warehouse.Now().Reprice("A")
	if err != nil {
		t.Fatalf("reprice: %s", err)
	}
	if got, want := result.Lines[0].Revision, 1; got != want {
		t.Fatalf("revision = %d, want %d (business time must select the price, not the latest)", got, want)
	}
	if got, want := result.Lines[0].UnitCents, int64(1000); got != want {
		t.Fatalf("unit cents = %d, want %d", got, want)
	}
	if got, want := result.TotalCents, int64(3000); got != want {
		t.Fatalf("total = %d, want %d", got, want)
	}
}

func TestHalfOpenIntervalBoundaries(t *testing.T) {
	warehouse := pricing.NewWarehouse()
	_, err := warehouse.Ingest(snapshotOf("2019-03-01T00:00:00Z", []pricing.PriceRevision{
		revision("SKU-1", 1, 500, "2019-01-01T00:00:00Z", "2019-02-01T00:00:00Z", "2019-01-01T00:00:00Z"),
	}, []pricing.Order{
		{OrderID: "start", BusinessAt: mustTime(t, "2019-01-01T00:00:00Z"), Lines: []pricing.OrderLine{{PriceID: "SKU-1", Qty: 1}}},
		{OrderID: "end", BusinessAt: mustTime(t, "2019-02-01T00:00:00Z"), Lines: []pricing.OrderLine{{PriceID: "SKU-1", Qty: 1}}},
	}))
	if err != nil {
		t.Fatalf("ingest: %s", err)
	}

	start, err := warehouse.Now().Reprice("start")
	if err != nil {
		t.Fatalf("reprice start: %s", err)
	}
	if !start.Lines[0].Priced {
		t.Fatalf("order exactly at valid-from must be priced")
	}
	end, err := warehouse.Now().Reprice("end")
	if err != nil {
		t.Fatalf("reprice end: %s", err)
	}
	if end.Lines[0].Priced {
		t.Fatalf("order exactly at valid-to must not be priced (half-open interval)")
	}
}

func TestMissingPriceRetainsUnpricedLine(t *testing.T) {
	warehouse := pricing.NewWarehouse()
	_, err := warehouse.Ingest(snapshotOf("2019-03-01T00:00:00Z", []pricing.PriceRevision{
		revision("SKU-1", 1, 700, "2019-01-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-01-01T00:00:00Z"),
	}, []pricing.Order{
		{OrderID: "A", BusinessAt: mustTime(t, "2019-02-01T00:00:00Z"), Lines: []pricing.OrderLine{
			{PriceID: "SKU-1", Qty: 2},
			{PriceID: "SKU-9", Qty: 4},
		}},
	}))
	if err != nil {
		t.Fatalf("ingest: %s", err)
	}

	result, err := warehouse.Now().Reprice("A")
	if err != nil {
		t.Fatalf("reprice: %s", err)
	}
	if len(result.Lines) != 2 {
		t.Fatalf("lines = %d, want 2 (unpriced lines must be retained)", len(result.Lines))
	}
	if result.Lines[1].Priced {
		t.Fatalf("missing price must be reported as unpriced")
	}
	if got, want := result.Lines[1].AmountCents, int64(0); got != want {
		t.Fatalf("unpriced amount = %d, want 0", got)
	}
	if result.Priced {
		t.Fatalf("order with a missing price must not be fully priced")
	}
	if got, want := result.TotalCents, int64(1400); got != want {
		t.Fatalf("total = %d, want %d (missing line contributes nothing)", got, want)
	}
	if len(result.Missing) != 1 || result.Missing[0] != "SKU-9" {
		t.Fatalf("missing = %v, want [SKU-9]", result.Missing)
	}
}

func TestZeroPriceIsNotMissing(t *testing.T) {
	warehouse := pricing.NewWarehouse()
	_, err := warehouse.Ingest(snapshotOf("2019-03-01T00:00:00Z", []pricing.PriceRevision{
		revision("FREE", 1, 0, "2019-01-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-01-01T00:00:00Z"),
	}, []pricing.Order{
		{OrderID: "A", BusinessAt: mustTime(t, "2019-02-01T00:00:00Z"), Lines: []pricing.OrderLine{{PriceID: "FREE", Qty: 5}}},
	}))
	if err != nil {
		t.Fatalf("ingest: %s", err)
	}
	result, err := warehouse.Now().Reprice("A")
	if err != nil {
		t.Fatalf("reprice: %s", err)
	}
	if !result.Lines[0].Priced {
		t.Fatalf("a zero price must be treated as a real price, not a missing one")
	}
	if result.Lines[0].Revision != 1 {
		t.Fatalf("revision = %d, want 1", result.Lines[0].Revision)
	}
	if result.TotalCents != 0 || !result.Priced {
		t.Fatalf("zero-priced order total = %d priced=%v, want 0 true", result.TotalCents, result.Priced)
	}
	if len(result.Missing) != 0 {
		t.Fatalf("missing = %v, want none", result.Missing)
	}
}

func TestReingestSameRevisionDoesNotCountTwice(t *testing.T) {
	warehouse := pricing.NewWarehouse()
	first := snapshotOf("2019-03-01T00:00:00Z", []pricing.PriceRevision{
		revision("SKU-1", 1, 1000, "2019-01-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-01-01T00:00:00Z"),
	}, []pricing.Order{
		{OrderID: "A", BusinessAt: mustTime(t, "2019-02-01T00:00:00Z"), Lines: []pricing.OrderLine{{PriceID: "SKU-1", Qty: 1}}},
	})
	result, err := warehouse.Ingest(first)
	if err != nil {
		t.Fatalf("first ingest: %s", err)
	}
	if result.NewRevisions != 1 || result.NewOrders != 1 || result.NewLines != 1 {
		t.Fatalf("first ingest counts = %+v, want one of each", result)
	}

	result, err = warehouse.Ingest(first)
	if err != nil {
		t.Fatalf("second ingest: %s", err)
	}
	if result.NewRevisions != 0 || result.NewOrders != 0 || result.NewLines != 0 || result.NewDeletions != 0 {
		t.Fatalf("re-reading the same revision must not double count: %+v", result)
	}
	if got := len(warehouse.Now().Revisions()); got != 1 {
		t.Fatalf("revisions stored = %d, want 1", got)
	}
}

func TestBitemporalViewsSeparateKnowledge(t *testing.T) {
	warehouse := pricing.NewWarehouse()
	// Original price is known in January. In March the supplier makes a
	// retroactive correction for the same business window.
	_, err := warehouse.Ingest(snapshotOf("2019-01-10T00:00:00Z", []pricing.PriceRevision{
		revision("SKU-1", 1, 1000, "2019-01-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-01-01T00:00:00Z"),
	}, []pricing.Order{
		{OrderID: "A", BusinessAt: mustTime(t, "2019-02-01T00:00:00Z"), Lines: []pricing.OrderLine{{PriceID: "SKU-1", Qty: 1}}},
	}))
	if err != nil {
		t.Fatalf("first ingest: %s", err)
	}
	_, err = warehouse.Ingest(snapshotOf("2019-03-10T00:00:00Z", []pricing.PriceRevision{
		revision("SKU-1", 2, 800, "2019-01-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-03-01T00:00:00Z"),
	}, nil))
	if err != nil {
		t.Fatalf("second ingest: %s", err)
	}

	now, err := warehouse.Now().Reprice("A")
	if err != nil {
		t.Fatalf("reprice now: %s", err)
	}
	if got, want := now.Lines[0].UnitCents, int64(800); got != want {
		t.Fatalf("as-known-now unit = %d, want %d (retroactive correction visible)", got, want)
	}

	then, err := warehouse.KnownAt(mustTime(t, "2019-02-01T00:00:00Z")).Reprice("A")
	if err != nil {
		t.Fatalf("reprice then: %s", err)
	}
	if got, want := then.Lines[0].UnitCents, int64(1000); got != want {
		t.Fatalf("as-known-then unit = %d, want %d (later change must not pollute the past view)", got, want)
	}
}

func TestDeletingRevisionDoesNotResurrectOldPrice(t *testing.T) {
	warehouse := pricing.NewWarehouse()
	_, err := warehouse.Ingest(snapshotOf("2019-01-01T00:00:00Z", []pricing.PriceRevision{
		revision("SKU-1", 1, 1000, "2019-01-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-01-01T00:00:00Z"),
	}, []pricing.Order{
		{OrderID: "A", BusinessAt: mustTime(t, "2019-02-01T00:00:00Z"), Lines: []pricing.OrderLine{{PriceID: "SKU-1", Qty: 1}}},
	}))
	if err != nil {
		t.Fatalf("first ingest: %s", err)
	}
	// Revision 2 supersedes revision 1 for the same window.
	_, err = warehouse.Ingest(snapshotOf("2019-02-01T00:00:00Z", []pricing.PriceRevision{
		revision("SKU-1", 2, 1200, "2019-01-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-02-01T00:00:00Z"),
	}, nil))
	if err != nil {
		t.Fatalf("second ingest: %s", err)
	}
	// Now the supplier deletes revision 2.
	deleted := revision("SKU-1", 2, 1200, "2019-01-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-02-01T00:00:00Z")
	deleted.Deleted = true
	deleted.DeletedAt = mustTime(t, "2019-03-01T00:00:00Z")
	_, err = warehouse.Ingest(snapshotOf("2019-03-01T00:00:00Z", []pricing.PriceRevision{deleted}, nil))
	if err != nil {
		t.Fatalf("delete ingest: %s", err)
	}

	result, err := warehouse.Now().Reprice("A")
	if err != nil {
		t.Fatalf("reprice: %s", err)
	}
	if result.Lines[0].Priced {
		t.Fatalf("deleting the revision must not resurrect revision 1's old price; got revision %d", result.Lines[0].Revision)
	}

	// A past view still sees revision 2 as live, because the deletion was not
	// yet known then.
	then, err := warehouse.KnownAt(mustTime(t, "2019-02-15T00:00:00Z")).Reprice("A")
	if err != nil {
		t.Fatalf("reprice then: %s", err)
	}
	if !then.Lines[0].Priced || then.Lines[0].Revision != 2 {
		t.Fatalf("past view = revision %d priced=%v, want revision 2 priced", then.Lines[0].Revision, then.Lines[0].Priced)
	}
}

func TestLiveReadDoesNotReviveDeletedRevision(t *testing.T) {
	warehouse := pricing.NewWarehouse()
	deleted := revision("SKU-1", 1, 1000, "2019-01-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-01-01T00:00:00Z")
	deleted.Deleted = true
	deleted.DeletedAt = mustTime(t, "2019-02-01T00:00:00Z")
	if _, err := warehouse.Ingest(snapshotOf("2019-02-01T00:00:00Z", []pricing.PriceRevision{deleted}, nil)); err != nil {
		t.Fatalf("ingest deletion: %s", err)
	}
	live := revision("SKU-1", 1, 1000, "2019-01-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-01-01T00:00:00Z")
	if _, err := warehouse.Ingest(snapshotOf("2019-03-01T00:00:00Z", []pricing.PriceRevision{live}, nil)); err != nil {
		t.Fatalf("ingest live: %s", err)
	}
	visible := warehouse.Now().Revisions()
	if len(visible) != 1 || !visible[0].Deleted {
		t.Fatalf("deleted revision must stay deleted after a live re-read: %+v", visible)
	}
}

func TestConflictingRevisionFailsAndPreservesState(t *testing.T) {
	warehouse := pricing.NewWarehouse()
	if _, err := warehouse.Ingest(snapshotOf("2019-01-01T00:00:00Z", []pricing.PriceRevision{
		revision("SKU-1", 1, 1000, "2019-01-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-01-01T00:00:00Z"),
	}, nil)); err != nil {
		t.Fatalf("first ingest: %s", err)
	}

	_, err := warehouse.Ingest(snapshotOf("2019-02-01T00:00:00Z", []pricing.PriceRevision{
		revision("SKU-1", 1, 999, "2019-01-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-01-01T00:00:00Z"),
	}, nil))
	if _, ok := err.(*pricing.RevisionConflictError); !ok {
		t.Fatalf("error = %v, want *RevisionConflictError", err)
	}
	visible := warehouse.Now().Revisions()
	if len(visible) != 1 || visible[0].AmountCents != 1000 {
		t.Fatalf("stored history changed after a conflict: %+v", visible)
	}
}

func TestEqualStandingOverlapFailsAtIngest(t *testing.T) {
	warehouse := pricing.NewWarehouse()
	_, err := warehouse.Ingest(snapshotOf("2019-01-01T00:00:00Z", []pricing.PriceRevision{
		revision("SKU-1", 1, 1000, "2019-01-01T00:00:00Z", "2019-03-01T00:00:00Z", "2019-01-01T00:00:00Z"),
		revision("SKU-1", 2, 1200, "2019-02-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-01-01T00:00:00Z"),
	}, nil))
	if _, ok := err.(*pricing.OverlappingPriceError); !ok {
		t.Fatalf("error = %v, want *OverlappingPriceError", err)
	}
	if got := len(warehouse.Now().Revisions()); got != 0 {
		t.Fatalf("failed ingest stored %d revisions, want 0", got)
	}
}

func TestDeletionConflictFails(t *testing.T) {
	warehouse := pricing.NewWarehouse()
	if _, err := warehouse.Ingest(snapshotOf("2019-01-01T00:00:00Z", []pricing.PriceRevision{
		revision("SKU-1", 1, 1000, "2019-01-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-01-01T00:00:00Z"),
	}, nil)); err != nil {
		t.Fatalf("first ingest: %s", err)
	}
	first := revision("SKU-1", 1, 1000, "2019-01-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-01-01T00:00:00Z")
	first.Deleted, first.DeletedAt = true, mustTime(t, "2019-02-01T00:00:00Z")
	if _, err := warehouse.Ingest(snapshotOf("2019-02-01T00:00:00Z", []pricing.PriceRevision{first}, nil)); err != nil {
		t.Fatalf("delete ingest: %s", err)
	}
	second := first
	second.DeletedAt = mustTime(t, "2019-03-01T00:00:00Z")
	_, err := warehouse.Ingest(snapshotOf("2019-03-01T00:00:00Z", []pricing.PriceRevision{second}, nil))
	if _, ok := err.(*pricing.DeletionConflictError); !ok {
		t.Fatalf("error = %v, want *DeletionConflictError", err)
	}
}

func TestInvalidRevisionRejected(t *testing.T) {
	cases := map[string]pricing.PriceRevision{
		"negative amount":    revision("SKU-1", 1, -5, "2019-01-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-01-01T00:00:00Z"),
		"backwards interval": revision("SKU-1", 1, 5, "2019-04-01T00:00:00Z", "2019-01-01T00:00:00Z", "2019-01-01T00:00:00Z"),
		"empty price id":     revision("", 1, 5, "2019-01-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-01-01T00:00:00Z"),
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			warehouse := pricing.NewWarehouse()
			_, err := warehouse.Ingest(snapshotOf("2019-01-01T00:00:00Z", []pricing.PriceRevision{bad}, nil))
			if _, ok := err.(*pricing.InvalidDataError); !ok {
				t.Fatalf("error = %v, want *InvalidDataError", err)
			}
		})
	}
}

func TestOverflowFails(t *testing.T) {
	warehouse := pricing.NewWarehouse()
	_, err := warehouse.Ingest(snapshotOf("2019-03-01T00:00:00Z", []pricing.PriceRevision{
		revision("SKU-1", 1, 1<<62, "2019-01-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-01-01T00:00:00Z"),
	}, []pricing.Order{
		{OrderID: "A", BusinessAt: mustTime(t, "2019-02-01T00:00:00Z"), Lines: []pricing.OrderLine{{PriceID: "SKU-1", Qty: 4}}},
	}))
	if err != nil {
		t.Fatalf("ingest: %s", err)
	}
	_, err = warehouse.Now().Reprice("A")
	if _, ok := err.(*pricing.OverflowError); !ok {
		t.Fatalf("error = %v, want *OverflowError", err)
	}
}

func TestOrderConflicts(t *testing.T) {
	base := pricing.Order{OrderID: "A", BusinessAt: mustTime(t, "2019-02-01T00:00:00Z"), Lines: []pricing.OrderLine{{PriceID: "SKU-1", Qty: 1}}}
	warehouse := pricing.NewWarehouse()
	if _, err := warehouse.Ingest(snapshotOf("2019-01-01T00:00:00Z", nil, []pricing.Order{base})); err != nil {
		t.Fatalf("first ingest: %s", err)
	}

	t.Run("moved business time", func(t *testing.T) {
		moved := base
		moved.BusinessAt = mustTime(t, "2019-03-01T00:00:00Z")
		_, err := warehouse.Ingest(snapshotOf("2019-02-01T00:00:00Z", nil, []pricing.Order{moved}))
		if _, ok := err.(*pricing.OrderConflictError); !ok {
			t.Fatalf("error = %v, want *OrderConflictError", err)
		}
	})

	t.Run("changed line quantity", func(t *testing.T) {
		changed := base
		changed.Lines = []pricing.OrderLine{{PriceID: "SKU-1", Qty: 9}}
		_, err := warehouse.Ingest(snapshotOf("2019-02-01T00:00:00Z", nil, []pricing.Order{changed}))
		if _, ok := err.(*pricing.OrderConflictError); !ok {
			t.Fatalf("error = %v, want *OrderConflictError", err)
		}
	})

	t.Run("added line is merged", func(t *testing.T) {
		added := base
		added.Lines = []pricing.OrderLine{{PriceID: "SKU-1", Qty: 1}, {PriceID: "SKU-2", Qty: 2}}
		result, err := warehouse.Ingest(snapshotOf("2019-02-01T00:00:00Z", nil, []pricing.Order{added}))
		if err != nil {
			t.Fatalf("ingest added line: %s", err)
		}
		if result.NewLines != 1 || result.NewOrders != 0 {
			t.Fatalf("counts = %+v, want 1 new line and 0 new orders", result)
		}
		order, err := warehouse.Now().Order("A")
		if err != nil {
			t.Fatalf("order: %s", err)
		}
		if len(order.Lines) != 2 {
			t.Fatalf("lines = %d, want 2", len(order.Lines))
		}
	})
}

func TestRepriceUnknownOrder(t *testing.T) {
	_, err := pricing.NewWarehouse().Now().Reprice("nope")
	if _, ok := err.(*pricing.OrderNotFoundError); !ok {
		t.Fatalf("error = %v, want *OrderNotFoundError", err)
	}
}

func TestPersistenceRoundTripKeepsBothViews(t *testing.T) {
	directory, err := ioutil.TempDir("", "pricing-warehouse-")
	if err != nil {
		t.Fatalf("temp dir: %s", err)
	}
	defer os.RemoveAll(directory)
	path := filepath.Join(directory, "warehouse.json")

	warehouse, err := pricing.Open(path)
	if err != nil {
		t.Fatalf("open empty: %s", err)
	}
	if _, err := warehouse.Ingest(snapshotOf("2019-01-10T00:00:00Z", []pricing.PriceRevision{
		revision("SKU-1", 1, 1000, "2019-01-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-01-01T00:00:00Z"),
	}, []pricing.Order{
		{OrderID: "A", BusinessAt: mustTime(t, "2019-02-01T00:00:00Z"), Lines: []pricing.OrderLine{{PriceID: "SKU-1", Qty: 2}}},
	})); err != nil {
		t.Fatalf("first ingest: %s", err)
	}
	if _, err := warehouse.Ingest(snapshotOf("2019-03-10T00:00:00Z", []pricing.PriceRevision{
		revision("SKU-1", 2, 800, "2019-01-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-03-01T00:00:00Z"),
	}, nil)); err != nil {
		t.Fatalf("second ingest: %s", err)
	}

	reopened, err := pricing.Open(path)
	if err != nil {
		t.Fatalf("reopen: %s", err)
	}
	now, err := reopened.Now().Reprice("A")
	if err != nil {
		t.Fatalf("reprice now: %s", err)
	}
	if got, want := now.Lines[0].UnitCents, int64(800); got != want {
		t.Fatalf("reopened as-known-now unit = %d, want %d", got, want)
	}
	if got, want := now.TotalCents, int64(1600); got != want {
		t.Fatalf("reopened total = %d, want %d", got, want)
	}
	then, err := reopened.KnownAt(mustTime(t, "2019-02-01T00:00:00Z")).Reprice("A")
	if err != nil {
		t.Fatalf("reprice then: %s", err)
	}
	if got, want := then.Lines[0].UnitCents, int64(1000); got != want {
		t.Fatalf("reopened as-known-then unit = %d, want %d", got, want)
	}
}

func TestRefreshFailureKeepsOldResults(t *testing.T) {
	warehouse := pricing.NewWarehouse()
	if _, err := warehouse.Ingest(snapshotOf("2019-01-01T00:00:00Z", []pricing.PriceRevision{
		revision("SKU-1", 1, 1000, "2019-01-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-01-01T00:00:00Z"),
	}, nil)); err != nil {
		t.Fatalf("seed: %s", err)
	}

	source := pricing.SourceFunc(func() (pricing.Snapshot, error) {
		return pricing.Snapshot{}, errors.New("network down")
	})
	if _, err := warehouse.Refresh(source); err == nil {
		t.Fatalf("expected refresh error")
	}
	visible := warehouse.Now().Revisions()
	if len(visible) != 1 || visible[0].AmountCents != 1000 {
		t.Fatalf("failed refresh changed stored history: %+v", visible)
	}
}

func TestRetroactiveDeletionInvisibleInPastView(t *testing.T) {
	warehouse := pricing.NewWarehouse()
	original := revision("SKU-1", 1, 1000, "2019-01-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-01-01T00:00:00Z")
	if _, err := warehouse.Ingest(snapshotOf("2019-01-05T00:00:00Z", []pricing.PriceRevision{original}, []pricing.Order{
		{OrderID: "A", BusinessAt: mustTime(t, "2019-02-01T00:00:00Z"), Lines: []pricing.OrderLine{{PriceID: "SKU-1", Qty: 1}}},
	})); err != nil {
		t.Fatalf("first ingest: %s", err)
	}
	// In March the supplier deletes the only revision retroactively: the order
	// placed in February never had a price after all.
	deleted := original
	deleted.Deleted, deleted.DeletedAt = true, mustTime(t, "2019-03-01T00:00:00Z")
	if _, err := warehouse.Ingest(snapshotOf("2019-03-01T00:00:00Z", []pricing.PriceRevision{deleted}, nil)); err != nil {
		t.Fatalf("delete ingest: %s", err)
	}

	now, err := warehouse.Now().Reprice("A")
	if err != nil {
		t.Fatalf("reprice now: %s", err)
	}
	if now.Lines[0].Priced {
		t.Fatalf("as-known-now must see the deletion and report the line unpriced")
	}

	then, err := warehouse.KnownAt(mustTime(t, "2019-02-15T00:00:00Z")).Reprice("A")
	if err != nil {
		t.Fatalf("reprice then: %s", err)
	}
	if !then.Lines[0].Priced || then.Lines[0].UnitCents != 1000 {
		t.Fatalf("as-known-then must still see the price: priced=%v unit=%d", then.Lines[0].Priced, then.Lines[0].UnitCents)
	}
}

func TestOrdersVisibleInEveryView(t *testing.T) {
	warehouse := pricing.NewWarehouse()
	order := pricing.Order{OrderID: "A", BusinessAt: mustTime(t, "2019-02-01T00:00:00Z"), Lines: []pricing.OrderLine{{PriceID: "SKU-1", Qty: 1}}}
	if _, err := warehouse.Ingest(snapshotOf("2019-05-01T00:00:00Z", []pricing.PriceRevision{
		revision("SKU-1", 1, 1000, "2019-01-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-01-01T00:00:00Z"),
	}, []pricing.Order{order})); err != nil {
		t.Fatalf("ingest: %s", err)
	}
	if _, err := warehouse.KnownAt(mustTime(t, "2019-01-15T00:00:00Z")).Order("A"); err != nil {
		t.Fatalf("orders are facts and must be visible in a past view: %s", err)
	}
}

func TestSameKnowledgeTimeOverlapIsAmbiguous(t *testing.T) {
	warehouse := pricing.NewWarehouse()
	// Two live revisions with the same knowledge time overlap; the supplier's
	// completed version is internally ambiguous and must fail on ingest.
	_, err := warehouse.Ingest(snapshotOf("2019-01-01T00:00:00Z", []pricing.PriceRevision{
		revision("SKU-1", 5, 1000, "2019-01-01T00:00:00Z", "2019-03-01T00:00:00Z", "2019-01-01T00:00:00Z"),
		revision("SKU-1", 6, 1100, "2019-02-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-01-01T00:00:00Z"),
	}, nil))
	if err == nil {
		t.Fatalf("expected ambiguity to fail")
	}
}

func TestRepriceAll(t *testing.T) {
	warehouse := pricing.NewWarehouse()
	if _, err := warehouse.Ingest(snapshotOf("2019-03-01T00:00:00Z", []pricing.PriceRevision{
		revision("SKU-1", 1, 100, "2019-01-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-01-01T00:00:00Z"),
	}, []pricing.Order{
		{OrderID: "B", BusinessAt: mustTime(t, "2019-02-01T00:00:00Z"), Lines: []pricing.OrderLine{{PriceID: "SKU-1", Qty: 1}}},
		{OrderID: "A", BusinessAt: mustTime(t, "2019-02-01T00:00:00Z"), Lines: []pricing.OrderLine{{PriceID: "SKU-1", Qty: 2}}},
	})); err != nil {
		t.Fatalf("ingest: %s", err)
	}
	results, err := warehouse.Now().RepriceAll()
	if err != nil {
		t.Fatalf("reprice all: %s", err)
	}
	if len(results) != 2 || results[0].OrderID != "A" || results[0].TotalCents != 200 || results[1].TotalCents != 100 {
		t.Fatalf("unexpected reprice-all results: %+v", results)
	}
}

func TestFailedPersistKeepsOldResultsAndFile(t *testing.T) {
	directory, err := ioutil.TempDir("", "pricing-persist-failure-")
	if err != nil {
		t.Fatalf("temp dir: %s", err)
	}
	defer os.Chmod(directory, 0700)
	defer os.RemoveAll(directory)
	path := filepath.Join(directory, "warehouse.json")

	warehouse, err := pricing.Open(path)
	if err != nil {
		t.Fatalf("open: %s", err)
	}
	if _, err := warehouse.Ingest(snapshotOf("2019-01-01T00:00:00Z", []pricing.PriceRevision{
		revision("SKU-1", 1, 1000, "2019-01-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-01-01T00:00:00Z"),
	}, nil)); err != nil {
		t.Fatalf("seed ingest: %s", err)
	}

	// Make the directory unwritable so the next atomic write cannot create its
	// temporary file. This simulates a disk failure during a later refresh.
	if err := os.Chmod(directory, 0500); err != nil {
		t.Fatalf("chmod: %s", err)
	}
	_, err = warehouse.Ingest(snapshotOf("2019-02-01T00:00:00Z", []pricing.PriceRevision{
		revision("SKU-2", 1, 500, "2019-01-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-02-01T00:00:00Z"),
	}, nil))
	if err == nil {
		t.Skip("directory is writable despite chmod (running as root?)")
	}

	// The in-memory history must still hold only the originally seeded price.
	if got := len(warehouse.Now().Revisions()); got != 1 {
		t.Fatalf("failed persist changed stored history: %d revisions, want 1", got)
	}
	// And the file on disk must still contain only the seeded price.
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatalf("restore chmod: %s", err)
	}
	reopened, err := pricing.Open(path)
	if err != nil {
		t.Fatalf("reopen: %s", err)
	}
	if got := len(reopened.Now().Revisions()); got != 1 {
		t.Fatalf("failed persist changed the file: %d revisions, want 1", got)
	}
}

func TestSaveBindsPathForLaterRefreshes(t *testing.T) {
	directory, err := ioutil.TempDir("", "pricing-save-")
	if err != nil {
		t.Fatalf("temp dir: %s", err)
	}
	defer os.RemoveAll(directory)
	path := filepath.Join(directory, "warehouse.json")

	warehouse := pricing.NewWarehouse()
	if _, err := warehouse.Ingest(snapshotOf("2019-01-01T00:00:00Z", []pricing.PriceRevision{
		revision("SKU-1", 1, 1000, "2019-01-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-01-01T00:00:00Z"),
	}, nil)); err != nil {
		t.Fatalf("first ingest: %s", err)
	}
	if err := warehouse.Save(path); err != nil {
		t.Fatalf("save: %s", err)
	}
	if warehouse.Path() != path {
		t.Fatalf("path = %q, want %q", warehouse.Path(), path)
	}
	if _, err := warehouse.Ingest(snapshotOf("2019-02-01T00:00:00Z", []pricing.PriceRevision{
		revision("SKU-2", 1, 500, "2019-01-01T00:00:00Z", "2019-04-01T00:00:00Z", "2019-02-01T00:00:00Z"),
	}, nil)); err != nil {
		t.Fatalf("later ingest: %s", err)
	}
	reopened, err := pricing.Open(path)
	if err != nil {
		t.Fatalf("reopen: %s", err)
	}
	if got := len(reopened.Now().Revisions()); got != 2 {
		t.Fatalf("reopened revisions = %d, want 2 (refresh should persist after Save)", got)
	}
}
