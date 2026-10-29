package server

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"time"
)

type inboundRequest struct {
	From  string  `json:"from"`
	To    string  `json:"to"`
	Body  string  `json:"body"`
	Media []Media `json:"media"`
	// Callback overrides the number's and the account's callback.
	Callback string `json:"callback"`
}

// handleInbound simulates a message from a handset to a number on the
// account, posting it to the request's, the number's or the account's
// callback with Flowroute's retries.
func (s *Server) handleInbound(w http.ResponseWriter, r *http.Request) {
	var req inboundRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Bad Request", "invalid JSON")
		return
	}
	if !e164.MatchString(req.From) || !e164.MatchString(req.To) {
		writeError(w, http.StatusBadRequest, "Bad Request", "to and from must be E.164 numbers")
		return
	}
	number, ok := s.numbers[digits(req.To)]
	if !ok {
		writeError(w, http.StatusNotFound, "Not Found", "to is not a number on the account")
		return
	}
	if u, err := url.Parse(req.Callback); req.Callback != "" && (err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "") {
		writeError(w, http.StatusBadRequest, "Bad Request", "callback must be an http or https URL")
		return
	}
	callback := firstNonEmpty(req.Callback, number.Callback, s.cfg.Callback)
	if callback == "" {
		writeError(w, http.StatusConflict, "Conflict", "no callback configured for "+req.To)
		return
	}
	id, err := newID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Internal Server Error", err.Error())
		return
	}

	segments, encoding := segmentCount(req.Body)
	if len(req.Media) > 0 {
		segments = 1
	}
	m := &Message{
		ID:                id,
		Version:           "v2.2",
		Direction:         "inbound",
		From:              digits(req.From),
		To:                digits(req.To),
		Body:              req.Body,
		IsMMS:             len(req.Media) > 0,
		Media:             req.Media,
		MessageType:       messageType(req.To),
		Encoding:          encoding,
		Segments:          segments,
		AmountNanodollars: s.cfg.InboundRateNanodollars * int64(segments),
		Callback:          callback,
		Timestamp:         time.Now().UTC(),
	}
	s.put(m)
	payload := inboundPayload(m, "+"+m.From, "+"+m.To)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.post(m.ID, "message", callback, 0, payload)
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"id": m.ID})
}

func inboundPayload(m *Message, from, to string) inboundCallback {
	var p inboundCallback
	p.Data.ID = m.ID
	p.Data.Type = "message"
	p.Data.Attributes = inboundAttributes{
		AmountDisplay:      "$" + decimal(m.AmountNanodollars, 4),
		AmountNanodollars:  strconv.FormatInt(m.AmountNanodollars, 10),
		Body:               m.Body,
		Direction:          "inbound",
		From:               from,
		IsMMS:              m.IsMMS,
		MessageCallbackURL: m.Callback,
		MessageEncoding:    m.Encoding,
		MessageType:        m.MessageType,
		Status:             m.Status(),
		Timestamp:          timestamp(m.Timestamp),
		To:                 to,
	}
	if !m.IsMMS {
		return p
	}
	p.Data.Attributes.AmountNanodollars = m.AmountNanodollars
	p.Data.Attributes.DeliveryReceipts = []string{}
	p.Data.Relationships = &struct {
		Media struct {
			Data []resourceRef `json:"data"`
		} `json:"media"`
	}{}
	for i, md := range m.Media {
		ref := resourceRef{ID: fmt.Sprintf("%s-%d", m.ID, i+1), Type: "media"}
		p.Data.Relationships.Media.Data = append(p.Data.Relationships.Media.Data, ref)
		res := mediaResource{ID: ref.ID, Type: "media"}
		res.Attributes.FileName = md.FileName
		if res.Attributes.FileName == "" {
			if u, err := url.Parse(md.URL); err == nil {
				res.Attributes.FileName = path.Base(u.Path)
			}
		}
		res.Attributes.FileSize = md.FileSize
		res.Attributes.MimeType = md.MimeType
		res.Attributes.URL = md.URL
		p.Included = append(p.Included, res)
	}
	return p
}

func (s *Server) handleListMessages(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string][]*Message{"messages": s.list()})
}

func (s *Server) handleGetMessage(w http.ResponseWriter, r *http.Request) {
	m, ok := s.get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "Not Found", "message not found")
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleResetMessages(w http.ResponseWriter, _ *http.Request) {
	s.reset()
	w.WriteHeader(http.StatusNoContent)
}
