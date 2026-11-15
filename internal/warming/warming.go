// Package warming schedules transient keep-alive model requests after a
// session goes idle, preserving provider prompt caches.
package warming

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

const defaultPrompt = "Do not perform any work. Reply with exactly: OK"

// Config accepts either a boolean or an object in the configuration file.
type Config struct {
	Enabled  bool   `json:"enabled"`
	Prompt   string `json:"prompt,omitempty"`
	Interval string `json:"interval,omitempty"`
	Duration string `json:"duration,omitempty"`
}

// Default returns disabled warming with the documented defaults.
func Default() Config {
	return Config{
		Enabled:  false,
		Prompt:   defaultPrompt,
		Interval: "4m",
		Duration: "30m",
	}
}

// UnmarshalJSON accepts `"warming": true` and the object form.
func (c *Config) UnmarshalJSON(data []byte) error {
	var enabled bool
	if err := json.Unmarshal(data, &enabled); err == nil {
		*c = Default()
		c.Enabled = enabled
		return nil
	}
	var input struct {
		Enabled  *bool  `json:"enabled"`
		Prompt   string `json:"prompt"`
		Interval string `json:"interval"`
		Duration string `json:"duration"`
	}
	if err := json.Unmarshal(data, &input); err != nil {
		return err
	}
	*c = Default()
	c.Enabled = true
	if input.Enabled != nil {
		c.Enabled = *input.Enabled
	}
	if strings.TrimSpace(input.Prompt) != "" {
		c.Prompt = input.Prompt
	}
	if strings.TrimSpace(input.Interval) != "" {
		c.Interval = input.Interval
	}
	if strings.TrimSpace(input.Duration) != "" {
		c.Duration = input.Duration
	}
	return nil
}

// Normalize fills omitted values and disables invalid timing combinations.
func Normalize(input Config) Config {
	out := Default()
	out.Enabled = input.Enabled
	if strings.TrimSpace(input.Prompt) != "" {
		out.Prompt = input.Prompt
	}
	if strings.TrimSpace(input.Interval) != "" {
		out.Interval = strings.TrimSpace(input.Interval)
	}
	if strings.TrimSpace(input.Duration) != "" {
		out.Duration = strings.TrimSpace(input.Duration)
	}
	interval, intervalOK := intervalValue(out.Interval, 0)
	if strings.EqualFold(strings.TrimSpace(out.Interval), "auto") {
		intervalOK = true
		interval = 0
	}
	duration, durationOK := parseDuration(out.Duration)
	if !intervalOK || !durationOK || (interval > 0 && interval >= duration) {
		out.Enabled = false
	}
	return out
}

// IntervalFor returns the next delay. "auto" uses 90% of the model's cache TTL
// and returns zero when the model declares no TTL.
func (c Config) IntervalFor(cacheTTL time.Duration) time.Duration {
	interval, ok := intervalValue(c.Interval, cacheTTL)
	if !ok {
		return 0
	}
	return interval
}

// DurationValue returns the configured active window.
func (c Config) DurationValue() time.Duration {
	duration, _ := parseDuration(c.Duration)
	return duration
}

func intervalValue(value string, cacheTTL time.Duration) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, "auto") {
		if cacheTTL <= 0 {
			return 0, false
		}
		return time.Duration(float64(cacheTTL) * 0.9), true
	}
	return parseDuration(value)
}

func parseDuration(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if duration, err := time.ParseDuration(value); err == nil && duration > 0 {
		return duration, true
	}
	fields := strings.Fields(strings.ToLower(value))
	if len(fields) != 2 {
		return 0, false
	}
	amount, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || amount <= 0 {
		return 0, false
	}
	var unit time.Duration
	switch fields[1] {
	case "second", "seconds":
		unit = time.Second
	case "minute", "minutes":
		unit = time.Minute
	case "hour", "hours":
		unit = time.Hour
	case "day", "days":
		unit = 24 * time.Hour
	default:
		return 0, false
	}
	return time.Duration(amount * float64(unit)), true
}
