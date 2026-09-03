// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"context"
	"encoding/json"
	"op-ai-server-agent/internal/config"
	"op-ai-server-agent/internal/sample"
	"strings"
	"testing"
	"time"

	runtimectl "op-ai-server-agent/internal/runtime"
)

// bareRuntimeDriver is a runtimeDriver that implements ONLY the required
// trio -- no runtimeConfigAcknowledger, no transitions doorbell, no log
// port. It stands in for a driver that cannot acknowledge anything, which
// is what keeps the optional-interface derivation honest: without it,
// "every driver in this package's tests happens to implement the ack" would
// make the absent case untested.
type bareRuntimeDriver struct{}

func (bareRuntimeDriver) Sync(context.Context, json.RawMessage) {}
func (bareRuntimeDriver) Status() []runtimectl.Status           { return nil }
func (bareRuntimeDriver) Active() bool                          { return true }

// collectOneSample builds exactly one telemetry sample from an Agent wired
// with d and returns it alongside its marshalled bytes (Normalize first, so
// the bytes are the ones the gateway would actually receive).
func collectOneSample(t *testing.T, d Deps) (*sample.Sample, string) {
	t.Helper()
	poster := &capturePoster{}
	d.Poster = poster
	a := NewFromDeps(config.Config{Interval: time.Hour}, d)
	a.collectOnce(context.Background())

	got := poster.first()
	if got == nil {
		t.Fatal("no sample posted")
	}
	got.Normalize()
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return got, string(raw)
}

// TestCollectOnceReportsTheAppliedRuntimeConfigETag is the wire contract
// this half of the feature exists for: an active driver that has applied a
// gateway document puts its ETag on the sample as the top-level
// runtime_config_applied_etag, so the gateway can wait for "you applied MY
// document" instead of guessing with a fixed delay.
func TestCollectOnceReportsTheAppliedRuntimeConfigETag(t *testing.T) {
	drv := newFakeRuntimeDriver()
	drv.setActive(true)
	drv.setAppliedETag("sha256-of-the-drain-document")

	got, raw := collectOneSample(t, Deps{RuntimeDriver: drv})

	if got.RuntimeConfigAppliedETag != "sha256-of-the-drain-document" {
		t.Fatalf("RuntimeConfigAppliedETag = %q, want %q", got.RuntimeConfigAppliedETag, "sha256-of-the-drain-document")
	}
	if !strings.Contains(raw, `"runtime_config_applied_etag":"sha256-of-the-drain-document"`) {
		t.Fatalf("marshalled sample is missing the runtime_config_applied_etag key: %s", raw)
	}
}

// TestCollectOnceOmitsTheETagKeyWhenNothingIsAcknowledged pins the
// byte-neutrality half, in every shape that produces "nothing to
// acknowledge". The key must be ABSENT, not present-and-empty: an agent
// that has nothing to say must produce the pre-feature sample shape, and
// the gateway's fallback path keys on the absence.
func TestCollectOnceOmitsTheETagKeyWhenNothingIsAcknowledged(t *testing.T) {
	acking := newFakeRuntimeDriver()
	acking.setActive(true)
	acking.setAppliedETag("") // active, but has applied no gateway document (file mode, or nothing yet)

	inactive := newFakeRuntimeDriver()
	inactive.setAppliedETag("stale-e1") // would answer, but has not negotiated the feature

	cases := []struct {
		name string
		deps Deps
	}{
		{"no runtime driver at all", Deps{}},
		{"a driver that cannot acknowledge", Deps{RuntimeDriver: bareRuntimeDriver{}}},
		{"an active driver with nothing applied", Deps{RuntimeDriver: acking}},
		{"a driver that has not negotiated the feature", Deps{RuntimeDriver: inactive}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, raw := collectOneSample(t, c.deps)
			if got.RuntimeConfigAppliedETag != "" {
				t.Fatalf("RuntimeConfigAppliedETag = %q, want \"\"", got.RuntimeConfigAppliedETag)
			}
			if strings.Contains(raw, "runtime_config_applied_etag") {
				t.Fatalf("marshalled sample carries a runtime_config_applied_etag key with nothing to acknowledge: %s", raw)
			}
		})
	}
}
