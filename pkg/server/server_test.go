package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/46labs/flowroute/pkg/config"
)

const (
	owned   = "+12065550100"
	handset = "+14035550199"
)

// receiver records callbacks and answers each with the next queued status,
// or 200 when none is queued.
type receiver struct {
	srv *httptest.Server

	mu       sync.Mutex
	statuses []int
	bodies   chan []byte
	ctypes   chan string
}

func newReceiver(t *testing.T, statuses ...int) *receiver {
	t.Helper()
	rc := &receiver{statuses: statuses, bodies: make(chan []byte, 16), ctypes: make(chan string, 16)}
	rc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read callback: %v", err)
		}
		rc.ctypes <- r.Header.Get("Content-Type")
		rc.bodies <- body
		rc.mu.Lock()
		status := http.StatusOK
		if len(rc.statuses) > 0 {
			status, rc.statuses = rc.statuses[0], rc.statuses[1:]
		}
		rc.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(rc.srv.Close)
	return rc
}

func (rc *receiver) next(t *testing.T) map[string]any {
	t.Helper()
	select {
	case body := <-rc.bodies:
		var v map[string]any
		if err := json.Unmarshal(body, &v); err != nil {
			t.Fatalf("decode callback %s: %v", body, err)
		}
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("no callback received")
		return nil
	}
}

func newServer(t *testing.T, mutate func(*config.Config)) (*Server, *httptest.Server) {
	t.Helper()
	cfg := &config.Config{
		BaseURL:                "https://api.flowroute.com",
		AccessKey:              "ak",
		SecretKey:              "sk",
		Numbers:                []config.Number{{Number: owned}},
		DefaultOutcome:         "delivered",
		Outcomes:               map[string]string{},
		Attempts:               4,
		BaseRateNanodollars:    3500000,
		SurchargeNanodollars:   6200000,
		InboundRateNanodollars: 4000000,
	}
	if mutate != nil {
		mutate(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := New(ctx, cfg)
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		cancel()
		s.Wait()
		hs.Close()
	})
	return s, hs
}

func sendBody(to, from, body, dlr string) string {
	b, err := json.Marshal(map[string]any{"data": map[string]any{
		"type":       "message",
		"attributes": map[string]any{"to": to, "from": from, "body": body, "dlr_callback": dlr},
	}})
	if err != nil {
		panic(err)
	}
	return string(b)
}

func do(t *testing.T, method, url, user, pass, ctype, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return resp, b
}

func TestSendValidation(t *testing.T) {
	_, hs := newServer(t, nil)
	tests := []struct {
		name   string
		path   string
		user   string
		ctype  string
		body   string
		status int
	}{
		{"accepted v2.2", "/v2.2/messages", "ak", contentType, sendBody(handset, owned, "hi", ""), http.StatusAccepted},
		{"accepted v2.1", "/v2.1/messages", "ak", contentType, sendBody(handset, owned, "hi", ""), http.StatusAccepted},
		{"unknown version", "/v3/messages", "ak", contentType, sendBody(handset, owned, "hi", ""), http.StatusNotFound},
		{"no auth", "/v2.2/messages", "", contentType, sendBody(handset, owned, "hi", ""), http.StatusUnauthorized},
		{"wrong key", "/v2.2/messages", "bad", contentType, sendBody(handset, owned, "hi", ""), http.StatusUnauthorized},
		{"json content type", "/v2.2/messages", "ak", "application/json", sendBody(handset, owned, "hi", ""), http.StatusUnsupportedMediaType},
		{"from not owned", "/v2.2/messages", "ak", contentType, sendBody(handset, "+12065550101", "hi", ""), http.StatusForbidden},
		{"bad number", "/v2.2/messages", "ak", contentType, sendBody("555", owned, "hi", ""), http.StatusBadRequest},
		{"empty body", "/v2.2/messages", "ak", contentType, sendBody(handset, owned, "", ""), http.StatusBadRequest},
		{"compact form", "/v2.1/messages", "ak", contentType, `{"to":"` + handset + `","from":"` + owned + `","body":"hi"}`, http.StatusAccepted},
		{"wrong data type", "/v2.2/messages", "ak", contentType, `{"data":{"type":"x","attributes":{"to":"` + handset + `","from":"` + owned + `","body":"hi"}}}`, http.StatusBadRequest},
		{"trailing data", "/v2.2/messages", "ak", contentType, sendBody(handset, owned, "hi", "") + "{}", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := do(t, http.MethodPost, hs.URL+tt.path, tt.user, "sk", tt.ctype, tt.body)
			if resp.StatusCode != tt.status {
				t.Fatalf("status %d, want %d: %s", resp.StatusCode, tt.status, body)
			}
		})
	}
}

func TestSendPriceDetails(t *testing.T) {
	_, hs := newServer(t, nil)
	resp, body := do(t, http.MethodPost, hs.URL+"/v2.2/messages", "ak", "sk", contentType, sendBody(handset, owned, "hi", ""))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	for _, want := range []string{
		`"base_rate":0.0035`, `"charged_cost":0.0097`, `"segment_count":1`, `"surcharge":0.0062`,
		`"links":{"self":"https://api.flowroute.com/v2.2/messages/mdr2-`,
	} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("response %s missing %s", body, want)
		}
	}

	_, body = do(t, http.MethodPost, hs.URL+"/v2.1/messages", "ak", "sk", contentType, sendBody(handset, owned, "hi", ""))
	if bytes.Contains(body, []byte("price_details")) {
		t.Errorf("v2.1 response has price_details: %s", body)
	}
}

func TestReceipts(t *testing.T) {
	tests := []struct {
		outcome  string
		statuses []string
	}{
		{"delivered", []string{"smsc submit", "delivered"}},
		{"buffered", []string{"smsc submit", "message buffered"}},
		{"undelivered", []string{"smsc submit", "undelivered"}},
		{"none", []string{"smsc submit"}},
	}
	for _, tt := range tests {
		t.Run(tt.outcome, func(t *testing.T) {
			rc := newReceiver(t)
			s, hs := newServer(t, func(c *config.Config) { c.Outcomes[handset] = tt.outcome })
			resp, body := do(t, http.MethodPost, hs.URL+"/v2.2/messages", "ak", "sk", contentType, sendBody(handset, owned, "hi", rc.srv.URL))
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			var sent sendResponse
			if err := json.Unmarshal(body, &sent); err != nil {
				t.Fatal(err)
			}
			for i, want := range tt.statuses {
				if ct := <-rc.ctypes; ct != contentType {
					t.Errorf("callback content type %q", ct)
				}
				data := rc.next(t)["data"].(map[string]any)
				attrs := data["attributes"].(map[string]any)
				if data["id"] != sent.Data.ID || data["type"] != "delivery_receipt" || attrs["body"] != "hi" {
					t.Errorf("receipt %d: id %v type %v", i, data["id"], data["type"])
				}
				if attrs["status"] != want || attrs["level"] != float64(i+1) {
					t.Errorf("receipt %d: status %v level %v, want %s level %d", i, attrs["status"], attrs["level"], want, i+1)
				}
			}
			s.Wait()

			_, body = do(t, http.MethodGet, hs.URL+"/v2.2/messages/"+sent.Data.ID, "ak", "sk", "", "")
			var got lookupResponse
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			a := got.Data.Attributes
			if a.Status != tt.statuses[len(tt.statuses)-1] || len(a.DeliveryReceipts) != len(tt.statuses) {
				t.Errorf("lookup status %q with %d receipts: %s", a.Status, len(a.DeliveryReceipts), body)
			}
			if a.DeliveryReceipts[0].Level != len(tt.statuses) {
				t.Errorf("lookup receipts not newest first: %s", body)
			}
			if a.PriceDetails == nil || a.PriceDetails.ChargedCost != "0.009700" || a.AmountDisplay != "$0.0097" {
				t.Errorf("lookup price: %s", body)
			}
		})
	}
}

func TestCallbackRetries(t *testing.T) {
	tests := []struct {
		name     string
		statuses []int
		attempts int
	}{
		{"first try", nil, 1},
		{"after two failures", []int{500, 404}, 3},
		{"gives up after four", []int{500, 500, 500, 500, 500}, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc := newReceiver(t, tt.statuses...)
			s, hs := newServer(t, func(c *config.Config) {
				c.Numbers = []config.Number{{Number: owned, Callback: rc.srv.URL}}
				c.RetryInterval = time.Millisecond
			})
			resp, body := do(t, http.MethodPost, hs.URL+"/mock/inbound", "", "", "application/json",
				`{"from":"`+handset+`","to":"`+owned+`","body":"STOP"}`)
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			var created map[string]string
			if err := json.Unmarshal(body, &created); err != nil {
				t.Fatal(err)
			}
			s.Wait()
			m, ok := s.get(created["id"])
			if !ok {
				t.Fatal("message not recorded")
			}
			if len(m.Attempts) != tt.attempts {
				t.Errorf("%d attempts, want %d", len(m.Attempts), tt.attempts)
			}
		})
	}
}

func TestInboundPayload(t *testing.T) {
	rc := newReceiver(t)
	_, hs := newServer(t, func(c *config.Config) { c.Callback = rc.srv.URL })
	resp, body := do(t, http.MethodPost, hs.URL+"/mock/inbound", "", "", "application/json",
		`{"from":"`+handset+`","to":"`+owned+`","body":"pic","media":[{"url":"https://media.example/a/cat.jpg?Expires=1&Signature=x","mime_type":"image/jpeg","file_size":2048}]}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	got := rc.next(t)
	data := got["data"].(map[string]any)
	attrs := data["attributes"].(map[string]any)
	checks := map[string]any{
		"direction":          "inbound",
		"from":               handset,
		"to":                 owned,
		"is_mms":             true,
		"amount_nanodollars": float64(4000000),
		"amount_display":     "$0.0040",
		"status":             "",
		"message_type":       "longcode",
	}
	for k, want := range checks {
		if attrs[k] != want {
			t.Errorf("%s = %v, want %v", k, attrs[k], want)
		}
	}
	if r, ok := attrs["delivery_receipts"].([]any); !ok || len(r) != 0 {
		t.Errorf("delivery_receipts = %v, want []", attrs["delivery_receipts"])
	}
	included := got["included"].([]any)
	media := included[0].(map[string]any)["attributes"].(map[string]any)
	if media["file_name"] != "cat.jpg" || media["mime_type"] != "image/jpeg" || media["file_size"] != float64(2048) {
		t.Errorf("media = %v", media)
	}
}

func TestInboundRejects(t *testing.T) {
	_, hs := newServer(t, nil)
	tests := []struct {
		name   string
		body   string
		status int
	}{
		{"not on account", `{"from":"` + handset + `","to":"+12065550101","body":"x"}`, http.StatusNotFound},
		{"no callback", `{"from":"` + handset + `","to":"` + owned + `","body":"x"}`, http.StatusConflict},
		{"bad number", `{"from":"abc","to":"` + owned + `","body":"x"}`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := do(t, http.MethodPost, hs.URL+"/mock/inbound", "", "", "application/json", tt.body)
			if resp.StatusCode != tt.status {
				t.Fatalf("status %d, want %d: %s", resp.StatusCode, tt.status, body)
			}
		})
	}
}

func TestRateLimit(t *testing.T) {
	_, hs := newServer(t, func(c *config.Config) { c.SMSPerSecond = 1 })
	statuses := make([]int, 0, 2)
	for range 2 {
		resp, _ := do(t, http.MethodPost, hs.URL+"/v2.2/messages", "ak", "sk", contentType, sendBody(handset, owned, "hi", ""))
		statuses = append(statuses, resp.StatusCode)
	}
	if statuses[0] != http.StatusAccepted || statuses[1] != http.StatusTooManyRequests {
		t.Errorf("statuses %v, want [202 429]", statuses)
	}
}

func TestSegmentCount(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		segments int
		encoding int
	}{
		{"empty", "", 1, encodingGSM7},
		{"gsm single", strings.Repeat("a", 160), 1, encodingGSM7},
		{"gsm two", strings.Repeat("a", 161), 2, encodingGSM7},
		{"gsm extension counts twice", strings.Repeat("€", 80), 1, encodingGSM7},
		{"gsm extension overflow", strings.Repeat("€", 81), 2, encodingGSM7},
		{"ucs2 single", strings.Repeat("é", 159) + "ł", 3, encodingUCS2},
		{"ucs2 seventy", strings.Repeat("ł", 70), 1, encodingUCS2},
		{"emoji surrogate pairs", strings.Repeat("😀", 35), 1, encodingUCS2},
		{"emoji overflow", strings.Repeat("😀", 36), 2, encodingUCS2},
		{"gsm escape not split", strings.Repeat("€", 153), 3, encodingGSM7},
		{"gsm escape fills parts", strings.Repeat("€", 152), 2, encodingGSM7},
		{"surrogate not split", strings.Repeat("😀", 67), 3, encodingUCS2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			segments, encoding := segmentCount(tt.body)
			if segments != tt.segments || encoding != tt.encoding {
				t.Errorf("segmentCount = %d, %d; want %d, %d", segments, encoding, tt.segments, tt.encoding)
			}
		})
	}
}

func TestInboundSMSPayload(t *testing.T) {
	rc := newReceiver(t)
	_, hs := newServer(t, func(c *config.Config) { c.Callback = rc.srv.URL })
	resp, body := do(t, http.MethodPost, hs.URL+"/mock/inbound", "", "", "application/json",
		`{"from":"`+handset+`","to":"`+owned+`","body":"`+strings.Repeat("a", 161)+`"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	attrs := rc.next(t)["data"].(map[string]any)["attributes"].(map[string]any)
	if attrs["amount_nanodollars"] != "8000000" || attrs["amount_display"] != "$0.0080" || attrs["status"] != "delivered" {
		t.Errorf("attributes = %v", attrs)
	}
	if _, ok := attrs["delivery_receipts"]; ok {
		t.Errorf("SMS has delivery_receipts: %v", attrs)
	}
}

func TestMMSReceiptBodyEmpty(t *testing.T) {
	rc := newReceiver(t)
	_, hs := newServer(t, func(c *config.Config) { c.Outcomes[handset] = "none" })
	b, err := json.Marshal(map[string]any{"data": map[string]any{
		"type": "message",
		"attributes": map[string]any{
			"to": handset, "from": owned, "body": "pic", "dlr_callback": rc.srv.URL,
			"media_urls": []string{"https://media.example/a.jpg"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	resp, body := do(t, http.MethodPost, hs.URL+"/v2.2/messages", "ak", "sk", contentType, string(b))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if got := rc.next(t)["data"].(map[string]any)["attributes"].(map[string]any)["body"]; got != "" {
		t.Errorf("MMS receipt body = %v, want empty", got)
	}
}

func TestCallbackRedirectIsFailure(t *testing.T) {
	rc := newReceiver(t)
	redirect := httptest.NewServer(http.RedirectHandler(rc.srv.URL, http.StatusFound))
	t.Cleanup(redirect.Close)
	s, hs := newServer(t, func(c *config.Config) {
		c.Numbers = []config.Number{{Number: owned, Callback: redirect.URL}}
		c.Attempts = 2
		c.RetryInterval = time.Millisecond
	})
	resp, body := do(t, http.MethodPost, hs.URL+"/mock/inbound", "", "", "application/json",
		`{"from":"`+handset+`","to":"`+owned+`","body":"x"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var created map[string]string
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	m, ok := s.get(created["id"])
	if !ok {
		t.Fatal("message not recorded")
	}
	if len(m.Attempts) != 2 || m.Attempts[1].StatusCode != http.StatusFound {
		t.Errorf("attempts = %+v, want two 302s", m.Attempts)
	}
	select {
	case <-rc.bodies:
		t.Error("redirect was followed")
	default:
	}
}

func TestFailingReceiptCallbackDoesNotDelayReceipts(t *testing.T) {
	rc := newReceiver(t, 500, 500, 500, 500, 500, 500, 500, 500)
	s, hs := newServer(t, func(c *config.Config) {
		c.RetryInterval = time.Hour
		c.Attempts = 2
	})
	resp, body := do(t, http.MethodPost, hs.URL+"/v2.2/messages", "ak", "sk", contentType, sendBody(handset, owned, "hi", rc.srv.URL))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	levels := map[float64]bool{}
	for range 2 {
		levels[rc.next(t)["data"].(map[string]any)["attributes"].(map[string]any)["level"].(float64)] = true
	}
	if !levels[1] || !levels[2] {
		t.Errorf("first attempts for levels %v, want 1 and 2 before any retry", levels)
	}
	var sent sendResponse
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatal(err)
	}
	if m, ok := s.get(sent.Data.ID); !ok || len(m.Receipts) != 2 {
		t.Errorf("%d receipts recorded, want 2", len(m.Receipts))
	}
}
