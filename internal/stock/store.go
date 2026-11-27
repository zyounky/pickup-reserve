// Package stock tracks physical stock and time-limited reservations at pickup stores.
package stock

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	ErrInvalid      = errors.New("invalid request")
	ErrNotFound     = errors.New("not found")
	ErrInsufficient = errors.New("insufficient stock")
	ErrConflict     = errors.New("conflict")
)

const (
	active    = "active"
	confirmed = "confirmed"
	cancelled = "cancelled"
	expired   = "expired"
)

type Item struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
}

type Inventory struct {
	StoreID   string `json:"store_id"`
	SKU       string `json:"sku"`
	OnHand    int    `json:"on_hand"`
	Reserved  int    `json:"reserved"`
	Available int    `json:"available"`
}

type Reservation struct {
	ID        string    `json:"id"`
	StoreID   string    `json:"store_id"`
	Status    string    `json:"status"`
	Items     []Item    `json:"items"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type record struct {
	Reservation
	Key string `json:"key"`
}

// Store owns one snapshot. A backing file belongs to a single running process.
type Store struct {
	mu   sync.Mutex
	data snapshot
	keys map[string]string // client key -> reservation ID
	path string
	now  func() time.Time
	last time.Time
}

// New opens a snapshot, or starts empty if the file does not yet exist.
// An empty path selects in-memory operation. A nil clock selects time.Now.
func New(path string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	data, err := load(path)
	if err != nil {
		return nil, err
	}
	start := now().UTC()
	if start.Before(data.AsOf) {
		start = data.AsOf
	}
	keys, err := data.validate(start)
	if err != nil {
		return nil, fmt.Errorf("invalid inventory snapshot: %w", err)
	}
	return &Store{data: data, keys: keys, path: path, now: now, last: start}, nil
}

// A clock correction must not resurrect a hold after its stock was reused.
// Call under mu. Mutations checkpoint this time alongside their stock changes.
func (s *Store) currentTime() time.Time {
	if now := s.now().UTC(); now.After(s.last) {
		s.last = now
	}
	return s.last
}

func validID(value string) bool {
	if len(value) == 0 || len(value) > 128 || value == "." || value == ".." {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._-", c)) {
			return false
		}
	}
	return true
}

func validKey(key string) bool {
	return len(key) > 0 && len(key) <= 128 && strings.TrimSpace(key) == key &&
		utf8.ValidString(key) && strings.IndexFunc(key, func(r rune) bool { return !unicode.IsPrint(r) }) == -1
}

func validTTL(ttl time.Duration) bool { return ttl >= time.Second && ttl <= 24*time.Hour }

func canonicalItems(items []Item) ([]Item, error) {
	if len(items) == 0 || len(items) > 100 {
		return nil, fmt.Errorf("%w: expected 1 to 100 items", ErrInvalid)
	}
	items = slices.Clone(items)
	slices.SortFunc(items, func(a, b Item) int { return strings.Compare(a.SKU, b.SKU) })
	for i, item := range items {
		if !validID(item.SKU) || item.Quantity <= 0 {
			return nil, fmt.Errorf("%w: invalid SKU or quantity", ErrInvalid)
		}
		if i > 0 && items[i-1].SKU == item.SKU {
			return nil, fmt.Errorf("%w: duplicate SKU %s", ErrInvalid, item.SKU)
		}
	}
	return items, nil
}

func (r Reservation) statusAt(now time.Time) string {
	if r.Status == active && !now.Before(r.ExpiresAt) {
		return expired
	}
	return r.Status
}

func (r Reservation) view(now time.Time) Reservation {
	r.Status = r.statusAt(now)
	r.Items = slices.Clone(r.Items)
	return r
}

func (s *Store) heldBySKU(storeID string, now time.Time) map[string]int {
	held := make(map[string]int)
	for _, r := range s.data.Reservations {
		if r.StoreID == storeID && r.statusAt(now) == active {
			for _, item := range r.Items {
				held[item.SKU] += item.Quantity
			}
		}
	}
	return held
}

func (s *Store) Inventory(storeID, sku string) (Inventory, error) {
	if !validID(storeID) || !validID(sku) {
		return Inventory{}, fmt.Errorf("%w: invalid store ID or SKU", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	total, ok := s.data.Stock[storeID][sku]
	if !ok {
		return Inventory{}, ErrNotFound
	}
	held := s.heldBySKU(storeID, s.currentTime())[sku]
	return Inventory{StoreID: storeID, SKU: sku, OnHand: total, Reserved: held, Available: total - held}, nil
}

func (s *Store) Receive(storeID, sku string, quantity int) (Inventory, error) {
	if !validID(storeID) || !validID(sku) || quantity <= 0 {
		return Inventory{}, fmt.Errorf("%w: invalid store ID, SKU or quantity", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.data.Stock[storeID][sku]
	if previous > math.MaxInt-quantity {
		return Inventory{}, fmt.Errorf("%w: stock exceeds integer limit", ErrInvalid)
	}
	total := previous + quantity
	now := s.currentTime()
	next := s.data.withStockCopy(storeID)
	next.Stock[storeID][sku] = total
	if err := s.commit(next); err != nil {
		return Inventory{}, err
	}
	held := s.heldBySKU(storeID, now)[sku]
	return Inventory{StoreID: storeID, SKU: sku, OnHand: total, Reserved: held, Available: total - held}, nil
}

// Reserve acquires every line in one transaction. A replay with the same
// store, lines and TTL returns the reservation's current state and replay=true.
func (s *Store) Reserve(key, storeID string, items []Item, ttl time.Duration) (Reservation, bool, error) {
	if !validKey(key) || !validID(storeID) || !validTTL(ttl) {
		return Reservation{}, false, fmt.Errorf("%w: invalid idempotency key, store ID or TTL", ErrInvalid)
	}
	items, err := canonicalItems(items)
	if err != nil {
		return Reservation{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.currentTime()
	if id, exists := s.keys[key]; exists {
		r := s.data.Reservations[id].Reservation
		if r.StoreID != storeID || !slices.Equal(items, r.Items) || ttl != r.ExpiresAt.Sub(r.CreatedAt) {
			return Reservation{}, false, fmt.Errorf("%w: idempotency key belongs to another request", ErrConflict)
		}
		return r.view(now), true, nil
	}
	held := s.heldBySKU(storeID, now)
	for _, item := range items {
		if s.data.Stock[storeID][item.SKU]-held[item.SKU] < item.Quantity {
			return Reservation{}, false, fmt.Errorf("%w: %s", ErrInsufficient, item.SKU)
		}
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Reservation{}, false, err
	}
	r := Reservation{ID: hex.EncodeToString(id[:]), StoreID: storeID, Status: active, Items: items, CreatedAt: now, ExpiresAt: now.Add(ttl)}
	next := s.data
	next.Reservations = maps.Clone(next.Reservations)
	next.Reservations[r.ID] = record{Reservation: r, Key: key}
	if err := s.commit(next); err != nil {
		return Reservation{}, false, err
	}
	s.keys[key] = r.ID
	return r.view(now), false, nil
}

func (s *Store) Get(id string) (Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.data.Reservations[id]
	if !ok {
		return Reservation{}, ErrNotFound
	}
	return r.view(s.currentTime()), nil
}

func (s *Store) Confirm(id string) (Reservation, error) { return s.finish(id, confirmed) }
func (s *Store) Cancel(id string) (Reservation, error)  { return s.finish(id, cancelled) }

func (s *Store) finish(id, status string) (Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.data.Reservations[id]
	if !ok {
		return Reservation{}, ErrNotFound
	}
	now := s.currentTime()
	current := r.statusAt(now)
	if current == status {
		return r.view(now), nil
	}
	if current != active {
		return Reservation{}, fmt.Errorf("%w: reservation is %s", ErrConflict, current)
	}
	next := s.data
	if status == confirmed {
		next = next.withStockCopy(r.StoreID)
		for _, item := range r.Items {
			next.Stock[r.StoreID][item.SKU] -= item.Quantity
		}
	}
	r.Status = status
	next.Reservations = maps.Clone(next.Reservations)
	next.Reservations[id] = r
	if err := s.commit(next); err != nil {
		return Reservation{}, err
	}
	return r.view(now), nil
}
