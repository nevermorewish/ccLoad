package model

import (
	"encoding/json"
	"testing"
)

func TestChannelPassthroughDefaultsAndPersistence(t *testing.T) {
	for _, raw := range []string{`{}`, `{"custom_request_rules":null}`, `{"custom_request_rules":{"anthropic_passthrough":true}}`, `{"custom_request_rules":{"force_upstream_stream":true}}`} {
		var cfg Config
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			t.Fatal(err)
		}
		if !cfg.PassthroughEnabled() {
			t.Fatalf("default disabled: %s", raw)
		}
	}
	var cfg Config
	if err := json.Unmarshal([]byte(`{"custom_request_rules":{"passthrough":false,"anthropic_passthrough":true}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.PassthroughEnabled() || cfg.CustomRequestRules.IsEmpty() {
		t.Fatal("explicit false must override the legacy flag and survive storage")
	}
	clone := cfg.CustomRequestRules.Clone()
	*clone.Passthrough = true
	if cfg.PassthroughEnabled() {
		t.Fatal("clone shares passthrough pointer")
	}
	encoded, err := json.Marshal(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Config
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.PassthroughEnabled() {
		t.Fatalf("explicit false lost: %s, %v", encoded, err)
	}
	for _, auth := range []string{AuthTypeAnthropicOAuth, AuthTypeCodexOAuth, AuthTypeSystemOne} {
		if (&Config{AuthType: auth, CustomRequestRules: clone}).PassthroughEnabled() {
			t.Fatalf("provider contract bypassed: %s", auth)
		}
	}
}
