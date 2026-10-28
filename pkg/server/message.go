package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"time"
)

// Message is a sent or received message as the mock records it.
type Message struct {
	ID                   string    `json:"id"`
	Version              string    `json:"version"`
	Direction            string    `json:"direction"`
	From                 string    `json:"from"`
	To                   string    `json:"to"`
	Body                 string    `json:"body"`
	IsMMS                bool      `json:"is_mms"`
	MediaURLs            []string  `json:"media_urls,omitempty"`
	Media                []Media   `json:"media,omitempty"`
	MessageType          string    `json:"message_type"`
	Encoding             int       `json:"message_encoding"`
	Segments             int       `json:"segment_count"`
	BaseNanodollars      int64     `json:"base_rate_nanodollars"`
	SurchargeNanodollars int64     `json:"surcharge_nanodollars"`
	AmountNanodollars    int64     `json:"amount_nanodollars"`
	Callback             string    `json:"callback,omitempty"`
	DLRCallback          string    `json:"dlr_callback,omitempty"`
	Outcome              string    `json:"outcome,omitempty"`
	Timestamp            time.Time `json:"timestamp"`
	Receipts             []Receipt `json:"delivery_receipts"`
	Attempts             []Attempt `json:"callback_attempts"`
}

// Media is an inbound MMS attachment.
type Media struct {
	URL      string `json:"url"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
	FileName string `json:"file_name"`
}

// Receipt is a delivery receipt at Flowroute level 1 (to the carrier) or
// level 2 (from the carrier).
type Receipt struct {
	Level       int       `json:"level"`
	Status      string    `json:"status"`
	StatusCode  *int      `json:"status_code"`
	Description string    `json:"status_code_description"`
	Timestamp   time.Time `json:"timestamp"`
}

// Attempt is one POST of a callback.
type Attempt struct {
	Kind       string    `json:"kind"`
	URL        string    `json:"url"`
	Level      int       `json:"level,omitempty"`
	StatusCode int       `json:"status_code,omitempty"`
	Error      string    `json:"error,omitempty"`
	At         time.Time `json:"at"`
}

// Status is the latest receipt's status, or the status at creation.
func (m *Message) Status() string {
	if n := len(m.Receipts); n > 0 {
		return m.Receipts[n-1].Status
	}
	if m.Direction == "inbound" && !m.IsMMS {
		return "delivered"
	}
	if m.Direction == "inbound" {
		return ""
	}
	return "message created"
}

func (m *Message) clone() *Message {
	c := *m
	c.MediaURLs = slices.Clone(m.MediaURLs)
	c.Media = slices.Clone(m.Media)
	c.Receipts = slices.Clone(m.Receipts)
	c.Attempts = slices.Clone(m.Attempts)
	return &c
}

func (s *Server) put(m *Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages[m.ID] = m
	s.order = append(s.order, m.ID)
}

func (s *Server) get(id string) (*Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.messages[id]
	if !ok {
		return nil, false
	}
	return m.clone(), true
}

func (s *Server) list() []*Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Message, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.messages[id].clone())
	}
	return out
}

func (s *Server) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = make(map[string]*Message)
	s.order = nil
}

// update applies fn to the stored message, if it still exists.
func (s *Server) update(id string, fn func(*Message)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.messages[id]; ok {
		fn(m)
	}
}

var bufferedCode = 1003

// level2 returns the level 2 receipt an outcome produces, if any.
func level2(outcome string) (Receipt, bool) {
	switch outcome {
	case "delivered":
		return Receipt{Level: 2, Status: "delivered", Description: "Message delivered to handset"}, true
	case "buffered":
		return Receipt{
			Level:       2,
			Status:      "message buffered",
			StatusCode:  &bufferedCode,
			Description: "Message accepted by the SMSC but the phone is off or out of reach. The message is buffered.",
		}, true
	case "undelivered":
		return Receipt{Level: 2, Status: "undelivered", Description: "Message undelivered"}, true
	}
	return Receipt{}, false
}

// deliver records the level 1 receipt after the DLR delay, then the level 2
// receipt for the message's outcome, posting each to the DLR callback.
func (s *Server) deliver(id string) {
	defer s.wg.Done()
	m, ok := s.get(id)
	if !ok {
		return
	}

	steps := []Receipt{{Level: 1, Status: "smsc submit", Description: "Message has been sent"}}
	if rc, ok := level2(m.Outcome); ok {
		steps = append(steps, rc)
	}
	for _, rc := range steps {
		if !s.wait(s.cfg.DLRDelay) {
			return
		}
		rc.Timestamp = time.Now().UTC()
		s.update(id, func(m *Message) { m.Receipts = append(m.Receipts, rc) })
		if m.DLRCallback != "" {
			s.post(id, "dlr", m.DLRCallback, rc.Level, dlrPayload(m, rc, s.selfURL(m.Version, m.ID)))
		}
	}
}

func dlrPayload(m *Message, rc Receipt, related string) dlrCallback {
	var p dlrCallback
	p.Data.ID = m.ID
	p.Data.Type = "delivery_receipt"
	p.Data.Links.Related = related
	if !m.IsMMS {
		p.Data.Attributes.Body = m.Body
	}
	p.Data.Attributes.Level = rc.Level
	p.Data.Attributes.Status = rc.Status
	if rc.StatusCode != nil {
		code := fmt.Sprint(*rc.StatusCode)
		p.Data.Attributes.StatusCode = &code
	}
	p.Data.Attributes.StatusCodeDescription = rc.Description
	p.Data.Attributes.Timestamp = rc.Timestamp.Format(time.RFC3339)
	return p
}

// post makes the first attempt at a callback inline, so callbacks for one
// message go out in order, and retries the rest in the background at the
// retry interval until a 200 or the last attempt. Flowroute treats anything
// but 200 as a failure.
func (s *Server) post(id, kind, url string, level int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		slog.Error("encode callback", "id", id, "err", err)
		return
	}
	if s.attempt(id, kind, url, level, body, 1) || s.cfg.Attempts < 2 {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for n := 2; n <= s.cfg.Attempts; n++ {
			if !s.wait(s.cfg.RetryInterval) || s.attempt(id, kind, url, level, body, n) {
				return
			}
		}
	}()
}

// attempt posts body once and records the result.
func (s *Server) attempt(id, kind, url string, level int, body []byte, n int) bool {
	a := Attempt{Kind: kind, URL: url, Level: level, At: time.Now().UTC()}
	var err error
	a.StatusCode, err = s.send(url, body)
	if err != nil {
		a.Error = err.Error()
	}
	s.update(id, func(m *Message) { m.Attempts = append(m.Attempts, a) })
	if a.StatusCode == http.StatusOK {
		return true
	}
	slog.Warn("callback failed", "id", id, "kind", kind, "attempt", n, "status", a.StatusCode, "err", a.Error)
	return false
}

func (s *Server) send(url string, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(s.ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	_, copyErr := io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, errors.Join(copyErr, resp.Body.Close())
}

// wait returns false if the server's context ends first.
func (s *Server) wait(d time.Duration) bool {
	if d <= 0 {
		return s.ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-s.ctx.Done():
		return false
	}
}
