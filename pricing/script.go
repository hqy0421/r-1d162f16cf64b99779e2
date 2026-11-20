package pricing

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

func trimSpace(value string) string {
	return strings.TrimSpace(value)
}

func parseInt(text string) (int64, error) {
	return strconv.ParseInt(text, 10, 64)
}

var timeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// parseTime parses a page-rendered timestamp. Timestamps without an explicit
// zone are treated as UTC so that stored history is independent of the host
// locale.
func parseTime(text string) (time.Time, error) {
	trimmed := trimSpace(text)
	if trimmed == "" {
		return time.Time{}, errors.New("empty timestamp")
	}
	for _, layout := range timeLayouts {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, errors.New("unrecognised timestamp " + strconv.Quote(trimmed))
}

// readScript returns JavaScript that walks the real DOM under the ledger root
// and returns one JavaScript object describing the completed version. Running
// in the page guarantees the data comes from the live DOM rather than a
// cached copy.
func readScript(selectors Selectors) string {
	config := jsObject(jsSelectorsFrom(selectors))
	return `return (function(sel) {
  function attr(node, name) {
    return name ? (node.getAttribute(name) || "") : "";
  }
  function text(root, query) {
    if (!query) { return ""; }
    var node = root.querySelector(query);
    return node ? node.textContent.trim() : "";
  }
  // nodeHas reports whether the node itself (not a descendant) matches the
  // selector. PhantomJS lacks Element.matches, so test via a scoped query.
  function nodeHas(node, query) {
    var parent = node.parentNode;
    if (!parent) { return false; }
    var matches = parent.querySelectorAll(query);
    for (var m = 0; m < matches.length; m++) {
      if (matches[m] === node) { return true; }
    }
    return false;
  }
  var roots = document.querySelectorAll(sel.ledger);
  if (roots.length !== 1) {
    return {found: false, complete: false, version: "", revisions: [], orders: []};
  }
  var root = roots[0];
  var complete = true;
  if (sel.complete) {
    complete = root.querySelector(sel.complete) !== null || nodeHas(root, sel.complete);
  }
  var result = {
    found: true,
    complete: complete,
    version: text(root, sel.version),
    revisions: [],
    orders: []
  };
  var attrs = sel.attrs;
  var revisions = sel.revisions ? root.querySelectorAll(sel.revisions) : [];
  for (var i = 0; i < revisions.length; i++) {
    var node = revisions[i];
    result.revisions.push({
      priceId: attr(node, attrs.priceId),
      revision: attr(node, attrs.revision),
      amount: attr(node, attrs.amount),
      validFrom: attr(node, attrs.validFrom),
      validTo: attr(node, attrs.validTo),
      knownAt: attr(node, attrs.knownAt),
      deleted: node.hasAttribute(attrs.deleted) && attr(node, attrs.deleted) !== "false",
      deletedAt: attr(node, attrs.deletedAt)
    });
  }
  var orders = sel.orders ? root.querySelectorAll(sel.orders) : [];
  for (var j = 0; j < orders.length; j++) {
    var order = orders[j];
    var orderResult = {
      orderId: attr(order, attrs.orderId),
      businessAt: attr(order, attrs.businessAt),
      lines: []
    };
    var lines = sel.lines ? order.querySelectorAll(sel.lines) : [];
    for (var k = 0; k < lines.length; k++) {
      orderResult.lines.push({
        priceId: attr(lines[k], attrs.priceId),
        qty: attr(lines[k], attrs.qty)
      });
    }
    result.orders.push(orderResult);
  }
  return result;
})(` + config + `);`
}

type jsSelectors struct {
	ledger    string
	version   string
	complete  string
	revisions string
	orders    string
	lines     string
	attrs     Attributes
}

func jsSelectorsFrom(selectors Selectors) jsSelectors {
	return jsSelectors{
		ledger:    selectors.Ledger,
		version:   selectors.Version,
		complete:  selectors.Complete,
		revisions: selectors.Revisions,
		orders:    selectors.Orders,
		lines:     selectors.Lines,
		attrs:     selectors.Attributes,
	}
}

func jsObject(selectors jsSelectors) string {
	return "{" +
		"ledger:" + jsString(selectors.ledger) + "," +
		"version:" + jsString(selectors.version) + "," +
		"complete:" + jsString(selectors.complete) + "," +
		"revisions:" + jsString(selectors.revisions) + "," +
		"orders:" + jsString(selectors.orders) + "," +
		"lines:" + jsString(selectors.lines) + "," +
		"attrs:" + jsAttributes(selectors.attrs) +
		"}"
}

func jsAttributes(attrs Attributes) string {
	return "{" +
		"priceId:" + jsString(attrs.PriceID) + "," +
		"revision:" + jsString(attrs.Revision) + "," +
		"amount:" + jsString(attrs.Amount) + "," +
		"validFrom:" + jsString(attrs.ValidFrom) + "," +
		"validTo:" + jsString(attrs.ValidTo) + "," +
		"knownAt:" + jsString(attrs.KnownAt) + "," +
		"deleted:" + jsString(attrs.Deleted) + "," +
		"deletedAt:" + jsString(attrs.DeletedAt) + "," +
		"orderId:" + jsString(attrs.OrderID) + "," +
		"businessAt:" + jsString(attrs.BusinessAt) + "," +
		"qty:" + jsString(attrs.Qty) +
		"}"
}

func jsString(value string) string {
	// Selectors and attribute names are developer-supplied and contain no
	// quotes in practice, but escape defensively so a malformed value cannot
	// break the script.
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`)
	return `"` + replacer.Replace(value) + `"`
}
