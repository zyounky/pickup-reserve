package stock

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fixture struct {
	store *Store
	now   time.Time
}

func setup(t *testing.T, path string) *fixture {
	t.Helper()
	f := &fixture{now: time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)}
	var err error
	f.store, err = New(path, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) receive(t *testing.T, sku string, quantity int) {
	t.Helper()
	if _, err := f.store.Receive("north", sku, quantity); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) stock(t *testing.T, sku string, total, held int) {
	t.Helper()
	got, err := f.store.Inventory("north", sku)
	want := Inventory{StoreID: "north", SKU: sku, OnHand: total, Reserved: held, Available: total - held}
	if err != nil || got != want {
		t.Fatalf("inventory = %+v, %v; want %+v", got, err, want)
	}
}

func TestConcurrentReservations(t *testing.T) {
	for _, sameKey := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared-key=%t", sameKey), func(t *testing.T) {
			f := setup(t, "")
			f.receive(t, "tea", 1)
			var wg sync.WaitGroup
			start := make(chan struct{})
			const buyers = 64
			type result struct {
				id     string
				replay bool
				err    error
			}
			results := make(chan result, buyers)
			for i := 0; i < buyers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					key := fmt.Sprintf("buyer-%d", i)
					if sameKey {
						key = "one-buyer"
					}
					<-start
					r, replay, err := f.store.Reserve(key, "north", []Item{{"tea", 1}}, time.Minute)
					results <- result{r.ID, replay, err}
				}(i)
			}
			close(start)
			wg.Wait()
			close(results)
			created := 0
			ids := map[string]bool{}
			for res := range results {
				if res.err != nil {
					if sameKey || !errors.Is(res.err, ErrInsufficient) {
						t.Fatalf("unexpected error: %v", res.err)
					}
					continue
				}
				ids[res.id] = true
				if !res.replay {
					created++
				} else if !sameKey {
					t.Fatal("separate buyers shared a reservation")
				}
			}
			if created != 1 || len(ids) != 1 {
				t.Fatalf("created %d distinct IDs %v", created, ids)
			}
			f.stock(t, "tea", 1, 1)
		})
	}
}

func TestBasketIsAtomicAndRequestOrderDoesNotMatter(t *testing.T) {
	f := setup(t, "")
	f.receive(t, "apples", 3)
	items := []Item{{"milk", 1}, {"apples", 2}}
	_, _, err := f.store.Reserve("order", "north", items, time.Minute)
	if !errors.Is(err, ErrInsufficient) {
		t.Fatal(err)
	}
	f.stock(t, "apples", 3, 0)
	f.receive(t, "milk", 1)
	r, replay, err := f.store.Reserve("order", "north", items, time.Minute)
	if err != nil || replay {
		t.Fatalf("reserve: %v, replay %t", err, replay)
	}
	items[0].Quantity = 99
	r.Items[0].Quantity = 99
	again, replay, err := f.store.Reserve("order", "north", []Item{{"apples", 2}, {"milk", 1}}, time.Minute)
	if err != nil || !replay || again.ID != r.ID {
		t.Fatalf("replay: %+v, %t, %v", again, replay, err)
	}
	_, _, err = f.store.Reserve("order", "north", again.Items, 2*time.Minute)
	if !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	f.stock(t, "apples", 3, 2)
	f.stock(t, "milk", 1, 1)
}

func TestTerminalTransitions(t *testing.T) {
	for _, terminal := range []string{cancelled, confirmed} {
		t.Run(terminal, func(t *testing.T) {
			f := setup(t, "")
			f.receive(t, "tea", 5)
			r, _, err := f.store.Reserve("order", "north", []Item{{"tea", 3}}, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			finish, other, total := f.store.Cancel, f.store.Confirm, 5
			if terminal == confirmed {
				finish, other, total = other, finish, 2
			}
			for i := 0; i < 2; i++ {
				if got, err := finish(r.ID); err != nil || got.Status != terminal {
					t.Fatalf("finish: %+v, %v", got, err)
				}
				f.now = f.now.Add(time.Hour)
			}
			if _, err := other(r.ID); !errors.Is(err, ErrConflict) {
				t.Fatal(err)
			}
			f.stock(t, "tea", total, 0)
		})
	}
}

func TestExpiryAndReplaySurviveRestartAndClockCorrection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	f := setup(t, path)
	f.receive(t, "tea", 1)
	items := []Item{{"tea", 1}}
	r, _, err := f.store.Reserve("first", "north", items, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	f.now = r.ExpiresAt.Add(-time.Nanosecond)
	f.stock(t, "tea", 1, 1)
	f.now = r.ExpiresAt
	f.stock(t, "tea", 1, 0)
	if _, err := f.store.Confirm(r.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("expired confirmation: %v", err)
	}
	if _, _, err := f.store.Reserve("second", "north", items, time.Minute); err != nil {
		t.Fatal(err)
	}
	f = setup(t, path) // simulated restart with the wall clock set back
	f.stock(t, "tea", 1, 1)
	got, replay, err := f.store.Reserve("first", "north", items, time.Minute)
	if err != nil || !replay || got.ID != r.ID || got.Status != expired {
		t.Fatalf("replay: %+v, %t, %v", got, replay, err)
	}
}

func TestPersistenceFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	f := setup(t, path)
	f.receive(t, "tea", 4)
	r, _, err := f.store.Reserve("first", "north", []Item{{"tea", 1}}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// A directory in place of the snapshot forces the final rename to fail.
	if err := os.Rename(path, path+".backup"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Confirm(r.ID); err == nil {
		t.Fatal("confirmation succeeded despite failed persistence")
	}
	if _, _, err := f.store.Reserve("retry", "north", []Item{{"tea", 1}}, time.Minute); err == nil {
		t.Fatal("reservation succeeded despite failed persistence")
	}
	f.stock(t, "tea", 4, 1)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, replay, err := f.store.Reserve("retry", "north", []Item{{"tea", 1}}, time.Minute); err != nil || replay {
		t.Fatalf("retry: replay %t, err %v", replay, err)
	}
}

func TestRejectOverflowAndDuplicateSKU(t *testing.T) {
	f := setup(t, "")
	f.receive(t, "tea", math.MaxInt)
	if _, err := f.store.Receive("north", "tea", 1); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, _, err := f.store.Reserve("order", "north", []Item{{"tea", 1}, {"tea", 1}}, time.Minute); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	f.stock(t, "tea", math.MaxInt, 0)
}

func TestHoldsAreIsolatedByStore(t *testing.T) {
	f := setup(t, "")
	f.receive(t, "tea", 1)
	if _, err := f.store.Receive("south", "tea", 1); err != nil {
		t.Fatal(err)
	}
	items := []Item{{"tea", 1}}
	r, _, err := f.store.Reserve("south-order", "south", items, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	f.stock(t, "tea", 1, 0)
	if _, _, err := f.store.Reserve("south-order", "north", items, time.Minute); !errors.Is(err, ErrConflict) {
		t.Fatalf("idempotency key reused at another store: %v", err)
	}
	if _, err := f.store.Confirm(r.ID); err != nil {
		t.Fatal(err)
	}
	f.stock(t, "tea", 1, 0)
}

func TestDamagedSnapshotIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	for _, data := range []string{
		"{",
		`{"version":2,"stock":{},"reservations":{}}`,
		`{"version":1,"reservations":{}}`,
		`{"version":1,"stock":{"north":{"tea":-1}},"reservations":{}}`,
	} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := New(path, time.Now); err == nil {
			t.Fatalf("accepted damaged snapshot %q", data)
		}
	}
}
