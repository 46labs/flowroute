package config

import "testing"

func TestValidateOutcomes(t *testing.T) {
	tests := []struct {
		name     string
		outcomes map[string]string
		ok       bool
	}{
		{"one spelling", map[string]string{"+14035550199": "undelivered"}, true},
		{"both spellings agree", map[string]string{"+14035550199": "undelivered", "14035550199": "undelivered"}, true},
		{"both spellings disagree", map[string]string{"+14035550199": "undelivered", "14035550199": "delivered"}, false},
		{"unknown outcome", map[string]string{"+14035550199": "lost"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Config{DefaultOutcome: "delivered", Attempts: 1, Outcomes: tt.outcomes}
			if err := c.Validate(); (err == nil) != tt.ok {
				t.Fatalf("Validate() = %v, want ok %v", err, tt.ok)
			}
		})
	}
}
