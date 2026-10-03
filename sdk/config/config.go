// Package config resolves a module's typed configuration. Precedence is
// fixed: the handshake-delivered document from core beats environment
// variables beat the caller's defaults. There is no config file layer —
// files are a core concern; modules receive their config, they do not go
// looking for it.
package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// Load unmarshals the handshake-delivered JSON document into dst, which the
// caller pre-fills with defaults. doc may be nil or empty (no config
// delivered), which leaves dst untouched. Env overrides are the caller's
// concern via Env where wanted.
func Load(doc []byte, dst any) error {
	if len(doc) == 0 {
		return nil
	}
	if err := json.Unmarshal(doc, dst); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return nil
}

// Env returns the named environment variable or the fallback.
func Env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}
