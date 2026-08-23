// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

// meshGateObservationWindow is how fresh a TLS observation must be to ARM the
// mesh plaintext-refusal switch. It mirrors edgeSchemeObservationWindow, but ONLY
// gates arming -- never enforcement (see meshGateRefuses' doc).
const meshGateObservationWindow = 5 * time.Minute

// meshSwitchTTL bounds how long the gate reuses a cached read of
// cert_mesh_require_tls so it does not hit the settings store on every mesh
// request. Mirrors edgeSchemeSwitchTTL.
const meshSwitchTTL = 5 * time.Second

// meshGateWarnInterval throttles the refusal log line to at most one per PATH per
// interval, so a retrying locked-out agent cannot evict the log ring the operator
// needs to read.
const meshGateWarnInterval = time.Minute

// errMeshTLSNotObserved is the arming precondition's refusal: no server has been
// observed authenticating over TLS on the mesh listener within
// meshGateObservationWindow, so arming would risk locking the fleet out on no
// evidence at all.
var errMeshTLSNotObserved = errors.New("no server has authenticated over TLS on the mesh listener recently")

// meshRequireTLSOn reports the cert_mesh_require_tls switch through a short TTL
// cache (via settingCache -- ttlcache.go). Nil-safe -> false, and
// CertMeshRequireTLSChecked already reports false on any store error, so every
// failure mode leaves the gate DISENGAGED. Mirrors edgeRequireHTTPSOn, including
// the generation guard that stops an in-flight pre-disarm read from re-arming the
// cache behind a disarming PUT.
func (s *Server) meshRequireTLSOn(ctx context.Context) bool {
	if s.Portal == nil {
		return false
	}
	return s.meshSwitch.Get(ctx, meshSwitchTTL, s.Portal.CertMeshRequireTLSChecked)
}

// invalidateMeshRequireTLSCache drops the cached switch so the next mesh request
// re-reads it. Called by handleSystemSettings after a PUT that carried
// cert_mesh_require_tls, so a toggle takes effect without waiting out meshSwitchTTL
// -- worst in the DISARMING direction, where the operator is trying to get the
// fleet back in.
func (s *Server) invalidateMeshRequireTLSCache() {
	s.meshSwitch.Invalidate()
}

// meshGateRefuses decides whether to refuse ONE request on the mesh agent
// listener. It is called only from serveWith's public=false branch, so it never
// touches the public mux. It refuses when ALL of these hold:
//
//	(a) the emergency kill switch is NOT set,
//	(b) the request arrived UNENCRYPTED (r.TLS == nil is the only truth),
//	(c) the path is an agent API path (/api/agent/v1/*), and
//	(d) the operator armed the switch (cert_mesh_require_tls).
//
// Crucially, and UNLIKE the edge gate (Plan B), enforcement is NOT coupled to a
// fresh-observation window: once armed the gate refuses plaintext unconditionally
// until the operator disarms it (portal toggle or kill switch). A mesh gate that
// re-opened plaintext the moment TLS observation lapsed would open exactly when
// someone forces TLS to fail. The 5-minute window applies only to ARMING
// (ArmMeshRequireTLS). The accepted cost -- an armed gate + an expired gateway leaf
// can lock the fleet out, recoverable only via the operator -- is documented in
// spec §6/§13. /healthz never reaches here (serveWith serves it first).
func (s *Server) meshGateRefuses(r *http.Request) bool {
	if s.certMeshRequireTLSDisable {
		return false
	}
	// The mesh gate is part of the transport subsystem: without the registry it can
	// never have been armed (ArmMeshRequireTLS needs AnyTLSWithin), so treat a nil
	// registry as "gate inert". New() always installs one, so this is only ever true
	// for a bare &Server{} in a test that never exercises the gate -- and, like the
	// edge gate's nil-safe observation short-circuit, it keeps this shared agent-path
	// check from reaching s.Portal on such a Server (whose fake portal need not
	// implement CertMeshRequireTLSChecked).
	if s.AgentTransport == nil {
		return false
	}
	if r.TLS != nil {
		return false
	}
	if !strings.HasPrefix(r.URL.Path, "/api/agent/v1/") {
		return false
	}
	return s.meshRequireTLSOn(r.Context())
}

// shouldLogMeshGateRefusal throttles the refusal Warn to one per path per
// meshGateWarnInterval (via warnThrottle -- ttlcache.go; mirrors
// shouldLogEdgeGateRefusal). Keyed by path, not remote, so a retrying agent
// cannot flood the log ring.
func (s *Server) shouldLogMeshGateRefusal(path string, now time.Time) bool {
	return s.meshWarn.ShouldLog(path, now, meshGateWarnInterval)
}

// ArmMeshRequireTLS is the precondition for switching cert_mesh_require_tls ON:
// at least one server must have authenticated over TLS on the mesh listener within
// meshGateObservationWindow, so an operator cannot arm a fleet-wide lockout before
// any agent has ever proven it can speak TLS. Nil-safe: a bare &Server{} (no
// registry) fails safe and refuses to arm. There is deliberately NO own-hop check
// (the edge gate's errEdgeArmHopPlaintext): the operator always arms from the
// portal over the public/edge path, never over the mesh listener itself.
func (s *Server) ArmMeshRequireTLS() error {
	if !s.AgentTransport.AnyTLSWithin(time.Now(), meshGateObservationWindow) {
		return errMeshTLSNotObserved
	}
	return nil
}
