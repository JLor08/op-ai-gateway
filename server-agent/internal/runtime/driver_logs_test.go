// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"encoding/json"
	"strings"
	"testing"
)

// newLogDriver builds a Driver over a real (idle) Manager, which is what the
// log surface needs: only the concrete *Manager owns a LogStore, so
// driver_test.go's fake-manager constructor deliberately cannot reach it.
func newLogDriver(t *testing.T) *Driver {
	t.Helper()
	m := NewManager(ManagerOptions{Policy: LocalPolicy{}, Getenv: func(string) string { return "" }})
	t.Cleanup(m.Close)
	return NewDriver(m, nil, nil, nil, "")
}

// TestSetLogWatchAppliesTheFullSet: the command states the whole desired set,
// so applying it replaces whatever was watched before.
func TestSetLogWatchAppliesTheFullSet(t *testing.T) {
	d := newLogDriver(t)
	d.SetLogWatch(json.RawMessage(`{"spec_ids":["spec-b","spec-a"]}`))
	if got := d.logs.Watching(); len(got) != 2 || got[0] != "spec-a" || got[1] != "spec-b" {
		t.Fatalf("Watching() = %v, want [spec-a spec-b]", got)
	}
	d.SetLogWatch(json.RawMessage(`{"spec_ids":["spec-c"]}`))
	if got := d.logs.Watching(); len(got) != 1 || got[0] != "spec-c" {
		t.Fatalf("Watching() = %v, want [spec-c] -- a command replaces, never merges", got)
	}
}

// TestSetLogWatchFailsClosed: every unusable input resolves to "watch
// nothing", never to "keep whatever was watched before". Keeping a stale set
// on a malformed command would mean an agent streaming output nobody asked
// for, which is precisely what the on-demand design exists to avoid.
func TestSetLogWatchFailsClosed(t *testing.T) {
	for _, raw := range []string{
		`{"spec_ids":[]}`,
		`{"spec_ids":null}`,
		`{}`,
		`not json at all`,
		``,
	} {
		d := newLogDriver(t)
		d.SetLogWatch(json.RawMessage(`{"spec_ids":["stale"]}`))
		d.SetLogWatch(json.RawMessage(raw))
		if got := d.logs.Watching(); len(got) != 0 {
			t.Errorf("after %q the watch set is %v, want empty", raw, got)
		}
	}
}

// TestSetLogWatchClampsTheSetSize bounds the streaming half's memory the way
// the total buffer setting bounds the retention half: each watched spec can
// hold a live queue, so an absurd command must not be obeyed verbatim.
func TestSetLogWatchClampsTheSetSize(t *testing.T) {
	d := newLogDriver(t)
	ids := make([]string, 0, maxWatchedSpecs+10)
	for i := range maxWatchedSpecs + 10 {
		ids = append(ids, "spec-"+strings.Repeat("x", i%5)+string(rune('a'+i%26))+string(rune('0'+i/26)))
	}
	raw, err := json.Marshal(LogWatchCommand{SpecIDs: ids})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	d.SetLogWatch(raw)
	if got := d.logs.Watching(); len(got) > maxWatchedSpecs {
		t.Fatalf("watching %d specs, want at most %d", len(got), maxWatchedSpecs)
	}
}

// TestDrainLogFramesMarshalsOneFramePerSpec: the driver hands the agent's run
// loop ready-to-send payloads, so that loop -- the single WebSocket writer --
// does the least possible work per frame and internal/agent needs to know
// nothing about the log wire shape.
func TestDrainLogFramesMarshalsOneFramePerSpec(t *testing.T) {
	d := newLogDriver(t)
	if got := d.DrainLogFrames(); got != nil {
		t.Fatalf("DrainLogFrames with nothing watched = %v, want nil", got)
	}

	a := d.logs.newProc("spec-a")
	a.Started(1, ResolvedCommand{})
	a.Write([]byte("from a\n"))
	b := d.logs.newProc("spec-b")
	b.Started(2, ResolvedCommand{})
	b.Write([]byte("from b\n"))

	d.SetLogWatch(json.RawMessage(`{"spec_ids":["spec-a","spec-b"]}`))
	frames := d.DrainLogFrames()
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want one per spec", len(frames))
	}
	bySpec := map[string]LogBatch{}
	for _, raw := range frames {
		var batch LogBatch
		if err := json.Unmarshal(raw, &batch); err != nil {
			t.Fatalf("frame is not a LogBatch: %v (%s)", err, raw)
		}
		bySpec[batch.SpecID] = batch
	}
	if !strings.Contains(batchText(bySpec["spec-a"]), "from a") {
		t.Errorf("spec-a frame = %+v", bySpec["spec-a"])
	}
	if !strings.Contains(batchText(bySpec["spec-b"]), "from b") {
		t.Errorf("spec-b frame = %+v", bySpec["spec-b"])
	}
	for id, batch := range bySpec {
		if !batch.Scrollback {
			t.Errorf("%s: first frame after a subscribe must be the scrollback", id)
		}
	}
}

// TestLogSurfaceIsANoOpWithoutARealManager: driver_test.go's fake manager has
// no log store, and the defensive NewDriver(nil, ...) path has no manager at
// all. Both must be complete no-ops rather than panics -- the same nil
// discipline Status/Transitions already follow.
func TestLogSurfaceIsANoOpWithoutARealManager(t *testing.T) {
	d := NewDriver(nil, nil, nil, nil, "")
	d.SetLogWatch(json.RawMessage(`{"spec_ids":["spec-a"]}`))
	if got := d.DrainLogFrames(); got != nil {
		t.Fatalf("DrainLogFrames on a manager-less driver = %v, want nil", got)
	}
}

// TestDrainLogFramesCarriesTheResolvedCommandOnTheWire asserts the agent->gateway
// wire contract on the MARSHALLED bytes, naming no new field at all.
//
// That is deliberate. Every other test of this feature reads the Go struct, so
// all of them would keep passing if the command were retained but never
// serialized -- an `omitempty` on the wrong field, a batch built without it, a
// frame assembled from a different value. Only the bytes the WebSocket writer
// actually hands over can catch that, and a version of this agent without the
// field produces exactly the frame this test rejects.
func TestDrainLogFramesCarriesTheResolvedCommandOnTheWire(t *testing.T) {
	d := newLogDriver(t)

	p := d.logs.newProc("spec-a")
	p.Started(4711, ResolvedCommand{
		Binary: "/opt/llama/llama-server",
		Args:   []string{"--port", "54331"},
		Env:    []string{"CUDA_VISIBLE_DEVICES=2,3"},
	})
	p.Write([]byte("loading weights\n"))

	d.SetLogWatch(json.RawMessage(`{"spec_ids":["spec-a"]}`))
	frames := d.DrainLogFrames()
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	raw := string(frames[0])
	for _, want := range []string{
		`"event":"started"`,
		`"pid":4711`,
		`"command"`,
		`"binary":"/opt/llama/llama-server"`,
		`"54331"`,
		`"CUDA_VISIBLE_DEVICES=2,3"`,
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("the frame handed to the WebSocket writer does not contain %s -- the resolved command never reaches the gateway.\nframe: %s", want, raw)
		}
	}
}
