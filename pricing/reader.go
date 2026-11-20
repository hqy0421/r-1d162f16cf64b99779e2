package pricing

import (
	"errors"
	"fmt"
	"time"
)

// Source is something a Warehouse can refresh from. Read must return a
// complete ledger snapshot or an error; it must never return a partial
// snapshot, because Ingest refuses incomplete versions.
type Source interface {
	Read() (Snapshot, error)
}

// SourceFunc adapts a function to Source.
type SourceFunc func() (Snapshot, error)

// Read calls f.
func (f SourceFunc) Read() (Snapshot, error) {
	return f()
}

// StaticSource returns a Source that always yields the given snapshot. It is
// useful in tests and for replaying a captured page.
func StaticSource(snapshot Snapshot) Source {
	return SourceFunc(func() (Snapshot, error) { return snapshot, nil })
}

// Page is the subset of agouti.Page used to read a logged-in ledger page.
type Page interface {
	// HTML returns the current DOM of the page.
	HTML() (string, error)
	// RunScript runs JavaScript in the page and decodes a return value into
	// result.
	RunScript(body string, arguments map[string]interface{}, result interface{}) error
}

// Reader reads a completed ledger version from a logged-in page. It reads the
// ledger's version marker and its price and order elements from the real DOM
// of the page; it does not run against a cached or pre-fetched copy.
//
// The page is expected to expose a completed ledger view (for example after a
// "finalise" step). If the version marker is missing or the page reports that
// the ledger is not complete, Read fails with *IncompleteLedgerError and the
// warehouse keeps whatever it had before.
type Reader struct {
	// Page is the logged-in page to read. It is required.
	Page Page
	// Clock supplies the knowledge time recorded for the read. It defaults
	// to time.Now.
	Clock func() time.Time
	// Selectors customise where the reader looks. Zero values select the
	// default data attributes documented on DefaultSelectors.
	Selectors Selectors
}

// Selectors names the CSS selectors and attributes the reader uses to find
// the ledger in the page's DOM. Zero values select the defaults documented on
// DefaultSelectors.
type Selectors struct {
	// Ledger selects the single element holding the completed ledger. It
	// must match exactly one element.
	Ledger string
	// Version selects an element inside the ledger whose text is the
	// completed version marker.
	Version string
	// Complete selects an element inside the ledger that must exist for the
	// ledger to be considered complete.
	Complete string
	// Revisions selects one element per price revision inside the ledger.
	Revisions string
	// Orders selects one element per order inside the ledger.
	Orders string
	// Lines selects one element per order line inside an order element.
	Lines string
	// Attributes maps the fields below to the DOM attributes carrying them.
	Attributes Attributes
}

// Attributes names the DOM attributes the reader reads. Zero values select
// the defaults documented on DefaultSelectors.
type Attributes struct {
	PriceID    string
	Revision   string
	Amount     string
	ValidFrom  string
	ValidTo    string
	KnownAt    string
	Deleted    string
	DeletedAt  string
	OrderID    string
	BusinessAt string
	Qty        string
}

// DefaultSelectors are the selectors and attributes Reader uses when none are
// provided. Each ledger element is a plain DOM node carrying data attributes:
//
//	[data-ledger]                       the ledger root
//	[data-ledger] [data-version]        completed version marker
//	[data-ledger] [data-complete]       completion marker
//	[data-ledger] [data-price-revision] a price revision node with
//	                                    data-price-id, data-revision, data-amount,
//	                                    data-valid-from, data-valid-to,
//	                                    data-known-at, data-deleted,
//	                                    data-deleted-at
//	[data-ledger] [data-order]          an order node with data-order-id and
//	                                    data-business-at
//	[data-order] [data-line]            a line node with data-price-id and
//	                                    data-qty
var DefaultSelectors = Selectors{
	Ledger:    "[data-ledger]",
	Version:   "[data-ledger] [data-version]",
	Complete:  "[data-complete]",
	Revisions: "[data-ledger] [data-price-revision]",
	Orders:    "[data-ledger] [data-order]",
	Lines:     "[data-order] [data-line]",
	Attributes: Attributes{
		PriceID:    "data-price-id",
		Revision:   "data-revision",
		Amount:     "data-amount",
		ValidFrom:  "data-valid-from",
		ValidTo:    "data-valid-to",
		KnownAt:    "data-known-at",
		Deleted:    "data-deleted",
		DeletedAt:  "data-deleted-at",
		OrderID:    "data-order-id",
		BusinessAt: "data-business-at",
		Qty:        "data-qty",
	},
}

// DefaultClock returns UTC time.Now.
func DefaultClock() time.Time {
	return time.Now().UTC()
}

func (r *Reader) selectors() Selectors {
	selectors := r.Selectors
	if selectors.Ledger == "" {
		selectors.Ledger = DefaultSelectors.Ledger
	}
	if selectors.Version == "" {
		selectors.Version = DefaultSelectors.Version
	}
	if selectors.Complete == "" {
		selectors.Complete = DefaultSelectors.Complete
	}
	if selectors.Revisions == "" {
		selectors.Revisions = DefaultSelectors.Revisions
	}
	if selectors.Orders == "" {
		selectors.Orders = DefaultSelectors.Orders
	}
	if selectors.Lines == "" {
		selectors.Lines = DefaultSelectors.Lines
	}
	selectors.Attributes = mergeAttributes(DefaultSelectors.Attributes, selectors.Attributes)
	return selectors
}

func mergeAttributes(defaults, overrides Attributes) Attributes {
	if overrides.PriceID == "" {
		overrides.PriceID = defaults.PriceID
	}
	if overrides.Revision == "" {
		overrides.Revision = defaults.Revision
	}
	if overrides.Amount == "" {
		overrides.Amount = defaults.Amount
	}
	if overrides.ValidFrom == "" {
		overrides.ValidFrom = defaults.ValidFrom
	}
	if overrides.ValidTo == "" {
		overrides.ValidTo = defaults.ValidTo
	}
	if overrides.KnownAt == "" {
		overrides.KnownAt = defaults.KnownAt
	}
	if overrides.Deleted == "" {
		overrides.Deleted = defaults.Deleted
	}
	if overrides.DeletedAt == "" {
		overrides.DeletedAt = defaults.DeletedAt
	}
	if overrides.OrderID == "" {
		overrides.OrderID = defaults.OrderID
	}
	if overrides.BusinessAt == "" {
		overrides.BusinessAt = defaults.BusinessAt
	}
	if overrides.Qty == "" {
		overrides.Qty = defaults.Qty
	}
	return overrides
}

// Read reads a completed ledger snapshot from the page.
func (r *Reader) Read() (Snapshot, error) {
	if r.Page == nil {
		return Snapshot{}, &IncompleteLedgerError{Reason: "no page provided"}
	}
	clock := r.Clock
	if clock == nil {
		clock = DefaultClock
	}
	readAt := clock().UTC()

	html, err := r.Page.HTML()
	if err != nil {
		return Snapshot{}, fmt.Errorf("pricing: reading ledger page: %s", err)
	}
	if html == "" {
		return Snapshot{}, &IncompleteLedgerError{Reason: "page has no content"}
	}

	selectors := r.selectors()
	var raw rawLedger
	script := readScript(selectors)
	if err := r.Page.RunScript(script, nil, &raw); err != nil {
		return Snapshot{}, fmt.Errorf("pricing: reading ledger DOM: %s", err)
	}
	return raw.snapshot(readAt)
}

// rawLedger mirrors the JSON produced by readScript.
type rawLedger struct {
	Found     bool          `json:"found"`
	Complete  bool          `json:"complete"`
	Version   string        `json:"version"`
	Revisions []rawRevision `json:"revisions"`
	Orders    []rawOrder    `json:"orders"`
}

type rawRevision struct {
	PriceID   string `json:"priceId"`
	Revision  number `json:"revision"`
	Amount    number `json:"amount"`
	ValidFrom string `json:"validFrom"`
	ValidTo   string `json:"validTo"`
	KnownAt   string `json:"knownAt"`
	Deleted   bool   `json:"deleted"`
	DeletedAt string `json:"deletedAt"`
}

type rawOrder struct {
	OrderID    string    `json:"orderId"`
	BusinessAt string    `json:"businessAt"`
	Lines      []rawLine `json:"lines"`
}

type rawLine struct {
	PriceID string `json:"priceId"`
	Qty     number `json:"qty"`
}

// number accepts a JSON number or a JSON string holding a number. The page
// may render attribute values as text, and the JSON boundary is where those
// strings are converted.
type number struct {
	value int64
	set   bool
}

func (n *number) UnmarshalJSON(data []byte) error {
	text := string(data)
	if text == "null" {
		return nil
	}
	if len(text) >= 2 && text[0] == '"' && text[len(text)-1] == '"' {
		text = text[1 : len(text)-1]
	}
	trimmed := trimSpace(text)
	if trimmed == "" {
		return nil
	}
	value, err := parseInt(trimmed)
	if err != nil {
		return errors.New("not a whole number: " + string(data))
	}
	n.value, n.set = value, true
	return nil
}

func (l rawLedger) snapshot(readAt time.Time) (Snapshot, error) {
	if !l.Found {
		return Snapshot{}, &IncompleteLedgerError{Reason: "no ledger element on the page"}
	}
	if !l.Complete {
		return Snapshot{}, &IncompleteLedgerError{Reason: "ledger is not complete"}
	}
	if trimSpace(l.Version) == "" {
		return Snapshot{}, &IncompleteLedgerError{Reason: "no completed version marker"}
	}

	snapshot := Snapshot{
		Version: trimSpace(l.Version),
		ReadAt:  readAt,
	}
	for _, revision := range l.Revisions {
		converted, err := revision.convert()
		if err != nil {
			return Snapshot{}, err
		}
		snapshot.Revisions = append(snapshot.Revisions, converted)
	}
	for _, order := range l.Orders {
		converted, err := order.convert()
		if err != nil {
			return Snapshot{}, err
		}
		snapshot.Orders = append(snapshot.Orders, converted)
	}
	return snapshot, nil
}

func (r rawRevision) convert() (PriceRevision, error) {
	if trimSpace(r.PriceID) == "" {
		return PriceRevision{}, &IncompleteLedgerError{Reason: "a price revision has no price ID"}
	}
	if !r.Revision.set {
		return PriceRevision{}, &IncompleteLedgerError{Reason: "price " + r.PriceID + " has no revision number"}
	}
	if !r.Amount.set {
		return PriceRevision{}, &IncompleteLedgerError{
			Reason: "price " + r.PriceID + " revision has no amount",
		}
	}
	validFrom, err := parseTime(r.ValidFrom)
	if err != nil {
		return PriceRevision{}, &IncompleteLedgerError{
			Reason: "price " + r.PriceID + " has an invalid valid-from: " + err.Error(),
		}
	}
	validTo, err := parseTime(r.ValidTo)
	if err != nil {
		return PriceRevision{}, &IncompleteLedgerError{
			Reason: "price " + r.PriceID + " has an invalid valid-to: " + err.Error(),
		}
	}
	revision := PriceRevision{
		PriceID:     trimSpace(r.PriceID),
		Revision:    int(r.Revision.value),
		AmountCents: r.Amount.value,
		ValidFrom:   validFrom,
		ValidTo:     validTo,
	}
	if r.KnownAt != "" {
		knownAt, err := parseTime(r.KnownAt)
		if err != nil {
			return PriceRevision{}, &IncompleteLedgerError{
				Reason: "price " + r.PriceID + " has an invalid known-at: " + err.Error(),
			}
		}
		revision.KnownAt = knownAt
	}
	if r.Deleted {
		revision.Deleted = true
		if r.DeletedAt != "" {
			deletedAt, err := parseTime(r.DeletedAt)
			if err != nil {
				return PriceRevision{}, &IncompleteLedgerError{
					Reason: "price " + r.PriceID + " has an invalid deleted-at: " + err.Error(),
				}
			}
			revision.DeletedAt = deletedAt
		}
	}
	return revision, nil
}

func (o rawOrder) convert() (Order, error) {
	if trimSpace(o.OrderID) == "" {
		return Order{}, &IncompleteLedgerError{Reason: "an order has no order ID"}
	}
	businessAt, err := parseTime(o.BusinessAt)
	if err != nil {
		return Order{}, &IncompleteLedgerError{
			Reason: "order " + o.OrderID + " has an invalid business-at: " + err.Error(),
		}
	}
	order := Order{OrderID: trimSpace(o.OrderID), BusinessAt: businessAt}
	for _, line := range o.Lines {
		if trimSpace(line.PriceID) == "" {
			return Order{}, &IncompleteLedgerError{
				Reason: "order " + o.OrderID + " has a line with no price ID",
			}
		}
		if !line.Qty.set {
			return Order{}, &IncompleteLedgerError{
				Reason: "order " + o.OrderID + " line " + line.PriceID + " has no quantity",
			}
		}
		order.Lines = append(order.Lines, OrderLine{
			PriceID: trimSpace(line.PriceID),
			Qty:     line.Qty.value,
		})
	}
	return order, nil
}
