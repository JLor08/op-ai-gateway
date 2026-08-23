// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package certissue

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// SplitNames normalizes a SAN list and separates DNS names from IP addresses.
// An entry that parses as an IP becomes an IP SAN, everything else a DNS SAN --
// this is what lets ONE certificate serve a connection made by name AND one made
// by bare IP (an IP connection sends no SNI, so a server can only ever present a
// single certificate for it). Order is preserved so the FIRST name stays the
// certificate's identity (its Subject CN and the store's primary key).
func SplitNames(names []string) ([]string, []net.IP, error) {
	var (
		dns  []string
		ips  []net.IP
		seen = map[string]bool{}
	)
	for _, raw := range names {
		n := strings.ToLower(strings.TrimSpace(raw))
		if n == "" {
			continue
		}
		if seen[n] {
			return nil, nil, fmt.Errorf("certissue: duplicate name %q", n)
		}
		seen[n] = true
		if ip := net.ParseIP(n); ip != nil {
			ips = append(ips, ip)
			continue
		}
		dns = append(dns, n)
	}
	if len(dns) == 0 && len(ips) == 0 {
		return nil, nil, errors.New("certissue: no names")
	}
	return dns, ips, nil
}

// primaryName is the certificate's identity: the first DNS name, or the first IP
// when the list is IP-only. Used for the Subject CN.
func primaryName(dns []string, ips []net.IP) string {
	if len(dns) > 0 {
		return dns[0]
	}
	return ips[0].String()
}
