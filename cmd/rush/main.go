// Command rush runs a reproducible last-unit race against a live Pickup Reserve API.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	base := flag.String("url", "http://127.0.0.1:8080", "base URL of a running server")
	buyers := flag.Int("buyers", 100, "number of simultaneous buyers (1..1000)")
	units := flag.Int("units", 10, "units received by the demo store (1..1000)")
	flag.Parse()
	if err := run(strings.TrimRight(*base, "/"), *buyers, *units); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(base string, buyers, units int) error {
	if buyers < 1 || buyers > 1000 || units < 1 || units > 1000 {
		return fmt.Errorf("buyers and units must each be between 1 and 1000")
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	store := "rush-" + hex.EncodeToString(nonce[:])
	itemURL := base + "/v1/stores/" + store + "/inventory/milk"
	client := &http.Client{Timeout: 30 * time.Second}
	defer client.CloseIdleConnections()
	receipt := fmt.Sprintf(`{"quantity":%d}`, units)
	status, body, err := call(client, http.MethodPost, itemURL+"/receipts", receipt, "")
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("receipt: HTTP %d %s", status, body)
	}

	request := fmt.Sprintf(`{"store_id":%q,"items":[{"sku":"milk","quantity":1}],"ttl_seconds":3600}`, store)
	type result struct {
		created bool
		err     error
	}
	results := make(chan result, buyers)
	start := make(chan struct{})
	for i := range buyers {
		go func() {
			<-start
			key := fmt.Sprintf("%s-%d", store, i)
			code, body, err := call(client, http.MethodPost, base+"/v1/reservations", request, key)
			switch {
			case err != nil:
				results <- result{err: err}
			case code == http.StatusCreated:
				results <- result{created: true}
			case code == http.StatusConflict && isShortage(body):
				results <- result{}
			default:
				results <- result{err: fmt.Errorf("reservation: HTTP %d %s", code, body)}
			}
		}()
	}
	close(start)
	created, refused := 0, 0
	var firstErr error
	for range buyers {
		r := <-results
		switch {
		case r.err != nil:
			if firstErr == nil {
				firstErr = r.err
			}
		case r.created:
			created++
		default:
			refused++
		}
	}
	if firstErr != nil {
		return firstErr
	}
	code, body, err := call(client, http.MethodGet, itemURL, "", "")
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("inventory: HTTP %d %s", code, body)
	}
	var final struct {
		OnHand    int `json:"on_hand"`
		Reserved  int `json:"reserved"`
		Available int `json:"available"`
	}
	if err := json.Unmarshal(body, &final); err != nil {
		return err
	}
	fmt.Printf("buyers=%d units=%d created=%d insufficient_stock=%d available=%d\n", buyers, units, created, refused, final.Available)
	if created != min(buyers, units) || refused != buyers-created || final.OnHand != units || final.Reserved != created || final.Available != units-created {
		return fmt.Errorf("inventory mismatch in store %s: %s", store, body)
	}
	return nil
}

func isShortage(body []byte) bool {
	var response struct {
		Error struct{ Code string }
	}
	return json.Unmarshal(body, &response) == nil && response.Error.Code == "insufficient_stock"
}

func call(client *http.Client, method, url, payload, key string) (int, []byte, error) {
	req, err := http.NewRequest(method, url, bytes.NewBufferString(payload))
	if err != nil {
		return 0, nil, err
	}
	if payload != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, body, err
}
