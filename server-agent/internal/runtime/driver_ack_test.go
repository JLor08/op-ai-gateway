// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestDriverAppliedConfigETagReportsTheGatewayDocument is the happy path:
// a Driver over a *GatewaySource that has applied a pushed document names
// that document, so the gateway can tell "you have my overrides" from "no
// process happens to be running just now".
func TestDriverAppliedConfigETagReportsTheGatewayDocument(t *testing.T) {
	mgr := &fakeManager{}
	src := NewGatewaySource("http://127.0.0.1:1", "tok", nil, "")
	d := newDriver(mgr, src, activeFeaturesClient(t), nil, "")

	d.Sync(context.Background(), []byte(minimalConfigJSON(0, "gw-e1")))

	if got := d.AppliedConfigETag(); got != "gw-e1" {
		t.Fatalf("AppliedConfigETag() = %q, want %q", got, "gw-e1")
	}
}

// TestDriverAppliedConfigETagIsEmptyBeforeAnySync pins the cold-start
// value: a Driver that has negotiated nothing and applied nothing
// acknowledges nothing.
func TestDriverAppliedConfigETagIsEmptyBeforeAnySync(t *testing.T) {
	d := newDriver(&fakeManager{}, NewGatewaySource("http://127.0.0.1:1", "tok", nil, ""), activeFeaturesClient(t), nil, "")
	if got := d.AppliedConfigETag(); got != "" {
		t.Fatalf("AppliedConfigETag() before any Sync = %q, want \"\"", got)
	}
}

// TestDriverAppliedConfigETagIsEmptyInFileMode pins the file-mode decision
// and is the reason the gate is a positive check on *GatewaySource rather
// than a check for a non-empty ETag.
//
// A file-mode agent DISCARDS the gateway's pushed runtime_config document
// outright (Driver.load), so it has nothing of the gateway's to
// acknowledge. Its Config.ETag is whatever the operator's own file happens
// to contain -- and because the gateway's ETag is a deterministic sha256
// over the document's content, an operator who copies a served document
// verbatim into their local file ends up holding a value that would MATCH a
// document the gateway really served. Acknowledging it would tell the
// gateway "I applied your document" about a document this agent provably
// ignored.
//
// The fake is forced to report the file's own etag, so this test fails --
// rather than passing by luck -- if the *GatewaySource check is removed.
func TestDriverAppliedConfigETagIsEmptyInFileMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime-config.json")
	if err := os.WriteFile(path, []byte(minimalConfigJSON(0, "file-e1")), 0o600); err != nil {
		t.Fatalf("write local config: %v", err)
	}

	mgr := &fakeManager{}
	d := newDriver(mgr, NewFileSource(path), activeFeaturesClient(t), nil, "")
	d.Sync(context.Background(), []byte(minimalConfigJSON(0, "gw-e1")))

	if got := mgr.lastApplied().ETag; got != "file-e1" {
		t.Fatalf("applied config ETag = %q, want file-e1 -- this test needs the manager to actually hold a non-empty etag", got)
	}
	if got := d.AppliedConfigETag(); got != "" {
		t.Fatalf("AppliedConfigETag() in file mode = %q, want \"\" -- a file-mode agent has no gateway document to acknowledge", got)
	}
}

// TestDriverAppliedConfigETagIsEmptyWhileInactive pins the same invariant
// ResendReport carries (and for the same reason: the driver owns it, not
// its callers). While runtime_manager is not negotiated on both sides this
// agent enforces nothing, so it must acknowledge nothing -- and the answer
// must not depend on stopAll having incidentally overwritten the manager's
// document with an ETag-less empty one.
//
// The fake is forced to keep reporting a document, so this test fails if
// the Active() gate is removed.
func TestDriverAppliedConfigETagIsEmptyWhileInactive(t *testing.T) {
	mgr := &fakeManager{}
	features, setActive := newToggleableFeaturesServer(t, true)
	src := NewGatewaySource("http://127.0.0.1:1", "tok", nil, "")
	d := newDriver(mgr, src, features, nil, "")

	d.Sync(context.Background(), []byte(minimalConfigJSON(0, "gw-e1")))
	if got := d.AppliedConfigETag(); got != "gw-e1" {
		t.Fatalf("AppliedConfigETag() while active = %q, want gw-e1", got)
	}

	// The gateway stops declaring the feature: the next Sync drains
	// everything and the driver goes inactive.
	setActive(false)
	d.Sync(context.Background(), nil)
	if d.Active() {
		t.Fatal("driver still Active() after the gateway stopped declaring runtime_manager")
	}
	// Pin the gate itself rather than stopAll's side effect: even a manager
	// that still claims to hold the document must not be acknowledged while
	// the feature is off.
	mgr.forceAppliedETag("gw-e1")
	if got := d.AppliedConfigETag(); got != "" {
		t.Fatalf("AppliedConfigETag() while inactive = %q, want \"\" -- an agent enforcing nothing acknowledges nothing", got)
	}
}

// TestDriverAppliedConfigETagComesFromTheManagerNotTheSource pins WHICH of
// this module's three candidate "the ETag" values is reported. The fake
// manager is forced to answer with a document the source never fetched, so
// the test can only pass if the Driver delegates to the RECONCILER.
//
// Both alternatives are real and both are wrong. GatewaySource's own
// tracked etag advances when a document is fetched and parsed, strictly
// before Apply -- reporting it would acknowledge a document that has
// reconciled nothing, which is exactly the lie a gateway waiting on this
// value would then trust. A copy the Driver kept around its own Apply call
// would be honest about ordering but would duplicate state the reconciler
// owns, and drifts the moment anything else applies a config (stopAll
// does).
func TestDriverAppliedConfigETagComesFromTheManagerNotTheSource(t *testing.T) {
	mgr := &fakeManager{}
	src := NewGatewaySource("http://127.0.0.1:1", "tok", nil, "")
	d := newDriver(mgr, src, activeFeaturesClient(t), nil, "")

	d.Sync(context.Background(), []byte(minimalConfigJSON(0, "fetched-e1")))
	mgr.forceAppliedETag("reconciled-e0")

	if got := d.AppliedConfigETag(); got != "reconciled-e0" {
		t.Fatalf("AppliedConfigETag() = %q, want reconciled-e0 -- the report must come from the manager that reconciled, not from the source that fetched", got)
	}
}
