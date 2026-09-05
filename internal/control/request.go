package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"
)

// callControl preconnects once. Fallback is only safe before any HTTP request
// was sent; a lost response must never replay a mutation against another owner.
func callControl(ctx context.Context, socket, tcp, method, route string, body []byte, out any) (bool, error) {
	for _, endpoint := range []struct{ network, address string }{{"unix", socket}, {"tcp", tcp}} {
		if endpoint.address == "" {
			continue
		}
		if endpoint.network == "tcp" && !loopbackAddr(endpoint.address) {
			return false, errors.New("control: TCP address must be loopback")
		}
		d := net.Dialer{Timeout: time.Second}
		conn, err := d.DialContext(ctx, endpoint.network, endpoint.address)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
				continue
			}
			return false, err
		}
		return true, requestConnected(ctx, conn, method, route, body, out)
	}
	return false, nil
}

func requestConnected(ctx context.Context, conn net.Conn, method, route string, body []byte, out any) error {
	defer conn.Close()
	used := false
	tr := &http.Transport{DisableKeepAlives: true, DialContext: func(context.Context, string, string) (net.Conn, error) {
		if used {
			return nil, errors.New("control: request is not replayable")
		}
		used = true
		return conn, nil
	}}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, method, "http://cloudfs"+route, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CloudFS-Control", "1")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	const maxResponse = 4 << 20
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return err
	}
	if len(b) > maxResponse {
		return errors.New("control: response exceeded 4 MiB; reduce the list limit")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("control: %s: %s: %s", route, resp.Status, strings.TrimSpace(string(b[:min(len(b), 4096)])))
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("control: invalid response: %w", err)
	}
	return nil
}
