package pricing

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"
)

const warehouseSchema = 1

// fileFormat is the on-disk representation of a warehouse. Times are stored
// as RFC 3339 strings with nanosecond precision so that reopening restores
// exactly the instants that were ingested.
type fileFormat struct {
	Schema    int              `json:"schema"`
	Revisions []revisionOnDisk `json:"revisions"`
	Orders    []orderOnDisk    `json:"orders"`
}

type revisionOnDisk struct {
	PriceID     string `json:"priceId"`
	Revision    int    `json:"revision"`
	AmountCents int64  `json:"amountCents"`
	ValidFrom   string `json:"validFrom"`
	ValidTo     string `json:"validTo"`
	KnownAt     string `json:"knownAt"`
	Deleted     bool   `json:"deleted"`
	DeletedAt   string `json:"deletedAt,omitempty"`
}

type orderOnDisk struct {
	OrderID    string       `json:"orderId"`
	BusinessAt string       `json:"businessAt"`
	Lines      []lineOnDisk `json:"lines"`
	RecordedAt string       `json:"recordedAt"`
}

type lineOnDisk struct {
	PriceID string `json:"priceId"`
	Qty     int64  `json:"qty"`
}

// Open loads a warehouse from path. If the file does not exist, Open returns
// an empty warehouse bound to path so that later refreshes persist there.
func Open(path string) (*Warehouse, error) {
	data, err := ioutil.ReadFile(path)
	if os.IsNotExist(err) {
		return &Warehouse{path: path}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pricing: reading warehouse %s: %s", path, err)
	}

	var format fileFormat
	if err := json.Unmarshal(data, &format); err != nil {
		return nil, fmt.Errorf("pricing: decoding warehouse %s: %s", path, err)
	}
	if format.Schema != warehouseSchema {
		return nil, fmt.Errorf("pricing: warehouse %s has unsupported schema %d", path, format.Schema)
	}

	warehouse := &Warehouse{path: path}
	for _, revision := range format.Revisions {
		knownAt, err := requiredTime(revision.KnownAt, "known-at")
		if err != nil {
			return nil, fmt.Errorf("pricing: warehouse %s: %s", path, err)
		}
		validFrom, err := requiredTime(revision.ValidFrom, "valid-from")
		if err != nil {
			return nil, fmt.Errorf("pricing: warehouse %s: %s", path, err)
		}
		validTo, err := requiredTime(revision.ValidTo, "valid-to")
		if err != nil {
			return nil, fmt.Errorf("pricing: warehouse %s: %s", path, err)
		}
		record := &revisionRecord{
			revision: PriceRevision{
				PriceID:     revision.PriceID,
				Revision:    revision.Revision,
				AmountCents: revision.AmountCents,
				ValidFrom:   validFrom,
				ValidTo:     validTo,
				KnownAt:     knownAt,
			},
			knownAt: knownAt,
			deleted: revision.Deleted,
		}
		if revision.Deleted {
			deletedAt, err := requiredTime(revision.DeletedAt, "deleted-at")
			if err != nil {
				return nil, fmt.Errorf("pricing: warehouse %s: %s", path, err)
			}
			record.deletedAt = deletedAt
		}
		warehouse.revisions = append(warehouse.revisions, record)
	}
	for _, order := range format.Orders {
		businessAt, err := requiredTime(order.BusinessAt, "business-at")
		if err != nil {
			return nil, fmt.Errorf("pricing: warehouse %s: %s", path, err)
		}
		recordedAt, err := requiredTime(order.RecordedAt, "recorded-at")
		if err != nil {
			return nil, fmt.Errorf("pricing: warehouse %s: %s", path, err)
		}
		stored := Order{OrderID: order.OrderID, BusinessAt: businessAt}
		for _, line := range order.Lines {
			stored.Lines = append(stored.Lines, OrderLine{PriceID: line.PriceID, Qty: line.Qty})
		}
		warehouse.orders = append(warehouse.orders, &orderRecord{order: stored, recordedAt: recordedAt})
	}

	if err := warehouse.Validate(); err != nil {
		return nil, fmt.Errorf("pricing: warehouse %s is inconsistent: %s", path, err)
	}
	return warehouse, nil
}

// Save persists the warehouse to path and binds the warehouse to that path so
// that later refreshes persist automatically. The write is atomic: a failure
// leaves any previous file untouched.
func (w *Warehouse) Save(path string) error {
	if err := writeWarehouseFile(path, w.revisions, w.orders); err != nil {
		return err
	}
	w.path = path
	return nil
}

// writeWarehouseFile writes the records to path atomically.
func writeWarehouseFile(path string, revisions []*revisionRecord, orders []*orderRecord) error {
	format := fileFormat{Schema: warehouseSchema}
	for _, record := range revisions {
		onDisk := revisionOnDisk{
			PriceID:     record.revision.PriceID,
			Revision:    record.revision.Revision,
			AmountCents: record.revision.AmountCents,
			ValidFrom:   record.revision.ValidFrom.UTC().Format(time.RFC3339Nano),
			ValidTo:     record.revision.ValidTo.UTC().Format(time.RFC3339Nano),
			KnownAt:     record.knownAt.UTC().Format(time.RFC3339Nano),
			Deleted:     record.deleted,
		}
		if record.deleted {
			onDisk.DeletedAt = record.deletedAt.UTC().Format(time.RFC3339Nano)
		}
		format.Revisions = append(format.Revisions, onDisk)
	}
	for _, record := range orders {
		onDisk := orderOnDisk{
			OrderID:    record.order.OrderID,
			BusinessAt: record.order.BusinessAt.UTC().Format(time.RFC3339Nano),
			RecordedAt: record.recordedAt.UTC().Format(time.RFC3339Nano),
		}
		for _, line := range record.order.Lines {
			onDisk.Lines = append(onDisk.Lines, lineOnDisk{PriceID: line.PriceID, Qty: line.Qty})
		}
		format.Orders = append(format.Orders, onDisk)
	}

	data, err := json.MarshalIndent(format, "", "  ")
	if err != nil {
		return fmt.Errorf("pricing: encoding warehouse: %s", err)
	}
	data = append(data, '\n')

	directory := filepath.Dir(path)
	temp, err := ioutil.TempFile(directory, ".pricing-warehouse-")
	if err != nil {
		return fmt.Errorf("pricing: creating temporary warehouse file: %s", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)

	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("pricing: writing warehouse: %s", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("pricing: syncing warehouse: %s", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("pricing: closing warehouse: %s", err)
	}
	if err := os.Chmod(tempName, 0600); err != nil {
		return fmt.Errorf("pricing: securing warehouse: %s", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("pricing: replacing warehouse %s: %s", path, err)
	}
	return nil
}

func requiredTime(value, field string) (time.Time, error) {
	if value == "" {
		return time.Time{}, fmt.Errorf("missing %s", field)
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid %s %q: %s", field, value, err)
	}
	return parsed.UTC(), nil
}
