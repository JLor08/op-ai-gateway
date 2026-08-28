#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 OnPrem AI Gateway contributors
#
# Docs consistency check. Run from anywhere:
#   ./scripts/check-docs.sh          (or: make lint-docs / make lint)
#
# Four things the repository's conventions require but nothing enforced:
#
#   1. Every intra-repo markdown link resolves — the file, and where the link
#      carries a "#anchor", the anchor too. Anchors are derived from the target
#      document's own headings using GitHub's slug rules (that is how this
#      corpus is rendered), so generated anchors like the ADR log's are checked
#      rather than trusted.
#   2. Every file under docs/architecture/ is reachable from
#      docs/architecture/README.md, directly or transitively. An architecture
#      document that no index links to is invisible; AGENTS.md states the index
#      rule and, before this check, nobody enforced it.
#   3. docs/architecture/*.yaml (the OpenAPI spec) reads as the YAML subset it
#      is written in, and every "$ref" points at a node that exists.
#   4. config-env.md's agent table agrees with the flags server-agent actually
#      registers. The table states that every row has a matching CLI flag
#      except those marked "no flag form"; passing a flag the agent does not
#      register is a *startup error*, so a wrong marking sends an operator to
#      a command that takes their AI server down. This drifted twice on the
#      branch that added it, in both directions, which is why it is a gate.
#
# Deliberately out of scope:
#   - http(s)/mailto links. They fail for reasons that have nothing to do with
#     this repository (network, rate limits, sites that block CI) and would
#     turn a deterministic gate into a flaky one. Use a link checker out of
#     band if you want them audited.
#   - Prose style, spelling, line length. This is a consistency check, not a
#     markdown linter.
#   - docs/superpowers/ and docs/implementation-status.md as link *sources*:
#     AGENTS.md declares them branch-local working documents that never reach
#     main, so gating CI on them would gate it on scratch. Their headings are
#     still collected, so links pointing *into* them are still verified.
#
# Dependencies: bash, git, awk. Nothing else — the same footing as every other
# script in scripts/. Its own negative cases are pinned by
# scripts/check-docs.test.sh.
set -euo pipefail

# Byte semantics, not locale semantics: slugging and the YAML reader below both
# reason about bytes, and tolower() must fold ASCII only.
export LC_ALL=C

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

INDEX="docs/architecture/README.md"
ARCH_PREFIX="docs/architecture/"
# Branch-local working documents (AGENTS.md): scanned for headings, not for
# outgoing links.
EXCLUDE_RE="^docs/(superpowers/|implementation-status[.]md$)"

TMPDIR_RUN="$(mktemp -d -t op-check-docs.XXXXXX)"
trap 'rm -rf "$TMPDIR_RUN"' EXIT INT TERM

# The inventory doubles as the "does this link target exist?" oracle and as the
# list of files the index must reach. --others so a document you just created
# (not yet staged) is checked too; --exclude-standard so .gitignore'd trees
# (node_modules/, .worktrees/, data/) stay out.
INVENTORY="$TMPDIR_RUN/inventory"
git ls-files --cached --others --exclude-standard | LC_ALL=C sort -u >"$INVENTORY"
[ -s "$INVENTORY" ] || { echo "check-docs: no files found under $ROOT — not a git repository?" >&2; exit 2; }

MD_LIST="$TMPDIR_RUN/md"
grep -E '\.md$' "$INVENTORY" >"$MD_LIST" || true
[ -s "$MD_LIST" ] || { echo "check-docs: no markdown files found under $ROOT" >&2; exit 2; }

YAML_LIST="$TMPDIR_RUN/yaml"
grep -E "^${ARCH_PREFIX}.*\.(yaml|yml)$" "$INVENTORY" >"$YAML_LIST" || true

rc=0

# ---------------------------------------------------------------------------
# 1 + 2: markdown links, anchors, and index reachability
# ---------------------------------------------------------------------------
read -r -d '' MARKDOWN_AWK <<'AWK' || true
# First input file is the inventory (one repo-relative path per line); every
# input after it is a markdown file to read. Everything is resolved in END
# because a link may point forward at a heading in a file not read yet.
function fence_open(s,   c, n) {
  # Returns "" when s does not start a fence, else the marker char + its length.
  c = substr(s, 1, 1)
  if (c != "`" && c != "~") return ""
  n = 0
  while (substr(s, n + 1, 1) == c) n++
  if (n < 3) return ""
  return c " " n
}

function strip_links(s,   r, pre, post) {
  # [text](target) -> text, so a heading's rendered text is what gets slugged.
  while (match(s, /\[[^][]*\]\([^()]*\)/)) {
    pre = substr(s, 1, RSTART - 1)
    post = substr(s, RSTART + RLENGTH)
    r = substr(s, RSTART, RLENGTH)
    sub(/\]\([^()]*\)$/, "", r)
    sub(/^\[/, "", r)
    s = pre r post
  }
  return s
}

function slug(text,   s, i, c, out) {
  # GitHub's github-slugger, restricted to what this corpus contains: take the
  # heading's rendered text, lowercase it, drop punctuation, turn each space
  # into a hyphen (runs are NOT collapsed - "A & B" really does slug to
  # "a--b"), and suffix repeats with -1, -2 (handled by the caller).
  s = text
  gsub(/<[^>]*>/, "", s)          # inline HTML contributes no text
  s = strip_links(s)
  gsub(/`/, "", s)                # inline code contributes its content
  # Multi-byte punctuation the slugger drops. Any other non-ASCII byte is kept
  # verbatim (correct for lowercase non-ASCII letters, which this English
  # corpus does not use in headings); a mismatch would surface as a broken
  # anchor, never as a silent pass.
  gsub(/—|–|―|→|←|↔|⇒|⇔|§|·|×|÷|≥|≤|≈|≠|…|•|’|‘|“|”|™|©|®|†|‡|°|±/, "", s)
  s = tolower(s)
  out = ""
  for (i = 1; i <= length(s); i++) {
    c = substr(s, i, 1)
    if (c == " " || c == "\t") { out = out "-"; continue }
    if (index(PUNCT, c) > 0) continue
    out = out c
  }
  return out
}

function dirname(p) {
  if (!sub(/\/[^\/]*$/, "", p)) return ""
  return p
}

function normalize(p,   n, parts, i, top, stack, out) {
  # Collapse "." and ".." lexically. "" means the path escapes the repo root.
  n = split(p, parts, "/")
  top = 0
  for (i = 1; i <= n; i++) {
    if (parts[i] == "" || parts[i] == ".") continue
    if (parts[i] == "..") {
      if (top == 0) return ""
      top--
      continue
    }
    stack[++top] = parts[i]
  }
  out = ""
  for (i = 1; i <= top; i++) out = (out == "" ? stack[i] : out "/" stack[i])
  return out
}

function problem(where, msg) {
  printf "%s: %s\n", where, msg
  problems++
}

BEGIN {
  PUNCT = "!\"#$%&\047()*+,./:;<=>?@[\\]^`{|}~"
  problems = 0
  nlinks = 0
  nanchor = 0
  nfiles = 0
}

# ---- inventory --------------------------------------------------------------
NR == FNR {
  if ($0 == "") next
  exists[$0] = 1
  d = $0
  while ((d = dirname(d)) != "") exists[d] = 1   # directories are link targets too
  if (index($0, ARCH_PREFIX) == 1) { arch[$0] = 1; archlist[++narch] = $0 }
  next
}

# ---- markdown ---------------------------------------------------------------
FNR == 1 {
  file = FILENAME
  nfiles++
  fence = ""
  scan_links = (file ~ EXCLUDE_RE) ? 0 : 1
}

{
  match($0, /^ */)
  indent = RLENGTH
  content = substr($0, indent + 1)

  # Fenced code blocks: no headings, no links. Fences may be indented up to 3.
  if (indent <= 3) {
    marker = fence_open(content)
    if (fence != "") {
      if (marker != "") {
        split(marker, mm, " ")
        split(fence, ff, " ")
        rest = content
        sub(/^[`~]+/, "", rest)
        if (mm[1] == ff[1] && mm[2] + 0 >= ff[2] + 0 && rest ~ /^[ \t]*$/) fence = ""
      }
      next
    }
    if (marker != "") { fence = marker; next }
  }
  if (fence != "") next

  # ATX heading -> anchor.
  if (indent <= 3 && substr(content, 1, 1) == "#") {
    match(content, /^#*/)
    level = RLENGTH
    after = substr(content, level + 1, 1)
    if (level <= 6 && (after == "" || after == " " || after == "\t")) {
      h = substr(content, level + 1)
      sub(/^[ \t]+/, "", h)
      sub(/[ \t]+#+[ \t]*$/, "", h)     # optional closing sequence
      s = slug(h)
      n = ++headcount[file, s]
      anchor[file, (n == 1 ? s : s "-" (n - 1))] = 1
      nanchor++
    }
  }

  if (!scan_links) next

  # Links. Inline code spans first: `[a](b)` is not a link.
  l = content
  gsub(/`[^`]*`/, "", l)
  while (match(l, /\]\([^()]*\)/)) {
    raw = substr(l, RSTART + 2, RLENGTH - 3)
    l = substr(l, RSTART + RLENGTH)
    sub(/^[ \t]+/, "", raw)
    sub(/[ \t]+$/, "", raw)
    if (raw ~ /^</ && raw ~ />$/) { sub(/^</, "", raw); sub(/>$/, "", raw) }
    if (match(raw, /[ \t]+["\047(]/)) raw = substr(raw, 1, RSTART - 1)   # drop the title
    if (raw == "") continue
    if (raw ~ /^[a-zA-Z][a-zA-Z0-9+.-]*:/) continue    # http:, mailto:, ... — out of scope
    if (raw ~ /^\/\//) continue                        # protocol-relative — external
    link[++nlinks] = file SUBSEP FNR SUBSEP raw
  }
}

# ---- resolve ----------------------------------------------------------------
END {
  for (i = 1; i <= nlinks; i++) {
    split(link[i], a, SUBSEP)
    f = a[1]; ln = a[2]; raw = a[3]
    where = f ":" ln

    h = index(raw, "#")
    if (h > 0) { p = substr(raw, 1, h - 1); frag = substr(raw, h + 1) }
    else       { p = raw; frag = "" }

    if (p == "") {
      target = f
    } else if (substr(p, 1, 1) == "/") {
      target = normalize(substr(p, 2))
    } else {
      target = normalize(dirname(f) "/" p)
    }

    if (target == "") {
      problem(where, "link escapes the repository: " raw)
      continue
    }
    if (!(target in exists)) {
      problem(where, "link target does not exist: " raw " -> " target)
      continue
    }

    if (f ~ ARCH_RE && target ~ ARCH_RE) {
      # Index reachability is judged inside docs/architecture/ only: a path
      # that leaves and comes back (via AGENTS.md, say) is not the index
      # reaching the file.
      edge[f, ++outdeg[f]] = target
    }

    if (frag == "") continue
    if (target !~ /\.md$/) continue          # anchors in non-markdown are not modeled
    if (!((target SUBSEP frag) in anchor)) {
      problem(where, "anchor not found in " target ": #" frag)
    }
  }

  printf "  markdown files read: %d, anchors: %d, intra-repo links: %d\n", nfiles, nanchor, nlinks

  # Reachability from the index, breadth-first.
  if (!(INDEX in arch)) {
    problem(INDEX, "the architecture index is missing")
  } else {
    queue[1] = INDEX
    seen[INDEX] = 1
    qh = 1; qt = 1
    while (qh <= qt) {
      cur = queue[qh++]
      for (k = 1; k <= outdeg[cur]; k++) {
        nx = edge[cur, k]
        if (nx in seen) continue
        seen[nx] = 1
        queue[++qt] = nx
      }
    }
    nunreached = 0
    for (i = 1; i <= narch; i++) {          # inventory order, so output is stable
      if (archlist[i] in seen) continue
      problem(archlist[i], "not reachable from " INDEX " — add it to the index (or link it from a document that is indexed)")
      nunreached++
    }
    printf "  docs/architecture files: %d, reachable from the index: %d\n", narch, narch - nunreached
  }

  if (problems > 0) exit 1
}
AWK

echo "==> markdown links, anchors and the docs/architecture index"
# shellcheck disable=SC2086  # MD_LIST holds one path per line, none with spaces
if ! awk -v INDEX="$INDEX" -v ARCH_PREFIX="$ARCH_PREFIX" \
        -v ARCH_RE="^${ARCH_PREFIX}" -v EXCLUDE_RE="$EXCLUDE_RE" \
        "$MARKDOWN_AWK" "$INVENTORY" $(cat "$MD_LIST"); then
  rc=1
fi

# ---------------------------------------------------------------------------
# 3: the OpenAPI document
# ---------------------------------------------------------------------------
read -r -d '' OPENAPI_AWK <<'AWK' || true
# A structural reader for the YAML subset docs/architecture/reference/openapi.yaml
# is written in: block mappings, block sequences, single-line flow collections,
# block scalars, comments. It is NOT a conforming YAML parser — but it fails
# loudly on anything outside that subset instead of skipping it, so "no
# complaints" means the document really was understood end to end. What it
# reports: tabs in indentation, indentation that lands between levels,
# duplicate keys in one mapping, unparsable lines, multi-line flow collections,
# and $ref pointers with no node behind them.
function problem(msg) {
  printf "%s:%d: %s\n", FILENAME, FNR, msg
  problems++
}

function path_of(d,   i, s) {
  s = ""
  for (i = 1; i <= d; i++) s = s "/" stackkey[i]
  return s
}

function unescape_pointer(p) {
  gsub(/~1/, "/", p)
  gsub(/~0/, "~", p)
  return p
}

function flow_balanced(s,   i, c, q, depth) {
  q = ""
  depth = 0
  for (i = 1; i <= length(s); i++) {
    c = substr(s, i, 1)
    if (q != "") { if (c == q) q = ""; continue }
    if (c == "\047" || c == "\"") { q = c; continue }
    if (c == "[" || c == "{") depth++
    else if (c == "]" || c == "}") depth--
  }
  return (q == "" && depth == 0)
}

function collect_refs(s,   rest, tok, c, i, q) {
  rest = s
  while (match(rest, /\$ref:[ \t]*/)) {
    rest = substr(rest, RSTART + RLENGTH)
    tok = ""
    c = substr(rest, 1, 1)
    if (c == "\047" || c == "\"") {
      q = c
      for (i = 2; i <= length(rest); i++) {
        if (substr(rest, i, 1) == q) break
        tok = tok substr(rest, i, 1)
      }
      rest = substr(rest, i + 1)
    } else {
      for (i = 1; i <= length(rest); i++) {
        c = substr(rest, i, 1)
        if (c == "," || c == "}" || c == "]" || c == " " || c == "\t") break
        tok = tok c
      }
      rest = substr(rest, i)
    }
    if (tok == "") { problem("empty $ref"); continue }
    nrefs++
    reffile[nrefs] = FILENAME
    refline[nrefs] = FNR
    reftok[nrefs] = tok
  }
}

BEGIN { problems = 0; nrefs = 0; ndocs = 0 }

FNR == 1 {
  ndocs++
  depth = 0
  skip = -1
  delete stackkey
  delete stackind
  delete seqno
}

{
  match($0, /^[ \t]*/)
  lead = substr($0, 1, RLENGTH)
  ind = RLENGTH
  content = substr($0, ind + 1)
  sub(/[ \t]+$/, "", content)

  if (content == "") next                       # blank line: no state change
  if (skip >= 0) {                              # inside a block scalar or a
    if (ind > skip) next                        # multi-line plain scalar
    skip = -1
  }
  if (index(lead, "\t") > 0) { problem("tab in indentation (YAML forbids it)"); next }
  if (substr(content, 1, 1) == "#") next        # whole-line comment

  # Sequence item: "- " optionally followed by the first key of a compact
  # nested mapping ("- url: http://...").
  if (content == "-" || substr(content, 1, 2) == "- ") {
    check_indent(ind)
    pop_to(ind)
    parent = FILENAME SUBSEP path_of(depth)
    stackkey[depth + 1] = seqno[parent]++
    stackind[depth + 1] = ind
    depth++
    if (content == "-") next
    content = substr(content, 3)
    match(content, /^ */)
    ind = ind + 2 + RLENGTH
    content = substr(content, RLENGTH + 1)
    if (substr(content, 1, 1) == "[" || substr(content, 1, 1) == "{") {
      if (!flow_balanced(content)) problem("multi-line flow collections are not supported by this checker")
      collect_refs($0)
      skip = ind
      next
    }
  } else {
    check_indent(ind)
    pop_to(ind)
  }

  # Mapping key: quoted, or up to the first ":" that ends the line or is
  # followed by a space.
  key = ""
  c = substr(content, 1, 1)
  if (c == "\047" || c == "\"") {
    e = index(substr(content, 2), c)
    if (e == 0) { problem("unterminated quoted key"); next }
    key = substr(content, 2, e - 1)
    rest = substr(content, e + 2)
    if (substr(rest, 1, 1) != ":") { problem("expected \":\" after the quoted key"); next }
    rest = substr(rest, 2)
  } else {
    for (i = 1; i <= length(content); i++) {
      if (substr(content, i, 1) != ":") continue
      nc = substr(content, i + 1, 1)
      if (nc == "" || nc == " ") { key = substr(content, 1, i - 1); rest = substr(content, i + 1); break }
    }
    if (key == "") { problem("cannot parse: neither a mapping key nor a sequence item: " content); next }
  }

  parent = path_of(depth)
  if ((FILENAME SUBSEP parent SUBSEP key) in seenkey) {
    problem("duplicate key \"" key "\" in " (parent == "" ? "the document root" : parent))
  }
  seenkey[FILENAME, parent, key] = 1

  stackkey[depth + 1] = key
  stackind[depth + 1] = ind
  depth++
  haspath[FILENAME, path_of(depth)] = 1

  collect_refs($0)

  sub(/^[ \t]+/, "", rest)
  if (rest == "") next                                  # a block follows
  if (substr(rest, 1, 1) == "|" || substr(rest, 1, 1) == ">") {
    if (rest !~ /^[|>][-+0-9]*([ \t]+#.*)?$/) problem("unsupported block scalar header: " rest)
    skip = ind
    next
  }
  if ((substr(rest, 1, 1) == "[" || substr(rest, 1, 1) == "{") && !flow_balanced(rest)) {
    problem("multi-line flow collections are not supported by this checker")
    next
  }
  skip = ind                                            # scalar; continuations skipped
}

function check_indent(ind,   d) {
  # Valid: deeper than the current level, or exactly equal to a level already
  # on the stack. Anything else lands between levels — a YAML error that a
  # lenient reader would silently reinterpret. Reported, then the line is
  # parsed anyway against the nearest shallower level, so one bad indent does
  # not cascade into a page of follow-on complaints.
  if (depth == 0) {
    if (ind == 0) return
  } else {
    if (ind > stackind[depth]) return
    for (d = depth; d >= 1; d--) if (stackind[d] == ind) return
  }
  problem("indentation lands between levels (" ind " spaces)")
}

function pop_to(ind) {
  while (depth > 0 && stackind[depth] >= ind) depth--
}

END {
  for (i = 1; i <= nrefs; i++) {
    t = reftok[i]
    if (substr(t, 1, 2) != "#/") {
      printf "%s:%d: $ref is not a local pointer into this document: %s\n", reffile[i], refline[i], t
      problems++
      continue
    }
    p = unescape_pointer(substr(t, 2))
    if (!((reffile[i] SUBSEP p) in haspath)) {
      printf "%s:%d: dangling $ref: %s\n", reffile[i], refline[i], t
      problems++
    }
  }
  printf "  yaml documents read: %d, $refs: %d\n", ndocs, nrefs
  if (problems > 0) exit 1
}
AWK

echo "==> openapi \$refs"
if [ -s "$YAML_LIST" ]; then
  # shellcheck disable=SC2086  # one path per line, none with spaces
  if ! awk "$OPENAPI_AWK" $(cat "$YAML_LIST"); then
    rc=1
  fi
else
  echo "  no yaml documents under $ARCH_PREFIX"
fi

# ---------------------------------------------------------------------------
# 4: config-env.md's agent table vs. the flags server-agent registers
# ---------------------------------------------------------------------------
# Both directions matter. A row that claims a flag the agent does not register
# sends an operator to `server-agent -runtime-log-buffer-bytes=...`, which
# flag.ContinueOnError turns into a startup error that takes every managed
# model on that host down. A row marked "no flag form" for a flag that DOES
# exist hides a supported way to configure the agent. Neither is visible by
# reading either file alone.
CONFIG_ENV_DOC="docs/architecture/reference/config-env.md"
AGENT_CONFIG_GO="server-agent/internal/config/config.go"

echo "==> agent config: config-env.md vs. registered flags"
if [ ! -f "$CONFIG_ENV_DOC" ] || [ ! -f "$AGENT_CONFIG_GO" ]; then
  echo "  skipped: $CONFIG_ENV_DOC or $AGENT_CONFIG_GO is absent"
else
  read -r -d '' AGENTFLAG_AWK <<'AWK' || true
# Input 1: the registered flag names, one per line, from the Go flag set.
# Input 2: config-env.md. NR==FNR keys the first file into `have`.
NR == FNR { if (!($0 in have)) { have[$0] = 1; nflags++ }; next }

/^## Agent \(`OP_AGENT_\*`\)/ { inagent = 1; next }
inagent && /^## /             { inagent = 0 }

inagent && /^\| `OP_AGENT_/ {
  if (!match($0, /`OP_AGENT_[A-Z0-9_]+`/)) next
  var = substr($0, RSTART + 1, RLENGTH - 2)
  # Column 3 of a "| a | b | c |" row is the Type cell (split yields an empty
  # field 1 for the leading pipe).
  split($0, cell, "|")
  marked = (index(cell[3], "no flag form") > 0)
  # OP_AGENT_X_Y -> x-y. "OP_AGENT_" is 9 bytes, so the tail starts at 10.
  flagname = tolower(substr(var, 10))
  gsub(/_/, "-", flagname)
  rows++
  if (marked && (flagname in have)) {
    printf "%s:%d: %s is marked \"no flag form\" but -%s is registered in %s\n", \
           FILENAME, FNR, var, flagname, GOFILE
    problems++
  } else if (!marked && !(flagname in have)) {
    printf "%s:%d: the table claims -%s for %s; %s registers no such flag, and passing it is a startup error. Mark the row \"no flag form\".\n", \
           FILENAME, FNR, flagname, var, GOFILE
    problems++
  }
}

END {
  if (rows == 0) {
    print "no OP_AGENT_* rows found -- has the agent table moved or been renamed?"
    problems++
  }
  printf "  agent settings checked: %d, registered flags: %d\n", rows, nflags
  if (problems > 0) exit 1
}
AWK

  # One flag name per line. The registration block is uniform:
  #   name := fs.String("flag-name", ...)  /  fs.Bool("flag-name", ...)
  AGENT_FLAGS="$(sed -n 's/.*fs\.\(String\|Bool\|Int\|Int64\|Duration\)("\([a-z0-9-]*\)".*/\2/p' "$AGENT_CONFIG_GO" | sort -u)"
  if [ -z "$AGENT_FLAGS" ]; then
    echo "  $AGENT_CONFIG_GO: no fs.String/fs.Bool registrations found -- the extractor no longer matches the source" >&2
    rc=1
  elif ! printf '%s\n' "$AGENT_FLAGS" \
       | awk -v GOFILE="$AGENT_CONFIG_GO" "$AGENTFLAG_AWK" - "$CONFIG_ENV_DOC"; then
    rc=1
  fi
fi

if [ "$rc" -ne 0 ]; then
  echo "check-docs: FAILED" >&2
  exit 1
fi
echo "check-docs: OK"
