// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package helper is the root-only half of the agent's privilege split: a
// small process that serves RAPL counters and DIMM data over a Unix socket
// that only the configured unprivileged agent uid may use.
package helper

import (
	"errors"
	"fmt"
	"math"
	"os/user"
	"strconv"
)

// Identity is the agent account the helper serves. UID is the only peer uid
// allowed to connect; GID owns the socket directory and the socket and is
// never used for authorization.
type Identity struct {
	UID, GID int
}

// nobodyUID is the overflow uid: shared by unrelated daemons and what peers
// from an unmapped user namespace appear as, so it can never identify the
// agent.
const nobodyUID = 65534

var errRootAgent = errors.New("helper: -agent-user must not be root")

// ResolveIdentity turns the -agent-user / -agent-group flag values into an
// Identity. userSpec is a user name or a decimal uid; a numeric uid with no
// passwd entry is accepted as-is. groupSpec is a group name or a decimal gid
// (likewise accepted without a group entry); empty means the user's primary
// group, which requires a passwd entry. Numeric ids must be canonical
// decimal ("1000", not "01000"). A user resolving to uid 0 or to the
// overflow uid 65534 (nobody) is refused.
func ResolveIdentity(userSpec, groupSpec string) (Identity, error) {
	uid, primary, hasEntry, err := resolveUser(userSpec)
	if err != nil {
		return Identity{}, err
	}
	switch {
	case uid == 0:
		return Identity{}, errRootAgent
	case uid == nobodyUID:
		return Identity{}, errors.New("helper: -agent-user must not be nobody (uid 65534)")
	case uid < 0 || int64(uid) >= math.MaxUint32:
		return Identity{}, fmt.Errorf("helper: -agent-user %q has invalid uid %d", userSpec, uid)
	}
	id := Identity{UID: uid}
	switch {
	case groupSpec != "":
		if id.GID, err = resolveGroup(groupSpec); err != nil {
			return Identity{}, err
		}
	case hasEntry:
		id.GID = primary
	default:
		return Identity{}, fmt.Errorf("helper: uid %d has no passwd entry, so its primary group is unknown; set -agent-group", uid)
	}
	if id.GID < 0 || int64(id.GID) >= math.MaxUint32 {
		return Identity{}, fmt.Errorf("helper: invalid agent gid %d", id.GID)
	}
	return id, nil
}

// resolveUser returns the uid and, when the uid has a passwd entry
// (hasEntry), its primary gid.
func resolveUser(spec string) (uid, primary int, hasEntry bool, err error) {
	if spec == "" {
		return 0, 0, false, errors.New("helper: -agent-user is required")
	}
	u, lerr := user.Lookup(spec)
	if lerr != nil {
		n, ok := parseID(spec)
		if !ok {
			return 0, 0, false, fmt.Errorf("helper: unknown -agent-user %q: %w", spec, lerr)
		}
		if u, lerr = user.LookupId(spec); lerr != nil {
			return n, 0, false, nil // numeric uid without a passwd entry
		}
	}
	if uid, err = strconv.Atoi(u.Uid); err != nil {
		return 0, 0, false, fmt.Errorf("helper: user %q has non-numeric uid %q", spec, u.Uid)
	}
	if primary, err = strconv.Atoi(u.Gid); err != nil {
		return 0, 0, false, fmt.Errorf("helper: user %q has non-numeric gid %q", spec, u.Gid)
	}
	return uid, primary, true, nil
}

func resolveGroup(spec string) (int, error) {
	g, lerr := user.LookupGroup(spec)
	if lerr != nil {
		n, ok := parseID(spec)
		if !ok {
			return 0, fmt.Errorf("helper: unknown -agent-group %q: %w", spec, lerr)
		}
		return n, nil // numeric gid, with or without a group entry
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return 0, fmt.Errorf("helper: group %q has non-numeric gid %q", spec, g.Gid)
	}
	return gid, nil
}

// parseID accepts a canonical non-negative decimal id that fits a
// uid_t/gid_t, excluding (uid_t)-1, which chown reads as "leave unchanged".
func parseID(s string) (int, bool) {
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil || n == math.MaxUint32 || strconv.FormatUint(n, 10) != s {
		return 0, false
	}
	return int(n), true
}

// authorizePeer is the single place that decides whether a connected peer may
// use the helper: its uid must equal the agent uid. Root peers are not
// implicitly allowed, and the gid plays no part.
func authorizePeer(peerUID int, want Identity) (ok bool, reason string) {
	if peerUID != want.UID {
		return false, "uid mismatch"
	}
	return true, ""
}
