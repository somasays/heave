package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValidWithDefaults(t *testing.T) {
	c, err := Load(writeCfg(t, `
providers:
  - name: anthropic
    type: anthropic
    api_key_env: ANTHROPIC_API_KEY
models:
  - id: fast
    provider: anthropic
    upstream: claude-haiku-4-5
    price: { input_per_mtok: 1, output_per_mtok: 5 }
routing:
  default_model: fast
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Addr != ":8080" || c.Server.RequestTimeout != 120*time.Second || c.Server.MaxRequestBytes != 1<<20 {
		t.Fatalf("defaults not applied: %+v", c.Server)
	}
	if !c.Models[0].AcceptsSampling() {
		t.Fatal("sampling should default to true")
	}
}

func TestSamplingFalse(t *testing.T) {
	c, err := Load(writeCfg(t, `
providers: [{name: a, type: anthropic, api_key_env: K}]
models:
  - {id: smart, provider: a, upstream: claude-opus-4-8, sampling: false, price: {input_per_mtok: 5, output_per_mtok: 25}}
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Models[0].AcceptsSampling() {
		t.Fatal("sampling:false should disable sampling")
	}
}

func TestRejectDuplicateProvider(t *testing.T) {
	_, err := Load(writeCfg(t, `
providers:
  - {name: a, type: anthropic, api_key_env: K}
  - {name: a, type: openai, api_key_env: K2}
models: [{id: m, provider: a, upstream: x, price: {input_per_mtok: 1, output_per_mtok: 1}}]
`))
	if err == nil {
		t.Fatal("expected duplicate-provider error")
	}
}

func TestRejectUnknownProviderRef(t *testing.T) {
	_, err := Load(writeCfg(t, `
providers: [{name: a, type: anthropic, api_key_env: K}]
models: [{id: m, provider: ghost, upstream: x, price: {input_per_mtok: 1, output_per_mtok: 1}}]
`))
	if err == nil {
		t.Fatal("expected unknown-provider error")
	}
}

func TestRejectBadDefaultModel(t *testing.T) {
	_, err := Load(writeCfg(t, `
providers: [{name: a, type: anthropic, api_key_env: K}]
models: [{id: m, provider: a, upstream: x, price: {input_per_mtok: 1, output_per_mtok: 1}}]
routing: {default_model: nope}
`))
	if err == nil {
		t.Fatal("expected bad default_model error")
	}
}
