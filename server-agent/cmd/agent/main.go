// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/your-org/onprem-ai-gateway/server-agent/pkg/reporting"
)

func main() {
	gatewayURL := requiredEnv("GATEWAY_URL")
	agentID := envOr("AGENT_ID", hostname())
	interval := envOrDuration("REPORT_INTERVAL", 15*time.Second)
	client := &http.Client{Timeout: 10 * time.Second}
	collector := &reporting.Collector{}

	for {
		report := collector.Collect(agentID)
		if err := send(client, gatewayURL, report); err != nil { log.Printf("report delivery failed: %v", err) }
		time.Sleep(interval)
	}
}

func send(client *http.Client, gatewayURL string, report reporting.Report) error {
	payload, err := json.Marshal(report)
	if err != nil { return err }
	req, err := http.NewRequest(http.MethodPost, gatewayURL+"/api/v1/agent/reports", bytes.NewReader(payload))
	if err != nil { return err }
	req.Header.Set("Content-Type", "application/json")
	if token := os.Getenv("GATEWAY_AGENT_TOKEN"); token != "" { req.Header.Set("Authorization", "Bearer "+token) }
	response, err := client.Do(req)
	if err != nil { return err }
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted { return &deliveryError{status: response.Status} }
	return nil
}

type deliveryError struct{ status string }
func (e *deliveryError) Error() string { return "gateway returned " + e.status }
func requiredEnv(key string) string { value := os.Getenv(key); if value == "" { log.Fatalf("%s is required", key) }; return value }
func envOr(key, fallback string) string { if value := os.Getenv(key); value != "" { return value }; return fallback }
func envOrDuration(key string, fallback time.Duration) time.Duration { value, err := time.ParseDuration(os.Getenv(key)); if err == nil && value > 0 { return value }; return fallback }
func hostname() string { value, err := os.Hostname(); if err != nil { return "unknown" }; return value }
