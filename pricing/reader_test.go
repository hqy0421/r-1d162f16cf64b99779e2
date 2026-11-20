package pricing_test

import (
	"errors"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sclevine/agouti"
	"github.com/sclevine/agouti/pricing"
)

// ledgerPage is a completed supplier ledger rendered as data attributes, the
// shape agouti.Page reads from the live DOM.
const ledgerPage = `<!doctype html>
<html><body>
<div data-ledger data-complete>
  <h1>Supplier ledger</h1>
  <span data-version>2026-11-20T10:00:00Z</span>
  <ul>
    <li data-price-revision data-price-id="SKU-1" data-revision="1" data-amount="1000"
        data-valid-from="2026-01-01T00:00:00Z" data-valid-to="2026-02-01T00:00:00Z"
        data-known-at="2026-01-01T00:00:00Z"></li>
    <li data-price-revision data-price-id="SKU-1" data-revision="2" data-amount="1200"
        data-valid-from="2026-02-01T00:00:00Z" data-valid-to="2026-04-01T00:00:00Z"
        data-known-at="2026-02-01T00:00:00Z"></li>
    <li data-price-revision data-price-id="SKU-DEL" data-revision="1" data-amount="900"
        data-valid-from="2026-01-01T00:00:00Z" data-valid-to="2026-04-01T00:00:00Z"
        data-known-at="2026-01-01T00:00:00Z"
        data-deleted="true" data-deleted-at="2026-03-01T00:00:00Z"></li>
    <li data-price-revision data-price-id="SKU-FREE" data-revision="1" data-amount="0"
        data-valid-from="2026-01-01T00:00:00Z" data-valid-to="2026-04-01T00:00:00Z"
        data-known-at="2026-01-01T00:00:00Z"></li>
  </ul>
  <div data-order data-order-id="ORD-1" data-business-at="2026-01-15T00:00:00Z">
    <span data-line data-price-id="SKU-1" data-qty="2"></span>
    <span data-line data-price-id="SKU-FREE" data-qty="1"></span>
  </div>
  <div data-order data-order-id="ORD-2" data-business-at="2026-02-15T00:00:00Z">
    <span data-line data-price-id="SKU-1" data-qty="3"></span>
    <span data-line data-price-id="SKU-DEL" data-qty="1"></span>
    <span data-line data-price-id="SKU-MISSING" data-qty="4"></span>
  </div>
</div>
</body></html>`

func staticPage(html string) pricing.Page {
	return &fixedPage{html: html}
}

type fixedPage struct {
	html string
	err  error
}

func (p *fixedPage) HTML() (string, error) {
	return p.html, p.err
}

func (p *fixedPage) RunScript(body string, arguments map[string]interface{}, result interface{}) error {
	return errors.New("fixedPage does not execute scripts")
}

// domPage serves the ledger over HTTP and reads it through a real browser.
type domPage struct {
	page *agouti.Page
}

func (p *domPage) HTML() (string, error) { return p.page.HTML() }

func (p *domPage) RunScript(body string, arguments map[string]interface{}, result interface{}) error {
	return p.page.RunScript(body, arguments, result)
}

func TestReaderParsesLiveDOM(t *testing.T) {
	driver := agouti.PhantomJS()
	if err := driver.Start(); err != nil {
		t.Skipf("PhantomJS unavailable: %s", err)
	}
	defer driver.Stop()

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Write([]byte(ledgerPage))
	}))
	defer server.Close()

	page, err := driver.NewPage()
	if err != nil {
		t.Fatalf("new page: %s", err)
	}
	defer page.Destroy()
	if err := page.Navigate(server.URL); err != nil {
		t.Fatalf("navigate: %s", err)
	}

	readAt, _ := time.Parse(time.RFC3339, "2026-11-20T11:00:00Z")
	reader := &pricing.Reader{
		Page:  &domPage{page},
		Clock: func() time.Time { return readAt },
	}
	snapshot, err := reader.Read()
	if err != nil {
		t.Fatalf("read: %s", err)
	}
	if snapshot.Version != "2026-11-20T10:00:00Z" {
		t.Fatalf("version = %q", snapshot.Version)
	}
	if len(snapshot.Revisions) != 4 {
		t.Fatalf("revisions = %d, want 4", len(snapshot.Revisions))
	}
	if len(snapshot.Orders) != 2 {
		t.Fatalf("orders = %d, want 2", len(snapshot.Orders))
	}
	if len(snapshot.Orders[1].Lines) != 3 {
		t.Fatalf("order 2 lines = %d, want 3", len(snapshot.Orders[1].Lines))
	}
	var deletedFound bool
	for _, rev := range snapshot.Revisions {
		if rev.PriceID == "SKU-DEL" {
			deletedFound = rev.Deleted
		}
	}
	if !deletedFound {
		t.Fatalf("deleted revision was not read as deleted")
	}

	warehouse := pricing.NewWarehouse()
	if _, err := warehouse.Ingest(snapshot); err != nil {
		t.Fatalf("ingest: %s", err)
	}
	result, err := warehouse.Now().Reprice("ORD-1")
	if err != nil {
		t.Fatalf("reprice ORD-1: %s", err)
	}
	if got, want := result.TotalCents, int64(2000); got != want {
		t.Fatalf("ORD-1 total = %d, want %d", got, want)
	}
	result, err = warehouse.Now().Reprice("ORD-2")
	if err != nil {
		t.Fatalf("reprice ORD-2: %s", err)
	}
	if result.Priced {
		t.Fatalf("ORD-2 should not be fully priced (SKU-DEL deleted, SKU-MISSING absent)")
	}
	if got, want := result.TotalCents, int64(3600); got != want {
		t.Fatalf("ORD-2 total = %d, want %d", got, want)
	}
	if len(result.Missing) != 2 {
		t.Fatalf("ORD-2 missing = %v, want two price IDs", result.Missing)
	}
}

func TestReaderRejectsIncompleteLedger(t *testing.T) {
	incomplete := `<!doctype html><div data-ledger><span data-version>v1</span></div>`
	reader := &pricing.Reader{Page: staticPage(incomplete)}
	if _, err := reader.Read(); err == nil {
		t.Fatalf("expected an error for a ledger without a completion marker")
	}
}

func TestReaderRejectsMissingVersion(t *testing.T) {
	missingVersion := `<!doctype html><div data-ledger data-complete>
	  <div data-order data-order-id="A" data-business-at="2026-01-01T00:00:00Z"></div>
	</div>`
	reader := &pricing.Reader{Page: staticPage(missingVersion)}
	if _, err := reader.Read(); err == nil {
		t.Fatalf("expected an error for a ledger without a version marker")
	}
}

func TestReaderRejectsAmbiguousLedgerRoots(t *testing.T) {
	// The fixture is served twice; the reader requires exactly one ledger.
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Write([]byte(ledgerPage + ledgerPage))
	}))
	defer server.Close()

	driver := agouti.PhantomJS()
	if err := driver.Start(); err != nil {
		t.Skipf("PhantomJS unavailable: %s", err)
	}
	defer driver.Stop()
	page, err := driver.NewPage()
	if err != nil {
		t.Fatalf("new page: %s", err)
	}
	defer page.Destroy()
	if err := page.Navigate(server.URL); err != nil {
		t.Fatalf("navigate: %s", err)
	}

	reader := &pricing.Reader{Page: &domPage{page}}
	if _, err := reader.Read(); err == nil {
		t.Fatalf("expected an error for more than one ledger root")
	}
}

func TestFilePageIgnoresScripts(t *testing.T) {
	// A page that fails to execute the read script must not yield a snapshot.
	page := &fixedPage{html: ledgerPage}
	if _, err := (&pricing.Reader{Page: page}).Read(); err == nil {
		t.Fatalf("expected an error when the read script does not run")
	}
}

var _ = ioutil.Discard

func TestReaderRefreshIsIdempotent(t *testing.T) {
	driver := agouti.PhantomJS()
	if err := driver.Start(); err != nil {
		t.Skipf("PhantomJS unavailable: %s", err)
	}
	defer driver.Stop()

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Write([]byte(ledgerPage))
	}))
	defer server.Close()

	page, err := driver.NewPage()
	if err != nil {
		t.Fatalf("new page: %s", err)
	}
	defer page.Destroy()
	if err := page.Navigate(server.URL); err != nil {
		t.Fatalf("navigate: %s", err)
	}

	warehouse := pricing.NewWarehouse()
	reader := &pricing.Reader{Page: &domPage{page}}
	first, err := warehouse.Refresh(reader)
	if err != nil {
		t.Fatalf("first refresh: %s", err)
	}
	second, err := warehouse.Refresh(reader)
	if err != nil {
		t.Fatalf("second refresh: %s", err)
	}
	if first.NewRevisions == 0 {
		t.Fatalf("first refresh stored no revisions")
	}
	if second.NewRevisions != 0 || second.NewOrders != 0 || second.NewLines != 0 {
		t.Fatalf("re-reading the same page double counted: %+v", second)
	}
	if got, want := len(warehouse.Now().Revisions()), 4; got != want {
		t.Fatalf("revisions = %d, want %d", got, want)
	}
}
