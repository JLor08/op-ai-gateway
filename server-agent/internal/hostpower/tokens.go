// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultTokensPath is the host-owned per-VM token store.
	DefaultTokensPath = "/etc/op-hostpower/tokens.json"

	tokenPrefix = "ophp_"

	tokenReloadInterval = time.Second
	maxTokenStoreBytes  = 1 << 20
)

var tokenVMIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)

// tokenRecord is one VM's entry in the store file. Only the SHA-256 of the
// token is kept; the plaintext is printed once at creation.
type tokenRecord struct {
	SHA256  string `json:"sha256"`
	Created string `json:"created"`
}

// TokenInfo describes a stored token without any secret material.
type TokenInfo struct {
	VMID    string
	Created time.Time
}

func validVMID(vmid string) error {
	if !tokenVMIDPattern.MatchString(vmid) {
		return fmt.Errorf("invalid vmid %q: want a decimal VM ID", vmid)
	}
	return nil
}

func tokenDigest(token string) [sha256.Size]byte {
	return sha256.Sum256([]byte(token))
}

func readTokenFile(path string) (map[string]tokenRecord, error) {
	data, ok, err := readCapped(path, maxTokenStoreBytes)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("token store too large")
	}
	recs := map[string]tokenRecord{}
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if recs == nil {
		recs = map[string]tokenRecord{}
	}
	return recs, nil
}

// loadForWrite reads the store, treating a missing file as empty. An
// unparsable file is an error so a write never clobbers it.
func loadForWrite(path string) (map[string]tokenRecord, error) {
	recs, err := readTokenFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]tokenRecord{}, nil
	}
	return recs, err
}

func writeTokenFile(path string, recs map[string]tokenRecord) error {
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFileAtomic(path, append(data, '\n'))
}

// ensurePrivateDir creates dir (and its parents) with mode 0700, or tightens
// an existing dir that grants group or other permissions to 0700.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	di, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if di.Mode().Perm()&^0o700 != 0 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

// writePrivateFileAtomic replaces path with data through a temp file in the
// same directory that is synced and chmod'ed to 0600 before the rename, so
// readers see either the old or the new content and never a group/other-
// readable file; the directory is synced after the rename.
func writePrivateFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(name, path); err != nil {
		cleanup()
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// syncDir flushes dir's entries so a completed rename survives a crash. It is
// best-effort: some platforms (Windows) cannot sync a directory, and the file
// content itself was already synced.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// CreateToken creates or replaces vmid's token and returns the plaintext,
// which is not stored anywhere.
func CreateToken(path, vmid string) (string, error) {
	if err := validVMID(vmid); err != nil {
		return "", err
	}
	recs, err := loadForWrite(path)
	if err != nil {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := tokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	sum := tokenDigest(token)
	recs[vmid] = tokenRecord{
		SHA256:  hex.EncodeToString(sum[:]),
		Created: time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeTokenFile(path, recs); err != nil {
		return "", err
	}
	return token, nil
}

// RevokeToken removes vmid's token; an absent entry is not an error.
func RevokeToken(path, vmid string) error {
	if err := validVMID(vmid); err != nil {
		return err
	}
	recs, err := loadForWrite(path)
	if err != nil {
		return err
	}
	if _, ok := recs[vmid]; !ok {
		return nil
	}
	delete(recs, vmid)
	return writeTokenFile(path, recs)
}

// ListTokens returns the stored VM IDs and creation times, sorted numerically.
func ListTokens(path string) ([]TokenInfo, error) {
	recs, err := loadForWrite(path)
	if err != nil {
		return nil, err
	}
	out := make([]TokenInfo, 0, len(recs))
	for vmid, r := range recs {
		created, err := time.Parse(time.RFC3339, r.Created)
		if err != nil {
			return nil, fmt.Errorf("token entry %s: bad created time: %w", vmid, err)
		}
		out = append(out, TokenInfo{VMID: vmid, Created: created})
	}
	sort.Slice(out, func(i, j int) bool {
		a, aerr := strconv.ParseUint(out[i].VMID, 10, 64)
		b, berr := strconv.ParseUint(out[j].VMID, 10, 64)
		if aerr != nil || berr != nil || a == b {
			return out[i].VMID < out[j].VMID
		}
		return a < b
	})
	return out, nil
}

type tokenEntry struct {
	vmid   string
	digest [sha256.Size]byte
}

// TokenStore resolves bearer tokens to VM IDs, reloading the store file when
// it changes. It is safe for concurrent use.
type TokenStore struct {
	path string
	now  func() time.Time
	log  *slog.Logger

	mu        sync.Mutex
	checked   time.Time
	prev      os.FileInfo
	entries   []tokenEntry
	warned    bool
	everCheck bool
}

// NewTokenStore returns a store backed by the file at path. A missing file is
// an empty set.
func NewTokenStore(path string) *TokenStore {
	return &TokenStore{path: path, now: time.Now}
}

func (s *TokenStore) logger() *slog.Logger {
	if s.log != nil {
		return s.log
	}
	return slog.Default()
}

// Lookup returns the VM a token belongs to. It hashes the token once and
// compares the digest against every stored digest without early exit; a
// digest present for more than one VM matches none.
func (s *TokenStore) Lookup(token string) (string, bool) {
	if !strings.HasPrefix(token, tokenPrefix) {
		return "", false
	}
	want := tokenDigest(token)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()

	matches := 0
	vmid := ""
	for _, e := range s.entries {
		if subtle.ConstantTimeCompare(want[:], e.digest[:]) == 1 {
			matches++
			vmid = e.vmid
		}
	}
	if matches != 1 {
		return "", false
	}
	return vmid, true
}

func (s *TokenStore) refreshLocked() {
	now := s.now()
	if s.everCheck && now.Sub(s.checked) < tokenReloadInterval {
		return
	}
	s.everCheck = true
	s.checked = now

	fi, err := os.Stat(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.entries = nil
			s.prev = nil
			s.warned = false
			return
		}
		s.warnLocked()
		return
	}
	if s.prev != nil && os.SameFile(s.prev, fi) && fi.ModTime().Equal(s.prev.ModTime()) && fi.Size() == s.prev.Size() {
		return
	}
	recs, err := readTokenFile(s.path)
	s.prev = fi
	if err != nil {
		s.warnLocked()
		return
	}
	s.warned = false
	entries := make([]tokenEntry, 0, len(recs))
	for vmid, r := range recs {
		raw, err := hex.DecodeString(r.SHA256)
		if err != nil || len(raw) != sha256.Size {
			continue
		}
		e := tokenEntry{vmid: vmid}
		copy(e.digest[:], raw)
		entries = append(entries, e)
	}
	s.entries = entries
}

func (s *TokenStore) warnLocked() {
	if s.warned {
		return
	}
	s.warned = true
	s.logger().Warn("hostpower: token store unreadable, keeping previous tokens")
}

const tokenUsage = "usage: server-agent hostpower token [-tokens PATH] create|revoke|list [vmid]\n"

// TokenMain runs the operator-side token CLI and returns the exit code: 2 for
// usage errors, 1 for I/O errors.
func TokenMain(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("token", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("tokens", DefaultTokensPath, "token store file")
	if err := flags.Parse(args); err != nil {
		_, _ = io.WriteString(stderr, tokenUsage)
		return 2
	}
	usage := func(msg string) int {
		_, _ = fmt.Fprintf(stderr, "hostpower token: %s\n%s", msg, tokenUsage)
		return 2
	}
	rest := flags.Args()
	if len(rest) == 0 {
		return usage("missing verb")
	}
	verb, vmargs := rest[0], rest[1:]
	if msg := checkTokenArgs(verb, vmargs); msg != "" {
		return usage(msg)
	}

	fail := func(err error) int {
		_, _ = fmt.Fprintf(stderr, "hostpower token %s: %v\n", verb, err)
		return 1
	}
	if err := runTokenVerb(verb, *path, vmargs, stdout); err != nil {
		return fail(err)
	}
	return 0
}

// checkTokenArgs returns the usage message for a bad verb or argument list,
// or "" when the arguments are valid.
func checkTokenArgs(verb string, vmargs []string) string {
	switch verb {
	case "create", "revoke":
		if len(vmargs) != 1 {
			return verb + " needs exactly one vmid"
		}
		if err := validVMID(vmargs[0]); err != nil {
			return err.Error()
		}
	case "list":
		if len(vmargs) != 0 {
			return "list takes no vmid"
		}
	default:
		return "unknown verb " + strconv.Quote(verb)
	}
	return ""
}

// runTokenVerb executes a validated verb against the token store at path.
// vmargs holds the vmid for create and revoke.
func runTokenVerb(verb, path string, vmargs []string, stdout io.Writer) error {
	switch verb {
	case "create":
		tok, err := CreateToken(path, vmargs[0])
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(stdout, tok)
	case "revoke":
		return RevokeToken(path, vmargs[0])
	case "list":
		infos, err := ListTokens(path)
		if err != nil {
			return err
		}
		for _, i := range infos {
			_, _ = fmt.Fprintf(stdout, "%s\t%s\n", i.VMID, i.Created.UTC().Format(time.RFC3339))
		}
	}
	return nil
}
