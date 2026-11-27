package stock

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const snapshotVersion = 1

type snapshot struct {
	Version      int                       `json:"version"`
	AsOf         time.Time                 `json:"as_of"`
	Stock        map[string]map[string]int `json:"stock"`
	Reservations map[string]record         `json:"reservations"`
}

func load(path string) (snapshot, error) {
	empty := snapshot{Version: snapshotVersion, Stock: make(map[string]map[string]int), Reservations: make(map[string]record)}
	if path == "" {
		return empty, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
			return snapshot{}, fmt.Errorf("create snapshot directory: %w", err)
		}
		return empty, nil
	}
	if err != nil {
		return snapshot{}, fmt.Errorf("open snapshot: %w", err)
	}
	var data snapshot
	if err := json.Unmarshal(b, &data); err != nil {
		return snapshot{}, fmt.Errorf("decode snapshot: %w", err)
	}
	return data, nil
}

func (d snapshot) withStockCopy(storeID string) snapshot {
	d.Stock = maps.Clone(d.Stock)
	items := maps.Clone(d.Stock[storeID])
	if items == nil {
		items = make(map[string]int)
	}
	d.Stock[storeID] = items
	return d
}

// Validate before serving any traffic, so malformed or incompatible snapshots
// cannot silently turn into an empty catalog at startup.
func (d snapshot) validate(now time.Time) (map[string]string, error) {
	if d.Version != snapshotVersion || d.Stock == nil || d.Reservations == nil {
		return nil, errors.New("unsupported version or missing fields")
	}
	for storeID, items := range d.Stock {
		if !validID(storeID) || items == nil {
			return nil, errors.New("invalid store")
		}
		for sku, total := range items {
			if !validID(sku) || total < 0 {
				return nil, errors.New("invalid stock")
			}
		}
	}
	keys := make(map[string]string, len(d.Reservations))
	type location struct{ storeID, sku string }
	held := make(map[location]int)
	for id, r := range d.Reservations {
		if r.ID != id || !validID(id) || !validKey(r.Key) || !validID(r.StoreID) ||
			r.CreatedAt.IsZero() || !validTTL(r.ExpiresAt.Sub(r.CreatedAt)) {
			return nil, errors.New("invalid reservation")
		}
		if _, exists := keys[r.Key]; exists {
			return nil, errors.New("duplicate reservation key")
		}
		keys[r.Key] = id
		if r.Status != active && r.Status != cancelled && r.Status != confirmed {
			return nil, fmt.Errorf("unknown reservation status %q", r.Status)
		}
		canonical, err := canonicalItems(r.Items)
		if err != nil || !slices.Equal(r.Items, canonical) {
			return nil, errors.New("invalid reservation items")
		}
		for _, item := range r.Items {
			total, ok := d.Stock[r.StoreID][item.SKU]
			if !ok {
				return nil, errors.New("unknown SKU in reservation")
			}
			if r.statusAt(now) == active {
				loc := location{r.StoreID, item.SKU}
				if total-held[loc] < item.Quantity {
					return nil, errors.New("active reservations exceed stock")
				}
				held[loc] += item.Quantity
			}
		}
	}
	return keys, nil
}

// The caller owns the mutex. Shared maps stay read-only while the candidate
// snapshot is encoded, so a failed write leaves both memory and disk usable.
func (s *Store) commit(next snapshot) error {
	next.AsOf = s.last
	if s.path != "" {
		if err := save(s.path, next); err != nil {
			return fmt.Errorf("persist inventory snapshot: %w", err)
		}
	}
	s.data = next
	return nil
}

func save(path string, next snapshot) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".pickup-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := json.NewEncoder(file).Encode(next); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
