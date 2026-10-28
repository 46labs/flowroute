package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
)

const contentType = "application/vnd.api+json"

type sendAttributes struct {
	To          string   `json:"to"`
	From        string   `json:"from"`
	Body        string   `json:"body"`
	MediaURLs   []string `json:"media_urls"`
	IsMMS       bool     `json:"is_mms"`
	DLRCallback string   `json:"dlr_callback"`
}

// sendRequest accepts the JSON:API document and the compact form, which
// carries the attributes at the top level.
type sendRequest struct {
	Data *struct {
		Type       string         `json:"type"`
		Attributes sendAttributes `json:"attributes"`
	} `json:"data"`
	sendAttributes
}

type links struct {
	Self    string `json:"self,omitempty"`
	Related string `json:"related,omitempty"`
}

// sendPrice is the price_details of a v2.2 send response, where the money
// fields are JSON numbers.
type sendPrice struct {
	BaseRate     json.Number `json:"base_rate"`
	ChargedCost  json.Number `json:"charged_cost"`
	SegmentCount int         `json:"segment_count"`
	Surcharge    json.Number `json:"surcharge"`
}

type sendResponse struct {
	Data struct {
		ID           string     `json:"id"`
		Links        links      `json:"links"`
		PriceDetails *sendPrice `json:"price_details,omitempty"`
		Type         string     `json:"type"`
	} `json:"data"`
}

// lookupPrice is the price_details of a v2.2 MDR lookup, where the money
// fields are strings.
type lookupPrice struct {
	BaseRate      string `json:"base_rate"`
	ChargedCost   string `json:"charged_cost"`
	SegmentCount  int    `json:"segment_count"`
	SurchargeRate string `json:"surcharge_rate"`
}

type lookupReceipt struct {
	Level                 int    `json:"level"`
	Status                string `json:"status"`
	StatusCode            *int   `json:"status_code"`
	StatusCodeDescription string `json:"status_code_description"`
	Timestamp             string `json:"timestamp"`
}

type lookupAttributes struct {
	AmountDisplay     string          `json:"amount_display"`
	AmountNanodollars int64           `json:"amount_nanodollars"`
	Body              string          `json:"body"`
	DeliveryReceipts  []lookupReceipt `json:"delivery_receipts"`
	Direction         string          `json:"direction"`
	From              string          `json:"from"`
	IsMMS             bool            `json:"is_mms"`
	MessageEncoding   int             `json:"message_encoding"`
	MessageType       string          `json:"message_type"`
	PriceDetails      *lookupPrice    `json:"price_details,omitempty"`
	Status            string          `json:"status"`
	Timestamp         string          `json:"timestamp"`
	To                string          `json:"to"`
}

type lookupResponse struct {
	Data struct {
		Attributes lookupAttributes `json:"attributes"`
		ID         string           `json:"id"`
		Links      links            `json:"links"`
		Type       string           `json:"type"`
	} `json:"data"`
}

type inboundAttributes struct {
	AmountDisplay string `json:"amount_display"`
	// A string for SMS and a number for MMS, as Flowroute sends them.
	AmountNanodollars  any      `json:"amount_nanodollars"`
	Body               string   `json:"body"`
	DeliveryReceipts   []string `json:"delivery_receipts,omitzero"`
	Direction          string   `json:"direction"`
	From               string   `json:"from"`
	IsMMS              bool     `json:"is_mms"`
	MessageCallbackURL string   `json:"message_callback_url"`
	MessageEncoding    int      `json:"message_encoding"`
	MessageType        string   `json:"message_type"`
	Status             string   `json:"status"`
	Timestamp          string   `json:"timestamp"`
	To                 string   `json:"to"`
}

type resourceRef struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

type mediaResource struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Attributes struct {
		FileName string `json:"file_name"`
		FileSize int64  `json:"file_size"`
		MimeType string `json:"mime_type"`
		URL      string `json:"url"`
	} `json:"attributes"`
}

type inboundCallback struct {
	Data struct {
		Attributes    inboundAttributes `json:"attributes"`
		ID            string            `json:"id"`
		Relationships *struct {
			Media struct {
				Data []resourceRef `json:"data"`
			} `json:"media"`
		} `json:"relationships,omitempty"`
		Type string `json:"type"`
	} `json:"data"`
	Included []mediaResource `json:"included,omitempty"`
}

type dlrCallback struct {
	Data struct {
		Attributes struct {
			Body                  string  `json:"body"`
			Level                 int     `json:"level"`
			Status                string  `json:"status"`
			StatusCode            *string `json:"status_code"`
			StatusCodeDescription string  `json:"status_code_description"`
			Timestamp             string  `json:"timestamp"`
		} `json:"attributes"`
		ID    string `json:"id"`
		Links links  `json:"links"`
		Type  string `json:"type"`
	} `json:"data"`
}

type apiError struct {
	Status int    `json:"status"`
	Detail string `json:"detail"`
	Title  string `json:"title"`
}

// decode reads exactly one JSON value from the request body.
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after JSON value")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write response", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, title, detail string) {
	writeJSON(w, status, map[string][]apiError{
		"errors": {{Status: status, Detail: detail, Title: title}},
	})
}
