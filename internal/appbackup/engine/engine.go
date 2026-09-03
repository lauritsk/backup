// Package engine performs optional Docker and Podman diagnostics.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/lauritsk/backup/internal/appbackup/config"
)

type Runner struct{}

func (Runner) Check(ctx context.Context, engine config.EngineConfig) error {
	if !filepath.IsAbs(engine.Socket) || filepath.Clean(engine.Socket) != engine.Socket || strings.ContainsAny(engine.Socket, "\r\n\x00") {
		return errors.New("invalid container engine configuration")
	}
	if engine.Type != "docker" && engine.Type != "podman" {
		return fmt.Errorf("unsupported container engine %q", engine.Type)
	}

	transport := &http.Transport{
		DisableCompression:     true,
		MaxResponseHeaderBytes: 1 << 20,
		Proxy:                  nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", engine.Socket)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}

	ping, err := get(ctx, client, "/_ping")
	if err != nil {
		return fmt.Errorf("container engine ping failed: %w", err)
	}
	if strings.TrimSpace(string(ping)) != "OK" {
		return errors.New("container engine ping returned an unexpected response")
	}
	version, err := get(ctx, client, "/version")
	if err != nil {
		return fmt.Errorf("container engine version check failed: %w", err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(version, &document); err != nil || len(document) == 0 {
		return errors.New("container engine version returned invalid JSON")
	}
	return nil
}

func get(ctx context.Context, client *http.Client, path string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://engine"+path, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 1<<20 {
		return nil, errors.New("container engine response exceeds 1 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP status %s", response.Status)
	}
	return body, nil
}
