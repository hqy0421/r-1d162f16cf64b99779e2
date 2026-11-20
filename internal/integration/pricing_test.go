package integration_test

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/sclevine/agouti"
	"github.com/sclevine/agouti/pricing"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"
)

// The ledger page is the supplier's completed price view. It is only served
// to a request that carries the session cookie, so the read happens against
// the logged-in page rather than an anonymous copy.
const pricingLedgerHTML = `<!doctype html><html><body>
<div data-ledger data-complete>
  <span data-version>2026-11-20T10:00:00Z</span>
  <ul>
    <li data-price-revision data-price-id="SKU-A" data-revision="1" data-amount="1050"
        data-valid-from="2026-01-01T00:00:00Z" data-valid-to="2026-02-01T00:00:00Z"
        data-known-at="2026-01-01T00:00:00Z"></li>
    <li data-price-revision data-price-id="SKU-A" data-revision="2" data-amount="1250"
        data-valid-from="2026-02-01T00:00:00Z" data-valid-to="2026-04-01T00:00:00Z"
        data-known-at="2026-02-01T00:00:00Z"></li>
    <li data-price-revision data-price-id="SKU-B" data-revision="1" data-amount="0"
        data-valid-from="2026-01-01T00:00:00Z" data-valid-to="2026-04-01T00:00:00Z"
        data-known-at="2026-01-01T00:00:00Z"></li>
  </ul>
  <div data-order data-order-id="O-1" data-business-at="2026-01-15T00:00:00Z">
    <span data-line data-price-id="SKU-A" data-qty="2"></span>
    <span data-line data-price-id="SKU-B" data-qty="1"></span>
  </div>
  <div data-order data-order-id="O-2" data-business-at="2026-02-15T00:00:00Z">
    <span data-line data-price-id="SKU-A" data-qty="3"></span>
    <span data-line data-price-id="SKU-NONE" data-qty="1"></span>
  </div>
</div>
</body></html>`

func testPricingReadAndReprice(browserName string, newPage pageFunc) {
	Describe("pricing ledger for "+browserName, func() {
		var (
			page   *agouti.Page
			server *httptest.Server
		)

		BeforeEach(func() {
			server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/ledger" {
					if cookie, err := request.Cookie("pricing-session"); err != nil || cookie.Value != "authenticated" {
						http.Error(response, "not logged in", http.StatusUnauthorized)
						return
					}
					response.Write([]byte(pricingLedgerHTML))
					return
				}
				response.Write([]byte("<html><body>login</body></html>"))
			}))

			var err error
			page, err = newPage()
			Expect(err).NotTo(HaveOccurred())
			Expect(page.Navigate(server.URL)).To(Succeed())
			var cookie string
			Expect(page.RunScript("document.cookie = 'pricing-session=authenticated; path=/'; return document.cookie;", nil, &cookie)).To(Succeed())
			Expect(page.Navigate(server.URL + "/ledger")).To(Succeed())
		})

		AfterEach(func() {
			Expect(page.Destroy()).To(Succeed())
			server.Close()
		})

		It("reads the completed version from the logged-in page and reprices", func() {
			warehouse := pricing.NewWarehouse()
			reader := &pricing.Reader{Page: page}
			if _, err := warehouse.Refresh(reader); err != nil {
				Fail("refresh: " + err.Error())
			}

			order, err := warehouse.Now().Reprice("O-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(order.Lines[0].UnitCents).To(Equal(int64(1050)))
			Expect(order.TotalCents).To(Equal(int64(2100)))
			Expect(order.Priced).To(BeTrue())

			order, err = warehouse.Now().Reprice("O-2")
			Expect(err).NotTo(HaveOccurred())
			Expect(order.Priced).To(BeFalse())
			Expect(order.Missing).To(ConsistOf("SKU-NONE"))
			Expect(order.TotalCents).To(Equal(int64(3750)))
		})

		It("persists history and orders across a reopen", func() {
			directory, err := ioutil.TempDir("", "pricing-integration-")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(directory)
			path := filepath.Join(directory, "history.json")

			warehouse, err := pricing.Open(path)
			Expect(err).NotTo(HaveOccurred())
			_, err = warehouse.Refresh(&pricing.Reader{Page: page})
			Expect(err).NotTo(HaveOccurred())

			reopened, err := pricing.Open(path)
			Expect(err).NotTo(HaveOccurred())
			order, err := reopened.KnownAt(time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC)).Reprice("O-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(order.TotalCents).To(Equal(int64(2100)))
		})

		It("keeps old results when the page stops being readable", func() {
			warehouse := pricing.NewWarehouse()
			_, err := warehouse.Refresh(&pricing.Reader{Page: page})
			Expect(err).NotTo(HaveOccurred())

			// Log out; the server now answers the ledger request with 401 and
			// no ledger, so the DOM read cannot produce a complete version.
			Expect(page.RunScript("document.cookie = 'pricing-session=; expires=Thu, 01 Jan 1970 00:00:00 GMT; path=/';", nil, nil)).To(Succeed())
			Expect(page.Navigate(server.URL + "/ledger")).To(Succeed())

			_, err = warehouse.Refresh(&pricing.Reader{Page: page})
			Expect(err).To(HaveOccurred())

			order, err := warehouse.Now().Reprice("O-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(order.TotalCents).To(Equal(int64(2100)))
		})
	})
}
