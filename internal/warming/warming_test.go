package warming

import (
	"encoding/json"
	"testing"
	"time"
)

func TestConfigAcceptsBooleanAndObject(t *testing.T) {
	var enabled Config
	if err := json.Unmarshal([]byte("true"), &enabled); err != nil || !enabled.Enabled || enabled.Interval != "4m" {
		t.Fatalf("unexpected boolean config: %+v %v", enabled, err)
	}
	var object Config
	body := `{"prompt":"stay warm","interval":"2m","duration":"1h"}`
	if err := json.Unmarshal([]byte(body), &object); err != nil {
		t.Fatal(err)
	}
	if !object.Enabled || object.Prompt != "stay warm" || object.Interval != "2m" || object.Duration != "1h" {
		t.Fatalf("unexpected object config: %+v", object)
	}
}

func TestIntervalForAutoUsesCacheTTL(t *testing.T) {
	config := Normalize(Config{Enabled: true, Interval: "auto", Duration: "1h"})
	if got := config.IntervalFor(5 * time.Minute); got != 4*time.Minute+30*time.Second {
		t.Fatalf("unexpected auto interval: %s", got)
	}
	if got := config.IntervalFor(0); got != 0 {
		t.Fatalf("auto without TTL should disable warming: %s", got)
	}
	if !config.Enabled {
		t.Fatal("valid config should stay enabled")
	}
}

func TestNormalizeDisablesInvalidTiming(t *testing.T) {
	config := Normalize(Config{Enabled: true, Interval: "1h", Duration: "30m"})
	if config.Enabled {
		t.Fatal("interval >= duration should disable warming")
	}
	invalid := Normalize(Config{Enabled: true, Interval: "bad", Duration: "30m"})
	if invalid.Enabled {
		t.Fatal("invalid duration should disable warming")
	}
}

func TestDefaultPrompt(t *testing.T) {
	config := Normalize(Config{Enabled: true})
	if config.Prompt == "" || config.Interval != "4m" || config.Duration != "30m" {
		t.Fatalf("unexpected defaults: %+v", config)
	}
}

func TestHumanDuration(t *testing.T) {
	config := Normalize(Config{Enabled: true, Interval: "4 minutes", Duration: "1 hour"})
	if !config.Enabled || config.IntervalFor(0) != 4*time.Minute || config.DurationValue() != time.Hour {
		t.Fatalf("unexpected human duration config: %+v", config)
	}
}
