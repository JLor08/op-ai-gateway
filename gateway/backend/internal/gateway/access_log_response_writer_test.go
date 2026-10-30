// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// statusLog is a ResponseWriter that records every status code forwarded to it, standing in for
// the real connection under accessLogResponseWriter.
type statusLog struct {
	*httptest.ResponseRecorder
	forwarded []int
}

func (s *statusLog) WriteHeader(code int) {
	s.forwarded = append(s.forwarded, code)
	s.ResponseRecorder.WriteHeader(code)
}

// The access log (and the request span) must record the FINAL response status. net/http lets a
// handler send any number of interim 1xx responses (100 Continue, 102 Processing, 103 Early
// Hints) ahead of it, so the wrapper forwards them to the connection without latching them;
// 101 Switching Protocols is terminal and latches like a final status.
func TestAccessLogResponseWriterRecordsTheFinalStatusNotInterimOnes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		codes      []int
		wantStatus int
	}{
		{"no header written keeps the default", nil, http.StatusOK},
		{"final only", []int{http.StatusNotFound}, http.StatusNotFound},
		{"100 Continue then 200", []int{http.StatusContinue, http.StatusOK}, http.StatusOK},
		{"102 Processing then 200", []int{http.StatusProcessing, http.StatusOK}, http.StatusOK},
		{"103 Early Hints then 200", []int{http.StatusEarlyHints, http.StatusOK}, http.StatusOK},
		{"several interim then a refusal", []int{http.StatusContinue, http.StatusEarlyHints, http.StatusServiceUnavailable}, http.StatusServiceUnavailable},
		{"101 Switching Protocols is recorded", []int{http.StatusSwitchingProtocols}, http.StatusSwitchingProtocols},
		{"100 then 101", []int{http.StatusContinue, http.StatusSwitchingProtocols}, http.StatusSwitchingProtocols},
		{"the first final status wins", []int{http.StatusContinue, http.StatusOK, http.StatusInternalServerError}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			under := &statusLog{ResponseRecorder: httptest.NewRecorder()}
			w := &accessLogResponseWriter{ResponseWriter: under, status: http.StatusOK}
			for _, c := range tc.codes {
				w.WriteHeader(c)
			}
			if w.status != tc.wantStatus {
				t.Fatalf("recorded status = %d, want %d", w.status, tc.wantStatus)
			}
			// Every code still reaches the connection, interim ones included.
			if !reflect.DeepEqual(under.forwarded, tc.codes) && len(tc.codes) > 0 {
				t.Fatalf("forwarded codes = %v, want %v", under.forwarded, tc.codes)
			}
		})
	}
}

func TestAccessLogIsInterimStatus(t *testing.T) {
	for code, want := range map[int]bool{
		99: false, 100: true, 102: true, 103: true, 199: true,
		http.StatusSwitchingProtocols: false, 200: false, 404: false,
	} {
		if got := isInterimStatus(code); got != want {
			t.Errorf("isInterimStatus(%d) = %v, want %v", code, got, want)
		}
	}
}
