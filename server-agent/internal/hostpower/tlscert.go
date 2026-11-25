// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math/big"
	"op-ai-server-agent/internal/collector"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

const (
	// DefaultTLSCertPath is the host certificate for the -tls listener. It
	// lives outside the out directory, which is the guests' virtiofs share.
	DefaultTLSCertPath = "/var/lib/op-hostpower-tls/tls.crt"
	// DefaultTLSKeyPath is the private key belonging to DefaultTLSCertPath.
	DefaultTLSKeyPath = "/var/lib/op-hostpower-tls/tls.key"

	tlsCertCN       = "op-hostpower"
	tlsCertLifetime = 100 // years
	maxTLSFileBytes = 1 << 20

	readOnlyHint = "read-only file system: add StateDirectory=op-hostpower-tls and ReadWritePaths=/var/lib/op-hostpower-tls to the unit (see the agent README)"
)

// writeFileAtomic writes a generated certificate file. It is a variable so
// tests can inject write failures (such as EROFS) without a read-only mount.
var writeFileAtomic = writePrivateFileAtomic

// LoadOrCreateCert returns the host's TLS certificate. When both files exist
// they are loaded as a pair (a key/certificate mismatch is an error, a key file
// readable by group or others is only warned about). When neither exists a
// self-signed ECDSA P-256 certificate is generated and written (key first) with
// mode 0600 into 0700 parent directories; created reports this case. Exactly
// one existing file, or a path that is not a regular file, is an error and
// nothing is written. A nil log means slog.Default().
func LoadOrCreateCert(certPath, keyPath string, now func() time.Time, log *slog.Logger) (cert tls.Certificate, created bool, err error) {
	if log == nil {
		log = slog.Default()
	}
	certInfo, err := lstatRegular(certPath)
	if err != nil {
		return tls.Certificate{}, false, fmt.Errorf("hostpower: tls certificate: %w", err)
	}
	keyInfo, err := lstatRegular(keyPath)
	if err != nil {
		return tls.Certificate{}, false, fmt.Errorf("hostpower: tls key: %w", err)
	}
	switch {
	case certInfo != nil && keyInfo != nil:
		if runtime.GOOS != "windows" && keyInfo.Mode().Perm()&0o077 != 0 {
			log.Warn("hostpower: tls key file is readable by group or others",
				"path", keyPath, "mode", fmt.Sprintf("%04o", keyInfo.Mode().Perm()))
		}
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return tls.Certificate{}, false, fmt.Errorf("hostpower: load tls key pair %s, %s: %w", certPath, keyPath, err)
		}
		return cert, false, nil
	case certInfo != nil:
		return tls.Certificate{}, false, fmt.Errorf("hostpower: tls certificate %s exists but its key %s is missing", certPath, keyPath)
	case keyInfo != nil:
		return tls.Certificate{}, false, fmt.Errorf("hostpower: tls key %s exists but its certificate %s is missing", keyPath, certPath)
	}
	cert, err = generateCert(certPath, keyPath, now())
	if err != nil {
		if errors.Is(err, syscall.EROFS) {
			err = fmt.Errorf("%w; %s", err, readOnlyHint)
		}
		return tls.Certificate{}, false, fmt.Errorf("hostpower: generate tls certificate: %w", err)
	}
	return cert, true, nil
}

// lstatRegular returns path's FileInfo, nil for a missing path, or an error
// for a path that exists but is not a regular file (a symlink included).
func lstatRegular(path string) (fs.FileInfo, error) {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, err
	case !fi.Mode().IsRegular():
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return fi, nil
}

// generateCert creates a self-signed server certificate valid from an hour
// before now for tlsCertLifetime years and writes the key, then the
// certificate. A crash between the two writes leaves "key only", which the
// next start reports instead of overwriting.
func generateCert(certPath, keyPath string, now time.Time) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: tlsCertCN},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(tlsCertLifetime, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	for _, dir := range []string{filepath.Dir(keyPath), filepath.Dir(certPath)} {
		if err := ensurePrivateDir(dir); err != nil {
			return tls.Certificate{}, err
		}
	}
	if err := writeFileAtomic(keyPath, keyPEM); err != nil {
		return tls.Certificate{}, err
	}
	if err := writeFileAtomic(certPath, certPEM); err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

// readCertDER returns the DER of the first CERTIFICATE block in the PEM file
// at path (the block tls.LoadX509KeyPair serves as the leaf).
func readCertDER(path string) ([]byte, error) {
	data, ok, err := readCapped(path, maxTLSFileBytes)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%s: file too large", path)
	}
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("%s: no CERTIFICATE PEM block", path)
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return block.Bytes, nil
	}
}

const tlsUsage = "usage: server-agent hostpower tls [-tls-cert PATH] fingerprint\n"

// TLSMain runs the operator-side tls CLI and returns the exit code: 2 for
// usage errors, 1 when the certificate cannot be read or parsed. It never
// generates a certificate, so it is safe next to a running service.
func TLSMain(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("tls", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("tls-cert", DefaultTLSCertPath, "certificate (PEM)")
	if err := flags.Parse(args); err != nil {
		_, _ = io.WriteString(stderr, tlsUsage)
		return 2
	}
	usage := func(msg string) int {
		_, _ = fmt.Fprintf(stderr, "hostpower tls: %s\n%s", msg, tlsUsage)
		return 2
	}
	rest := flags.Args()
	switch {
	case len(rest) == 0:
		return usage("missing verb")
	case rest[0] != "fingerprint":
		return usage(fmt.Sprintf("unknown verb %q", rest[0]))
	case len(rest) > 1:
		return usage("fingerprint takes no arguments")
	}
	der, err := readCertDER(*path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hostpower tls fingerprint: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, collector.FormatHostpowerFingerprint(der))
	return 0
}
