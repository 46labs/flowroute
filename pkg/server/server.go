package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/46labs/flowroute/pkg/config"
)

var e164 = regexp.MustCompile(`^\+?[1-9][0-9]{6,14}$`)

// Server is the Flowroute Messaging API mock. Its state lives in memory.
type Server struct {
	cfg     *config.Config
	ctx     context.Context
	client  *http.Client
	numbers map[string]config.Number
	sms     *rate.Limiter
	mms     *rate.Limiter
	wg      sync.WaitGroup

	mu       sync.Mutex
	messages map[string]*Message
	order    []string
}

// New returns a server whose callbacks stop when ctx is done.
func New(ctx context.Context, cfg *config.Config) *Server {
	numbers := make(map[string]config.Number, len(cfg.Numbers))
	for _, n := range cfg.Numbers {
		numbers[digits(n.Number)] = n
	}
	return &Server{
		cfg: cfg,
		ctx: ctx,
		client: &http.Client{
			Timeout: 10 * time.Second,
			// A redirect is not a 200 from the callback, so it counts as a
			// failed attempt.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		numbers:  numbers,
		sms:      limiter(cfg.SMSPerSecond),
		mms:      limiter(cfg.MMSPerSecond),
		messages: make(map[string]*Message),
	}
}

func limiter(perSecond float64) *rate.Limiter {
	if perSecond <= 0 {
		return nil
	}
	return rate.NewLimiter(rate.Limit(perSecond), max(1, int(perSecond)))
}

// Handler serves the Flowroute API under /v2.1 and /v2.2 and the mock
// control API under /mock.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /{version}/messages", s.withAuth(s.handleSend))
	mux.HandleFunc("GET /{version}/messages/{id}", s.withAuth(s.handleLookup))

	mux.HandleFunc("POST /mock/inbound", s.handleInbound)
	mux.HandleFunc("GET /mock/messages", s.handleListMessages)
	mux.HandleFunc("GET /mock/messages/{id}", s.handleGetMessage)
	mux.HandleFunc("DELETE /mock/messages", s.handleResetMessages)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// Wait blocks until every scheduled receipt and callback has finished.
func (s *Server) Wait() {
	s.wg.Wait()
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if v := r.PathValue("version"); v != "v2.1" && v != "v2.2" {
			writeError(w, http.StatusNotFound, "Not Found", "unknown API version")
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(user), []byte(s.cfg.AccessKey)) != 1 ||
			subtle.ConstantTimeCompare([]byte(pass), []byte(s.cfg.SecretKey)) != 1 {
			writeError(w, http.StatusUnauthorized, "Unauthorized", "Authentication failed.")
			return
		}
		next(w, r)
	}
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	version := r.PathValue("version")
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != contentType {
		writeError(w, http.StatusUnsupportedMediaType, "Unsupported Media Type", "Content-Type must be "+contentType)
		return
	}

	var req sendRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Bad Request", "invalid JSON:API document")
		return
	}
	a := req.sendAttributes
	if req.Data != nil {
		if req.Data.Type != "message" {
			writeError(w, http.StatusBadRequest, "Bad Request", `data.type must be "message"`)
			return
		}
		a = req.Data.Attributes
	}
	switch {
	case !e164.MatchString(a.To) || !e164.MatchString(a.From):
		writeError(w, http.StatusBadRequest, "Bad Request", "to and from must be E.164 numbers")
		return
	case a.Body == "" && len(a.MediaURLs) == 0:
		writeError(w, http.StatusBadRequest, "Bad Request", "body or media_urls is required")
		return
	}
	if _, ok := s.numbers[digits(a.From)]; !ok {
		writeError(w, http.StatusForbidden, "ActionNotAuthorizedError", "")
		return
	}

	isMMS := a.IsMMS || len(a.MediaURLs) > 0
	lim := s.sms
	if isMMS {
		lim = s.mms
	}
	if lim != nil && !lim.Allow() {
		writeError(w, http.StatusTooManyRequests, "Too Many Requests", "rate limit exceeded")
		return
	}

	id, err := newID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Internal Server Error", err.Error())
		return
	}
	segments, encoding := segmentCount(a.Body)
	if isMMS {
		segments = 1
	}
	m := &Message{
		ID:                   id,
		Version:              version,
		Direction:            "outbound",
		From:                 digits(a.From),
		To:                   digits(a.To),
		Body:                 a.Body,
		IsMMS:                isMMS,
		MediaURLs:            a.MediaURLs,
		MessageType:          messageType(a.From),
		Encoding:             encoding,
		Segments:             segments,
		BaseNanodollars:      s.cfg.BaseRateNanodollars,
		SurchargeNanodollars: s.cfg.SurchargeNanodollars,
		AmountNanodollars:    (s.cfg.BaseRateNanodollars + s.cfg.SurchargeNanodollars) * int64(segments),
		DLRCallback:          firstNonEmpty(a.DLRCallback, s.cfg.DLRCallback),
		Outcome:              s.outcome(a.To),
		Timestamp:            time.Now().UTC(),
	}
	s.put(m)

	var resp sendResponse
	resp.Data.ID = m.ID
	resp.Data.Type = "message"
	resp.Data.Links.Self = s.selfURL(version, m.ID)
	if version == "v2.2" {
		resp.Data.PriceDetails = &sendPrice{
			BaseRate:     json.Number(decimal(m.BaseNanodollars, 4)),
			ChargedCost:  json.Number(decimal(m.AmountNanodollars, 4)),
			SegmentCount: m.Segments,
			Surcharge:    json.Number(decimal(m.SurchargeNanodollars, 4)),
		}
	}
	writeJSON(w, http.StatusAccepted, resp)

	s.wg.Add(1)
	go s.deliver(m.ID)
}

func (s *Server) handleLookup(w http.ResponseWriter, r *http.Request) {
	version := r.PathValue("version")
	m, ok := s.get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "Not Found", "message not found")
		return
	}

	var resp lookupResponse
	resp.Data.ID = m.ID
	resp.Data.Type = "message"
	resp.Data.Links.Self = s.selfURL(version, m.ID)
	a := &resp.Data.Attributes
	a.AmountDisplay = "$" + decimal(m.AmountNanodollars, 4)
	a.AmountNanodollars = m.AmountNanodollars
	a.Body = m.Body
	a.Direction = m.Direction
	a.From = m.From
	a.To = m.To
	a.IsMMS = m.IsMMS
	a.MessageEncoding = m.Encoding
	a.MessageType = m.MessageType
	a.Status = m.Status()
	a.Timestamp = timestamp(m.Timestamp)
	a.DeliveryReceipts = []lookupReceipt{}
	for i := len(m.Receipts) - 1; i >= 0; i-- {
		rc := m.Receipts[i]
		a.DeliveryReceipts = append(a.DeliveryReceipts, lookupReceipt{
			Level:                 rc.Level,
			Status:                rc.Status,
			StatusCode:            rc.StatusCode,
			StatusCodeDescription: rc.Description,
			Timestamp:             timestamp(rc.Timestamp),
		})
	}
	if version == "v2.2" && m.Direction == "outbound" {
		a.PriceDetails = &lookupPrice{
			BaseRate:      decimal(m.BaseNanodollars, 6),
			ChargedCost:   decimal(m.AmountNanodollars, 6),
			SegmentCount:  m.Segments,
			SurchargeRate: decimal(m.SurchargeNanodollars, 6),
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) outcome(to string) string {
	if o, ok := s.cfg.Outcomes[to]; ok {
		return o
	}
	if o, ok := s.cfg.Outcomes[digits(to)]; ok {
		return o
	}
	return s.cfg.DefaultOutcome
}

func (s *Server) selfURL(version, id string) string {
	return fmt.Sprintf("%s/%s/messages/%s", strings.TrimRight(s.cfg.BaseURL, "/"), version, id)
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("message id: %w", err)
	}
	return "mdr2-" + hex.EncodeToString(b), nil
}

func digits(number string) string {
	return strings.TrimPrefix(number, "+")
}

func messageType(from string) string {
	d := digits(from)
	if len(d) == 11 && d[0] == '1' {
		switch d[1:4] {
		case "800", "833", "844", "855", "866", "877", "888":
			return "toll-free"
		}
	}
	return "longcode"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func timestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.00Z")
}

// decimal renders nanodollars as dollars with the given number of places,
// truncating, without floating point.
func decimal(nanodollars int64, places int) string {
	sign := ""
	if nanodollars < 0 {
		sign = "-"
		nanodollars = -nanodollars
	}
	frac := fmt.Sprintf("%09d", nanodollars%1_000_000_000)[:places]
	return fmt.Sprintf("%s%d.%s", sign, nanodollars/1_000_000_000, frac)
}
