package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Number is a number on the mock account. Inbound messages to it are posted
// to Callback, or to the account callback when Callback is empty.
type Number struct {
	Number   string `mapstructure:"number"`
	Callback string `mapstructure:"callback"`
}

// Config is the mock's account, behaviour and seed data.
type Config struct {
	Port      int
	BaseURL   string
	AccessKey string
	SecretKey string

	// Account-level SMS and DLR callbacks.
	Callback    string
	DLRCallback string

	Numbers []Number

	// Outcomes maps a destination number to the outcome of messages sent to
	// it: delivered, buffered, undelivered or none. The default is delivered.
	Outcomes       map[string]string
	DefaultOutcome string

	DLRDelay      time.Duration
	RetryInterval time.Duration
	Attempts      int

	SMSPerSecond float64
	MMSPerSecond float64

	BaseRateNanodollars    int64
	SurchargeNanodollars   int64
	InboundRateNanodollars int64
}

func init() {
	viper.SetDefault("port", 8080)
	viper.SetDefault("base_url", "https://api.flowroute.com")
	viper.SetDefault("access_key", "test_access_key")
	viper.SetDefault("secret_key", "test_secret_key")
	viper.SetDefault("default_outcome", "delivered")
	viper.SetDefault("dlr_delay", "1s")
	viper.SetDefault("retry_interval", "30s")
	viper.SetDefault("attempts", 4)
	viper.SetDefault("sms_per_second", 5)
	viper.SetDefault("mms_per_second", 1)
	viper.SetDefault("base_rate_nanodollars", 3500000)
	viper.SetDefault("surcharge_nanodollars", 0)
	viper.SetDefault("inbound_rate_nanodollars", 4000000)

	viper.AutomaticEnv()
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("/config")
	viper.AddConfigPath(".")
}

// Load reads config.yaml from /config or the working directory, with
// environment variables taking precedence.
func Load() (*Config, error) {
	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("read config: %w", err)
		}
	}

	cfg := &Config{
		Port:                   viper.GetInt("port"),
		BaseURL:                viper.GetString("base_url"),
		AccessKey:              viper.GetString("access_key"),
		SecretKey:              viper.GetString("secret_key"),
		Callback:               viper.GetString("callback"),
		DLRCallback:            viper.GetString("dlr_callback"),
		Outcomes:               viper.GetStringMapString("outcomes"),
		DefaultOutcome:         viper.GetString("default_outcome"),
		DLRDelay:               viper.GetDuration("dlr_delay"),
		RetryInterval:          viper.GetDuration("retry_interval"),
		Attempts:               viper.GetInt("attempts"),
		SMSPerSecond:           viper.GetFloat64("sms_per_second"),
		MMSPerSecond:           viper.GetFloat64("mms_per_second"),
		BaseRateNanodollars:    viper.GetInt64("base_rate_nanodollars"),
		SurchargeNanodollars:   viper.GetInt64("surcharge_nanodollars"),
		InboundRateNanodollars: viper.GetInt64("inbound_rate_nanodollars"),
	}
	if err := viper.UnmarshalKey("numbers", &cfg.Numbers); err != nil {
		return nil, fmt.Errorf("unmarshal numbers: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate rejects outcomes the mock cannot produce, and two spellings of one
// number with different outcomes.
func (c *Config) Validate() error {
	if !ValidOutcome(c.DefaultOutcome) {
		return fmt.Errorf("default_outcome %q: want delivered, buffered, undelivered or none", c.DefaultOutcome)
	}
	seen := make(map[string]string, len(c.Outcomes))
	for number, outcome := range c.Outcomes {
		if !ValidOutcome(outcome) {
			return fmt.Errorf("outcome for %s %q: want delivered, buffered, undelivered or none", number, outcome)
		}
		// Numbers match with or without "+", so both spellings must agree.
		key := strings.TrimPrefix(number, "+")
		if o, ok := seen[key]; ok && o != outcome {
			return fmt.Errorf("outcomes for %s disagree: %q and %q", number, o, outcome)
		}
		seen[key] = outcome
	}
	if c.Attempts < 1 {
		return fmt.Errorf("attempts %d: want at least 1", c.Attempts)
	}
	return nil
}

// ValidOutcome reports whether outcome is one the mock can produce.
func ValidOutcome(outcome string) bool {
	switch outcome {
	case "delivered", "buffered", "undelivered", "none":
		return true
	}
	return false
}
