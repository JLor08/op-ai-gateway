// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

// defaultDecisionsBodyBytes is the default bound on the JSON body of
// POST /v1/systemone (and, later, /v1/decisions), an operator setting
// (ServerDeps.MaxDecisionsBodyBytes, config
// OP_AI_GATEWAY_MAX_DECISIONS_BODY_BYTES). It equals the agent router's JSON buffer.
const defaultDecisionsBodyBytes = 32 << 20

// decisionsBodyLimit is the JSON read cap in force: the configured one, or the
// default for a Server that was never given one (a bare &Server{}).
func (s *Server) decisionsBodyLimit() int64 {
	if s.maxDecisionsBodyBytes > 0 {
		return s.maxDecisionsBodyBytes
	}
	return defaultDecisionsBodyBytes
}
