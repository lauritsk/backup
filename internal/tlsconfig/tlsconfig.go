// Package tlsconfig builds TLS clients with an optional private CA bundle.
package tlsconfig

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"os"
)

const maxCABytes = 4 << 20

// Client returns a TLS client configuration. Private CA files augment the
// system trust store. InsecureSkipVerify is intended only for explicit tests.
func Client(serverName, caFile string, insecureSkipVerify bool) (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         serverName,
		InsecureSkipVerify: insecureSkipVerify,
	}
	if caFile == "" {
		return cfg, nil
	}
	file, err := os.Open(caFile)
	if err != nil {
		return nil, fmt.Errorf("open CA file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("inspect CA file: %w", err)
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("CA file is not a regular file")
	}
	contents, readErr := io.ReadAll(io.LimitReader(file, maxCABytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read CA file: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close CA file: %w", closeErr)
	}
	if len(contents) > maxCABytes {
		return nil, errors.New("CA file exceeds 4 MiB")
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("load system CA pool: %w", err)
	}
	if !pool.AppendCertsFromPEM(contents) {
		return nil, errors.New("CA file contains no certificates")
	}
	cfg.RootCAs = pool
	return cfg, nil
}
