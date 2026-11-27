// Package httpapi exposes the pickup reservation service over HTTP.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"time"

	"github.com/zyounky/pickup-reserve/internal/stock"
)

const (
	maxJSONBytes      = 64 << 10
	defaultTTLSeconds = 300
)

type api struct {
	stock *stock.Store
}

// New returns a handler safe to serve concurrently.
func New(store *stock.Store) http.Handler {
	a := api{stock: store}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/stores/{storeID}/inventory/{sku}", a.inventory)
	mux.HandleFunc("POST /v1/stores/{storeID}/inventory/{sku}/receipts", a.receive)
	mux.HandleFunc("POST /v1/reservations", a.reserve)
	mux.HandleFunc("GET /v1/reservations/{id}", a.getReservation)
	mux.HandleFunc("POST /v1/reservations/{id}/confirm", a.confirm)
	mux.HandleFunc("POST /v1/reservations/{id}/cancel", a.cancel)
	return mux
}

func (a api) inventory(w http.ResponseWriter, r *http.Request) {
	item, err := a.stock.Inventory(r.PathValue("storeID"), r.PathValue("sku"))
	writeResult(w, item, err)
}

func (a api) receive(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Quantity int `json:"quantity"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	item, err := a.stock.Receive(r.PathValue("storeID"), r.PathValue("sku"), request.Quantity)
	writeResult(w, item, err)
}

func (a api) reserve(w http.ResponseWriter, r *http.Request) {
	var request struct {
		StoreID    string       `json:"store_id"`
		Items      []stock.Item `json:"items"`
		TTLSeconds int          `json:"ttl_seconds"`
	}
	request.TTLSeconds = defaultTTLSeconds
	if !readJSON(w, r, &request) {
		return
	}
	if request.TTLSeconds < 1 || request.TTLSeconds > 86400 {
		writeError(w, http.StatusBadRequest, "invalid_request", "ttl_seconds must be between 1 and 86400")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "Idempotency-Key header is required")
		return
	}
	reservation, replay, err := a.stock.Reserve(key, request.StoreID, request.Items, time.Duration(request.TTLSeconds)*time.Second)
	if err != nil {
		writeStockError(w, err)
		return
	}
	w.Header().Set("Location", "/v1/reservations/"+reservation.ID)
	status := http.StatusCreated
	if replay {
		status = http.StatusOK
		w.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(w, status, reservation)
}

func (a api) getReservation(w http.ResponseWriter, r *http.Request) {
	reservation, err := a.stock.Get(r.PathValue("id"))
	writeResult(w, reservation, err)
}

func (a api) confirm(w http.ResponseWriter, r *http.Request) {
	reservation, err := a.stock.Confirm(r.PathValue("id"))
	writeResult(w, reservation, err)
}

func (a api) cancel(w http.ResponseWriter, r *http.Request) {
	reservation, err := a.stock.Cancel(r.PathValue("id"))
	writeResult(w, reservation, err)
}

func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "set Content-Type to application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	err = decoder.Decode(dst)
	if err == nil {
		// Consume trailing whitespace and reject a second value.
		err = decoder.Decode(&struct{}{})
		if errors.Is(err, io.EOF) {
			return true
		}
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body exceeds 64 KiB")
	} else {
		writeError(w, http.StatusBadRequest, "invalid_json", "expected one JSON object with valid field names and types")
	}
	return false
}

func writeResult(w http.ResponseWriter, value any, err error) {
	if err != nil {
		writeStockError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func writeStockError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, stock.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, stock.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, stock.ErrInsufficient):
		writeError(w, http.StatusConflict, "insufficient_stock", err.Error())
	case errors.Is(err, stock.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	default:
		slog.Error("stock operation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "could not complete the operation")
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
