// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"reflect"
	"testing"
)

// TestApplicationRequestCoversEveryWritableField is the ApplicationDTO analogue
// of TestPutRequestFromDTOCoversEveryWritableField, added for issue #83: like
// RuntimeSpecDTO/PutRuntimeSpecRequest, the ApplicationDTO and its write request
// are two hand-maintained field lists with no compiler tie between them, so a
// field added to one and forgotten on the other silently stops round-tripping.
// The runtime-spec guard earned its keep by catching exactly that during #81.
//
// This is the field-SET half of that guard: every writable ApplicationDTO field
// must have a request counterpart and vice-versa, with the two deliberate
// asymmetries named (identity/derived DTO fields that are never input; the
// write-only token). Unlike the runtime-spec guard there is no value round-trip
// half here: applications have no pure DTO->request mapper (putRequestFromDTO's
// analogue) -- a request is decoded from HTTP and applied inline inside
// CreateApplication/UpdateApplication, and the DTO is built from the stored
// routing.Application by applicationDTO. So the set-parity check below is what
// forces the two structs to grow together; a value round-trip would have to go
// through the service, a different shape left out of scope here.
func TestApplicationRequestCoversEveryWritableField(t *testing.T) {
	// ApplicationDTO fields that deliberately have NO request counterpart --
	// identity, derived, or operational read-only echoes.
	notInRequest := map[string]bool{
		"id":              true, // the application's own identity (server-assigned)
		"server_id":       true, // identity: the owning server is the URL arg to Create/UpdateApplication, never the body
		"endpoint":        true, // derived (routing.ApplicationEndpoint), read-only
		"api_token_set":   true, // derived presence (APIToken != ""); the value is write-only via api_token below
		"reachable":       true, // operational metadata from the app-health registry (enrichReachability), read-only
		"last_checked_at": true, // operational metadata (enrichReachability), read-only
		"created_at":      true, // server-assigned timestamp
	}
	// Request fields that deliberately have NO ApplicationDTO echo.
	writeOnlyInRequest := map[string]bool{
		// api_token: nil=keep / ""=clear / value=replace-and-seal. The DTO exposes
		// only api_token_set presence, never the value, so it can never round-trip.
		"api_token": true,
	}

	dtoTags := writableTagSet(t, reflect.TypeOf(ApplicationDTO{}), notInRequest)
	reqTags := writableTagSet(t, reflect.TypeOf(CreateApplicationRequest{}), writeOnlyInRequest)

	for tag := range dtoTags {
		if !reqTags[tag] {
			t.Fatalf("ApplicationDTO field %q has no CreateApplicationRequest counterpart: either it is not writable (add it to notInRequest, with a reason) or the request is missing it", tag)
		}
	}
	for tag := range reqTags {
		if !dtoTags[tag] {
			t.Fatalf("CreateApplicationRequest field %q has no ApplicationDTO counterpart, so it cannot round-trip back to the operator (add it to writeOnlyInRequest with a reason, or add the DTO field)", tag)
		}
	}
}

// TestCreateAndUpdateApplicationRequestsCarryTheSameFields pins the two write
// requests to one writable surface: CreateApplicationRequest takes create-time
// values, UpdateApplicationRequest takes the same fields as keep/clear/replace
// pointers, and a field added to one but not the other is a silent gap (a
// setting the operator can set on create but never change, or vice-versa).
// jsonTagName strips the ",omitempty" the update pointers carry, so the tag sets
// compare directly.
func TestCreateAndUpdateApplicationRequestsCarryTheSameFields(t *testing.T) {
	create := writableTagSet(t, reflect.TypeOf(CreateApplicationRequest{}), nil)
	update := writableTagSet(t, reflect.TypeOf(UpdateApplicationRequest{}), nil)

	for tag := range create {
		if !update[tag] {
			t.Fatalf("CreateApplicationRequest field %q has no UpdateApplicationRequest counterpart: an operator could set it at create time but never change it", tag)
		}
	}
	for tag := range update {
		if !create[tag] {
			t.Fatalf("UpdateApplicationRequest field %q has no CreateApplicationRequest counterpart: an operator could change it but not set it at create time", tag)
		}
	}
}

// writableTagSet returns the json-tag names of t's fields, skipping any in
// exclude. It shares jsonTagName with the runtime-spec guard (same package).
func writableTagSet(t *testing.T, typ reflect.Type, exclude map[string]bool) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		tag := jsonTagName(t, typ.Field(i))
		if exclude[tag] {
			continue
		}
		out[tag] = true
	}
	return out
}
