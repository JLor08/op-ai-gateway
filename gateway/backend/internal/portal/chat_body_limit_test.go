// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var nginxSizeRE = regexp.MustCompile(`^([0-9]+)([kKmMgG]?)$`)

// nginxBodyLimit is the byte value of one client_max_body_size argument: a
// number with an optional k/m/g suffix (1024-based). It reads "0" as 0 bytes,
// although nginx reads 0 as no limit at all. That is deliberate: the chat
// location must stay bounded, so 0 has to fail the at-least-the-cap check its
// callers make, like any other value below the cap.
func nginxBodyLimit(value string) (int64, error) {
	m := nginxSizeRE.FindStringSubmatch(value)
	if m == nil {
		return 0, fmt.Errorf("client_max_body_size %q is not a size", value)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("client_max_body_size %q: %w", value, err)
	}
	switch strings.ToLower(m[2]) {
	case "k":
		n <<= 10
	case "m":
		n <<= 20
	case "g":
		n <<= 30
	}
	return n, nil
}

// nginxCommentRE matches one nginx comment: a `#` that starts a token (at the
// start of a line, or after whitespace, `;`, `{` or `}`) up to the end of that
// line. A `#` inside a word is part of the word to nginx. A `#` inside a quoted
// string would be taken for a comment here, and none of the configs this reads
// quote one.
var nginxCommentRE = regexp.MustCompile(`(?m)(^|[ \t;{}])#.*$`)

// chatLocationRE matches the location that covers the chat paths, and captures
// its block body. It requires a `location` at the start of a line whose path is
// exactly /api/portal/chats, followed by the block's `{`: a prefix match on that
// path covers the list and create path itself and every /api/portal/chats/{id}
// below it. `^~` is the same prefix match (it only skips the regex locations),
// so it is accepted too. Refused: an exact match (`=`), which leaves /{id} at
// the default; a trailing slash (`/api/portal/chats/`), whose slash-less list
// and create path nginx answers with a 301; and a longer path such as
// `/api/portal/chats-x`.
var chatLocationRE = regexp.MustCompile(`(?m)^[ \t]*location\s+(?:\^~\s+)?/api/portal/chats\s*\{([^}]*)\}`)

// chatBodyLimitRE reads the client_max_body_size directive from a block body,
// anchored to the start of a statement (the start of the body, or after a
// `;`), so a longer name that merely ends in the directive's does not count.
var chatBodyLimitRE = regexp.MustCompile(`(?:\A|;)\s*client_max_body_size\s+([^;\s]+)\s*;`)

// chatLocationLimit finds the `location /api/portal/chats` block in an nginx
// config, with its comments stripped, and returns the block's
// client_max_body_size in bytes. It refuses a config where the location or the
// directive is missing: without them nginx's 1 MiB default refuses every chat
// document over 1 MiB with an HTML 413.
func chatLocationLimit(config string) (int64, error) {
	config = nginxCommentRE.ReplaceAllString(config, "${1}")
	loc := chatLocationRE.FindStringSubmatch(config)
	if loc == nil {
		return 0, errors.New("no `location /api/portal/chats` block")
	}
	m := chatBodyLimitRE.FindStringSubmatch(loc[1])
	if m == nil {
		return 0, errors.New("`location /api/portal/chats` sets no client_max_body_size")
	}
	return nginxBodyLimit(m[1])
}

// checkChatBodyLimit refuses a config that does not let every chat body the
// gateway accepts (MaxChatRequestBytes) through on /api/portal/chats.
func checkChatBodyLimit(config string) error {
	got, err := chatLocationLimit(config)
	if err != nil {
		return err
	}
	if got < MaxChatRequestBytes {
		return fmt.Errorf("client_max_body_size on /api/portal/chats = %d bytes, want at least MaxChatRequestBytes = %d", got, MaxChatRequestBytes)
	}
	return nil
}

// The check reads the config the way nginx does, not as text: every refused
// config below contains the location and the directive as words, and none of
// them lets a chat body up to the cap through on every chat path.
func TestChatBodyLimitCheckReadsTheConfigAsNginxDoes(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		ok           bool
	}{
		{
			name: "a prefix location at the cap",
			config: "location /api/ { proxy_pass $backend; }\n" +
				"location /api/portal/chats { client_max_body_size 5m; proxy_pass $backend; }\n",
			ok: true,
		},
		{
			name: "a multi-line ^~ location with trailing comments",
			config: "    location ^~ /api/portal/chats {  # the chat documents\n" +
				"        client_max_body_size 5M;  # the chat request cap\n" +
				"        proxy_pass $backend;\n" +
				"    }\n",
			ok: true,
		},
		{
			name:   "a commented-out location",
			config: "# location /api/portal/chats { client_max_body_size 5m; proxy_pass $backend; }\n",
		},
		{
			name:   "a trailing-slash location, which leaves the list and create path out",
			config: "location /api/portal/chats/ { client_max_body_size 5m; proxy_pass $backend; }\n",
		},
		{
			name: "a block whose directive is commented out",
			config: "location /api/portal/chats {\n" +
				"    # client_max_body_size 5m;\n" +
				"    proxy_pass $backend;\n" +
				"}\n",
		},
		{
			name: "a directive that sits only in a comment, after a semicolon",
			config: "location /api/portal/chats {\n" +
				"    proxy_pass $backend;  # off; client_max_body_size 5m;\n" +
				"}\n",
		},
		{
			name:   "a longer path",
			config: "location /api/portal/chats-x { client_max_body_size 5m; proxy_pass $backend; }\n",
		},
		{
			name:   "an exact-match location, which leaves /{id} out",
			config: "location = /api/portal/chats { client_max_body_size 5m; proxy_pass $backend; }\n",
		},
		{
			name:   "the location named only inside a quoted string",
			config: `location /api/ { add_header X-Note "location /api/portal/chats { client_max_body_size 5m; }"; proxy_pass $backend; }` + "\n",
		},
		{
			name:   "a directive whose name only ends in client_max_body_size",
			config: "location /api/portal/chats { proxy_pass $backend; x_client_max_body_size 5m; }\n",
		},
		{
			name:   "a limit below the cap",
			config: "location /api/portal/chats { client_max_body_size 1m; proxy_pass $backend; }\n",
		},
		{
			name:   "no limit at all, which leaves the chat location unbounded",
			config: "location /api/portal/chats { client_max_body_size 0; proxy_pass $backend; }\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkChatBodyLimit(tc.config)
			if tc.ok && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("accepted, want refused")
			}
		})
	}
}

// The bundled edge must let through every chat body the gateway accepts
// (MaxChatRequestBytes), or a document the backend would store is refused at
// the edge with nginx's HTML 413. The number lives once, here in Go; this test
// reads both deploy configs by their repo paths.
func TestNginxChatBodyLimitCoversTheChatRequestCap(t *testing.T) {
	for _, path := range []string{
		"../../../deploy/nginx/locations.conf",
		"../../../deploy/k8s/nginx-configmap.yaml",
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if err := checkChatBodyLimit(string(raw)); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
}

// The outer-proxy config the portal generates must allow the same.
func TestEdgeProxyConfigCarriesTheChatBodyLimit(t *testing.T) {
	svc, ctx := certEnv(t)
	enableSelfSigned(t, svc, ctx, "all", 30)
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.lan")

	cfg, err := svc.EdgeProxyConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkChatBodyLimit(cfg); err != nil {
		t.Fatalf("EdgeProxyConfig: %v", err)
	}
}
