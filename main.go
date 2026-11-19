// Entry lifecycle: bind an ephemeral port on all interfaces, announce it
// to the env, serve until the env reports done, then exit 0. Mirrors the
// v1 Node main.ts lifecycle (same retries, same refusal statuses).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"
)

var envURL = strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")

func backoff(attempt int) time.Duration {
	if attempt > 8 {
		attempt = 8
	}
	ms := 250 * (1 << uint(attempt))
	if ms > 2000 {
		ms = 2000
	}
	return time.Duration(ms) * time.Millisecond
}

func announce(port int, client *http.Client) error {
	attempt := 0
	for {
		body, _ := json.Marshal(map[string]any{"port": port})
		req, err := http.NewRequest("POST", envURL+"/register", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("content-type", "application/json")
		res, err := client.Do(req)
		if err == nil {
			res.Body.Close()
			if res.StatusCode == 200 || res.StatusCode == 409 {
				return nil
			}
			if res.StatusCode == 400 || res.StatusCode == 405 {
				return fmt.Errorf("register refused with HTTP %d", res.StatusCode)
			}
		}
		time.Sleep(backoff(attempt))
		attempt++
	}
}

func waitDone(client *http.Client) {
	attempt := 0
	for {
		res, err := client.Get(envURL + "/done")
		if err == nil {
			var body struct {
				Done bool `json:"done"`
			}
			_ = json.NewDecoder(res.Body).Decode(&body)
			res.Body.Close()
			if res.StatusCode >= 200 && res.StatusCode < 300 && body.Done {
				return
			}
		}
		time.Sleep(backoff(attempt))
		attempt++
	}
}

func main() {
	// The live set is small (a few MB of objects); collecting rarely
	// trades a little heap for measurably less GC CPU on the hot paths.
	debug.SetGCPercent(500)
	if envURL == "" {
		fmt.Fprintln(os.Stderr, "[forge] MAKEFUN_ENV_URL is not set")
		os.Exit(1)
	}
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] %v\n", err)
		os.Exit(1)
	}
	addr := ln.Addr().(*net.TCPAddr)
	fmt.Fprintf(os.Stdout, "[forge] listening on %d\n", addr.Port)
	go serve(ln)
	client := &http.Client{Timeout: 30 * time.Second}
	if err := announce(addr.Port, client); err != nil {
		fmt.Fprintf(os.Stderr, "[forge] %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "[forge] registered")
	waitDone(client)
	ln.Close()
	os.Exit(0)
}
