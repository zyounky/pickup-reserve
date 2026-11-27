package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zyounky/pickup-reserve/internal/httpapi"
	"github.com/zyounky/pickup-reserve/internal/stock"
)

const (
	inventoryURL = "/v1/stores/msk-1/inventory/milk"
	reserveURL   = "/v1/reservations"
	cart         = `{"store_id":"msk-1","items":[{"sku":"milk","quantity":2}]}`
)

func testHandler(t *testing.T) http.Handler {
	t.Helper()
	now := time.Date(2026, 1, 10, 13, 0, 0, 0, time.UTC)
	store, err := stock.New("", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	return httpapi.New(store)
}

func call(t *testing.T, h http.Handler, method, path, body, key string, status int) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != status {
		t.Fatalf("%s %s returned %d, want %d: %s", method, path, w.Code, status, w.Body)
	}
	return w
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode %q: %v", w.Body, err)
	}
	return value
}

func TestPickupLifecycle(t *testing.T) {
	h := testHandler(t)
	call(t, h, "POST", inventoryURL+"/receipts", `{"quantity":5}`, "", 200)
	created := call(t, h, "POST", reserveURL, cart, "basket-42", 201)
	reservation := decode[stock.Reservation](t, created)
	if reservation.ID == "" || reservation.Status != "active" || reservation.ExpiresAt.Sub(reservation.CreatedAt) != 5*time.Minute {
		t.Fatalf("unexpected reservation: %+v", reservation)
	}
	path := created.Header().Get("Location")
	if path != reserveURL+"/"+reservation.ID {
		t.Fatalf("unexpected Location: %q", path)
	}
	replay := call(t, h, "POST", reserveURL, cart, "basket-42", 200)
	if replay.Header().Get("Idempotency-Replayed") != "true" || decode[stock.Reservation](t, replay).ID != reservation.ID {
		t.Fatal("replay must identify the same reservation")
	}
	call(t, h, "POST", reserveURL, strings.Replace(cart, `"quantity":2`, `"quantity":3`, 1), "basket-42", 409)
	inventory := decode[stock.Inventory](t, call(t, h, "GET", inventoryURL, "", "", 200))
	if inventory.OnHand != 5 || inventory.Reserved != 2 || inventory.Available != 3 {
		t.Fatalf("replay changed inventory: %+v", inventory)
	}
	call(t, h, "POST", path+"/confirm", "", "", 200)
	got := decode[stock.Reservation](t, call(t, h, "GET", path, "", "", 200))
	if got.Status != "confirmed" {
		t.Fatalf("unexpected status %q", got.Status)
	}
	call(t, h, "POST", path+"/cancel", "", "", 409)
	second := call(t, h, "POST", reserveURL, cart, "basket-43", 201).Header().Get("Location")
	call(t, h, "POST", second+"/cancel", "", "", 200)
	inventory = decode[stock.Inventory](t, call(t, h, "GET", inventoryURL, "", "", 200))
	if inventory.OnHand != 3 || inventory.Available != 3 || inventory.Reserved != 0 {
		t.Fatalf("unexpected final inventory: %+v", inventory)
	}
}

func TestRequestErrors(t *testing.T) {
	h := testHandler(t)
	call(t, h, "POST", inventoryURL+"/receipts", `{"quantity":1}`, "", 200)
	for _, tc := range []struct {
		name, path, body, key string
		status                int
		code                  string
	}{
		{"missing key", reserveURL, cart, "", 400, "invalid_request"},
		{"unknown field", reserveURL, `{"store_idx":"abc"}`, "k", 400, "invalid_json"},
		{"malformed", reserveURL, "{", "k", 400, "invalid_json"},
		{"two values", reserveURL, cart + " {}", "k", 400, "invalid_json"},
		{"oversized", reserveURL, cart + strings.Repeat(" ", 65536), "k", 413, "body_too_large"},
		{"excessive ttl", reserveURL, `{"ttl_seconds":9223372036}`, "k", 400, "invalid_request"},
		{"zero quantity", inventoryURL + "/receipts", `{"quantity":0}`, "", 400, "invalid_request"},
		{"oversell", reserveURL, cart, "k", 409, "insufficient_stock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := call(t, h, "POST", tc.path, tc.body, tc.key, tc.status)
			body := decode[map[string]map[string]string](t, w)
			if body["error"]["code"] != tc.code || body["error"]["message"] == "" {
				t.Fatalf("unexpected error body: %v", body)
			}
		})
	}
	call(t, h, "GET", reserveURL+"/missing", "", "", 404)
}

func TestContentTypeRequired(t *testing.T) {
	h := testHandler(t)
	req := httptest.NewRequest("POST", inventoryURL+"/receipts", strings.NewReader(`{"quantity":1}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415, got %d", w.Code)
	}
}
