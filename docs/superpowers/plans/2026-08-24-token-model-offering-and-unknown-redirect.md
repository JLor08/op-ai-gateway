# API-Token: Angebotene Override-Namen und Umleitung unbekannter Modelle — Implementierungsplan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Pro Override-Zeile steuerbar machen, ob ihr Name als Modell angeboten und ihr Ziel verborgen wird; unbekannte Modellanfragen optional auf das zuletzt genutzte Modell bzw. einen Fallback umleiten; das zuletzt genutzte Modell je Token führen und anzeigen.

**Architecture:** Die Auflösung bleibt im Gateway-Layer, wo `resolveModelOverride` schon sitzt — das Routing lernt nichts über Tokens. Die Override-Map wechselt von `map[string]string` auf `map[string]ModelOverrideRule`; weil die Spalte ein JSON-String ist, genügt ein toleranter Decoder statt einer Datenmigration. Die Modell-Liste bekommt ein Alias-Overlay in `modelFlavorSets`.

**Tech Stack:** Go 1.25 (stdlib-only Backend), SQLite/PostgreSQL/Memory hinter dem Dialekt-Seam, React + TypeScript + MUI + Vitest im Portal.

## Global Constraints

- Backend ist stdlib-only: keine neuen Go-Abhängigkeiten.
- Migrationen sind append-only; neue Spalten ausschließlich über `addColumnIfMissing`. Nächste freie Version: **63**.
- Jede Store-Änderung muss gegen alle drei Treiber laufen (Memory, SQLite, PostgreSQL) — Store-Konformitätssuite.
- i18n: `de` und `en` müssen exakt dieselben Schlüssel haben, der Build erzwingt das über `PortalMessages = typeof de`.
- Defaults reproduzieren das heutige Verhalten exakt: `unknown_model_redirect=false`, `unknown_model_redirect_blocked=false`, `unknown_model_fallback=''`, jede Alt-Override-Zeile `offer=false, hide_target=false`.
- Die Invariante der Spec: Ein Umleitungsziel durchläuft alle Zulassungsprüfungen so, als hätte der Client es genannt. Die Umleitung ändert nie, was ein Token erreichen darf.
- Kein neuer Fehlercode: Validierungsfehler der neuen Felder nutzen `ErrTokenModelOverrideInvalid` (`portal.token_model_override_invalid`).
- Alle Kommentare und Bezeichner im Code auf Englisch (Bestandskonvention); nur Nutzertexte sind übersetzt.

---

## Dateiübersicht

| Datei | Verantwortung |
|---|---|
| `gateway/backend/internal/store/token_override.go` | Codec der Override-Regeln, tolerant gegenüber dem Altformat |
| `gateway/backend/internal/store/migrate.go` | Migration 63: vier neue Spalten auf `api_tokens` |
| `gateway/backend/internal/store/models.go` | `TokenRecord` um die vier Felder erweitern |
| `gateway/backend/internal/store/sqlite_token.go` | Lesen/Schreiben der neuen Spalten, `SetTokenLastUsedModel` |
| `gateway/backend/internal/store/memory_token.go` | Dasselbe für den Memory-Treiber |
| `gateway/backend/internal/auth/token_store.go` | `auth.Token` trägt Regeln + Umleitungseinstellungen + Merker |
| `gateway/backend/internal/portal/service_model_offering.go` | **neu**: `ModelOffering` (angeboten + existierend), Alias-Overlay |
| `gateway/backend/internal/portal/service.go` | DTOs, Validierung, Verdrahtung |
| `gateway/backend/internal/gateway/inference_redirect.go` | **neu**: reine Umleitungsentscheidung |
| `gateway/backend/internal/gateway/inference_handlers.go` | Verdrahtung in `inferencePreflight` |
| `gateway/frontend/src/components/shared/ModelOverrideEditor.tsx` | Zwei Checkboxen je Zeile, Wire-Format |
| `gateway/frontend/src/components/TokenList.tsx` | Umleitungs-Abschnitt, Merker-Anzeige, neue Spalte |
| `gateway/frontend/src/components/ServiceTokensSection.tsx` | Derselbe Abschnitt für Service-Tokens |
| `gateway/frontend/src/i18n.ts` | Neue Schlüssel in `de` und `en` |

---

## Task 1: Codec der Override-Regeln

**Files:**
- Modify: `gateway/backend/internal/store/token_override.go`
- Test: `gateway/backend/internal/store/token_override_test.go`

**Interfaces:**
- Produces: `type ModelOverrideRule struct { To string; Offer bool; HideTarget bool }`, `func DecodeModelOverrideRules(s string) map[string]ModelOverrideRule`, `func EncodeModelOverrideRules(m map[string]ModelOverrideRule) string`

- [ ] **Step 1: Write the failing tests**

In `token_override_test.go` anhängen:

```go
func TestDecodeModelOverrideRulesLegacyStringMap(t *testing.T) {
	// Rows written before this feature hold a plain requested->model map. They
	// must keep working and must default both switches to false, so no existing
	// token changes behavior.
	got := DecodeModelOverrideRules(`{"gpt-4o":"qwen3-32b"}`)
	want := map[string]ModelOverrideRule{"gpt-4o": {To: "qwen3-32b"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy decode = %#v, want %#v", got, want)
	}
}

func TestDecodeModelOverrideRulesObjectForm(t *testing.T) {
	got := DecodeModelOverrideRules(`{"gpt-4o":{"to":"qwen3-32b","offer":true,"hide_target":true}}`)
	want := map[string]ModelOverrideRule{"gpt-4o": {To: "qwen3-32b", Offer: true, HideTarget: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("object decode = %#v, want %#v", got, want)
	}
}

func TestDecodeModelOverrideRulesBlankAndMalformed(t *testing.T) {
	// Malformed is "none", never an error: a bad row must not break token auth.
	for _, in := range []string{"", "   ", "not json", "[1,2]"} {
		if got := DecodeModelOverrideRules(in); got != nil {
			t.Fatalf("DecodeModelOverrideRules(%q) = %#v, want nil", in, got)
		}
	}
}

func TestDecodeModelOverrideRulesDropsEmptyTarget(t *testing.T) {
	// A rule with no target cannot route anywhere; keeping it would advertise an
	// alias that resolves to nothing.
	if got := DecodeModelOverrideRules(`{"a":{"to":"  ","offer":true},"b":"x"}`); !reflect.DeepEqual(
		got, map[string]ModelOverrideRule{"b": {To: "x"}}) {
		t.Fatalf("drop-empty decode = %#v", got)
	}
}

func TestEncodeModelOverrideRulesRoundTrip(t *testing.T) {
	in := map[string]ModelOverrideRule{"gpt-4o": {To: "qwen3-32b", Offer: true}}
	if got := DecodeModelOverrideRules(EncodeModelOverrideRules(in)); !reflect.DeepEqual(got, in) {
		t.Fatalf("round trip = %#v, want %#v", got, in)
	}
}

func TestEncodeModelOverrideRulesEmptyIsBlank(t *testing.T) {
	// "no entries" round-trips as the empty string, not "{}" — the column default.
	if got := EncodeModelOverrideRules(nil); got != "" {
		t.Fatalf("EncodeModelOverrideRules(nil) = %q, want \"\"", got)
	}
}
```

Der Import-Block braucht `reflect`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd gateway/backend && go test ./internal/store/ -run TestDecodeModelOverrideRules -v`
Expected: FAIL, `undefined: DecodeModelOverrideRules`

- [ ] **Step 3: Implement the codec**

In `token_override.go` ergänzen (die bestehenden `DecodeModelOverrideMap`/`EncodeModelOverrideMap` bleiben vorerst stehen, Task 2 entfernt sie):

```go
// ModelOverrideRule is one row of a token's model-override map: the gateway
// model a requested name resolves to, plus whether that requested name is
// advertised in the model listing (Offer) and whether the target's own name is
// dropped from it (HideTarget). Both switches affect the LISTING only — the
// listing is a display, never an access control: a hidden target stays callable
// under its real name, exactly as before this feature.
type ModelOverrideRule struct {
	To         string `json:"to"`
	Offer      bool   `json:"offer,omitempty"`
	HideTarget bool   `json:"hide_target,omitempty"`
}

// DecodeModelOverrideRules parses api_tokens.model_override_map. It accepts BOTH
// shapes: the object form written since this feature, and the plain
// requested->model string map written before it. The legacy form yields rules
// with both switches false, so no pre-existing token changes behavior — that is
// why this needs no data migration.
//
// An empty, blank or malformed value yields nil (no per-model overrides);
// malformed is treated as "none" rather than an error so a bad row never breaks
// token resolution. Rules with a blank target are dropped: they could not route
// anywhere, and advertising such an alias would offer a dead name.
func DecodeModelOverrideRules(s string) map[string]ModelOverrideRule {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil
	}
	out := make(map[string]ModelOverrideRule, len(raw))
	for name, value := range raw {
		rule, ok := decodeOverrideRule(value)
		if !ok || strings.TrimSpace(rule.To) == "" {
			continue
		}
		rule.To = strings.TrimSpace(rule.To)
		out[name] = rule
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// decodeOverrideRule reads one entry in either shape.
func decodeOverrideRule(value json.RawMessage) (ModelOverrideRule, bool) {
	var legacy string
	if err := json.Unmarshal(value, &legacy); err == nil {
		return ModelOverrideRule{To: legacy}, true
	}
	var rule ModelOverrideRule
	if err := json.Unmarshal(value, &rule); err != nil {
		return ModelOverrideRule{}, false
	}
	return rule, true
}

// EncodeModelOverrideRules serializes the rules into the JSON string stored in
// api_tokens.model_override_map. An empty/nil map yields "" (the column
// default) so "no entries" round-trips as the empty string, not "{}".
func EncodeModelOverrideRules(m map[string]ModelOverrideRule) string {
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd gateway/backend && go test ./internal/store/ -run "ModelOverrideRule" -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/store/token_override.go gateway/backend/internal/store/token_override_test.go
git commit -m "feat(store): model-override rules codec, tolerant of the legacy map"
```

---

## Task 2: Neue Token-Spalten und Durchleitung bis auth.Token

**Files:**
- Modify: `gateway/backend/internal/store/migrate.go` (Registry am Ende + neue `migration63Up`)
- Modify: `gateway/backend/internal/store/models.go:117-140` (`TokenRecord`)
- Modify: `gateway/backend/internal/store/sqlite_token.go` (select/insert/update, `TokenBySecret`)
- Modify: `gateway/backend/internal/store/memory_token.go`
- Modify: `gateway/backend/internal/auth/token_store.go:20-30`
- Test: `gateway/backend/internal/store/store_conformance_test.go` (bestehende Suite erweitern)

**Interfaces:**
- Consumes: `store.ModelOverrideRule`, `store.DecodeModelOverrideRules`, `store.EncodeModelOverrideRules` (Task 1)
- Produces: `TokenRecord.LastUsedModel/UnknownModelRedirect/UnknownModelRedirectBlocked/UnknownModelFallback` (alle `string`/`bool`), `auth.Token.ModelOverrideRules map[string]store.ModelOverrideRule` sowie dieselben vier Felder auf `auth.Token`

- [ ] **Step 1: Write the failing test**

In der Store-Konformitätssuite (dort, wo Tokens angelegt und wieder gelesen werden) ergänzen:

```go
func testTokenRedirectSettingsRoundTrip(t *testing.T, s Store) {
	t.Helper()
	rec := TokenRecord{
		ID: "tok_redirect", UserID: "usr_1", Name: "redirect",
		SecretHash: "h", SecretPrefix: "p", Status: TokenStatusActive, Scopes: `["gateway:use"]`,
		ModelOverrideMap:            EncodeModelOverrideRules(map[string]ModelOverrideRule{"gpt-4o": {To: "qwen3-32b", Offer: true}}),
		UnknownModelRedirect:        true,
		UnknownModelRedirectBlocked: true,
		UnknownModelFallback:        "fallback-model",
		LastUsedModel:               "qwen3-32b",
	}
	if err := s.CreateToken(context.Background(), rec); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	got, err := s.TokenByID(context.Background(), "tok_redirect")
	if err != nil {
		t.Fatalf("TokenByID: %v", err)
	}
	if !got.UnknownModelRedirect || !got.UnknownModelRedirectBlocked {
		t.Fatalf("switches lost: %+v", got)
	}
	if got.UnknownModelFallback != "fallback-model" || got.LastUsedModel != "qwen3-32b" {
		t.Fatalf("fallback/last-used lost: %+v", got)
	}
	rules := DecodeModelOverrideRules(got.ModelOverrideMap)
	if !rules["gpt-4o"].Offer || rules["gpt-4o"].To != "qwen3-32b" {
		t.Fatalf("rules lost: %#v", rules)
	}
}

func testTokenDefaultsUnchanged(t *testing.T, s Store) {
	t.Helper()
	// A token created without touching the new fields must read back exactly as
	// before the feature: redirect off, no fallback, no last-used model.
	rec := TokenRecord{ID: "tok_plain", UserID: "usr_1", Name: "plain",
		SecretHash: "h", SecretPrefix: "p2", Status: TokenStatusActive, Scopes: `["gateway:use"]`}
	if err := s.CreateToken(context.Background(), rec); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	got, _ := s.TokenByID(context.Background(), "tok_plain")
	if got.UnknownModelRedirect || got.UnknownModelRedirectBlocked ||
		got.UnknownModelFallback != "" || got.LastUsedModel != "" {
		t.Fatalf("defaults changed: %+v", got)
	}
}
```

Beide Funktionen in der Liste registrieren, die die Konformitätssuite je Treiber durchläuft (dem bestehenden Muster in der Datei folgen).

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd gateway/backend && go test ./internal/store/ -run "Conformance" 2>&1 | head -20`
Expected: FAIL, `rec.UnknownModelRedirect undefined`

- [ ] **Step 3: Migration, Record, Treiber und auth.Token**

`migrate.go`, ans Ende der Migrationsfunktionen:

```go
// migration63Up adds the per-token unknown-model redirect settings and the
// last-used-model marker. Defaults reproduce the pre-feature behavior exactly:
// the redirect is off, so resolution is unchanged for every existing token.
// The model-override map needs NO migration — the column holds a JSON string
// and DecodeModelOverrideRules reads the legacy shape as rules with both
// listing switches false.
func migration63Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	cols := []string{
		"last_used_model text not null default ''",
		"unknown_model_redirect integer not null default 0",
		"unknown_model_redirect_blocked integer not null default 0",
		"unknown_model_fallback text not null default ''",
	}
	for _, col := range cols {
		if err := addColumnIfMissing(ctx, tx, dl, "api_tokens", col); err != nil {
			return err
		}
	}
	return nil
}
```

In der Registry ergänzen:

```go
	{version: 63, name: "token_unknown_model_redirect", up: migration63Up},
```

`models.go`, in `TokenRecord` nach `ServerOverrideForceUnreachable`:

```go
	// LastUsedModel is the gateway model or group name of this token's last
	// SUCCESSFULLY ROUTED request ("" = none yet). Kept for every token, not
	// only those using the redirect below, because the token list shows it.
	LastUsedModel string
	// UnknownModelRedirect turns on the unknown-model redirect: a requested
	// model that does not apply falls back to LastUsedModel, then to
	// UnknownModelFallback. An exact override row and the catch-all both win
	// over it.
	UnknownModelRedirect bool
	// UnknownModelRedirectBlocked widens what counts as "does not apply": with
	// it, a model that exists but this token may not use (allowlist, resource
	// group visibility) is redirected too, instead of being refused.
	UnknownModelRedirectBlocked bool
	// UnknownModelFallback is the model or group used when LastUsedModel is
	// empty or no longer offered ("" = none, the request then fails as before).
	UnknownModelFallback string
```

`sqlite_token.go`: die vier Spalten in die `select`-Spaltenlisten (Zeilen ~50 und ~106), in das `insert` und in das `update` von `UpdateToken` aufnehmen, und in `TokenBySecret` (~317) auf `auth.Token` mappen. `ModelOverrideMap:` dort auf `ModelOverrideRules: DecodeModelOverrideRules(record.ModelOverrideMap)` umstellen und die alten `DecodeModelOverrideMap`/`EncodeModelOverrideMap` samt Aufrufern entfernen.

`memory_token.go`: die vier Felder im gespeicherten Record mitführen (der Memory-Treiber hält `TokenRecord` direkt, es genügt, dass Create/Update sie nicht verwerfen).

`auth/token_store.go`, `Token`:

```go
	// ModelOverrideRules maps a REQUESTED model name -> the rule for it (target
	// plus the two listing switches). Takes precedence over ModelOverride, the
	// catch-all. Empty/nil = no per-model overrides.
	ModelOverrideRules map[string]store.ModelOverrideRule
	// LastUsedModel / UnknownModelRedirect / UnknownModelRedirectBlocked /
	// UnknownModelFallback mirror the token record — see store.TokenRecord.
	LastUsedModel               string
	UnknownModelRedirect        bool
	UnknownModelRedirectBlocked bool
	UnknownModelFallback        string
```

`cloneOverrideMap` auf den neuen Typ umstellen (gleiche Semantik, `map[string]store.ModelOverrideRule`).

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd gateway/backend && go build ./... && go test ./internal/store/ ./internal/auth/`
Expected: PASS

- [ ] **Step 5: Verify the migration against real PostgreSQL**

Run: `cd gateway/backend && OP_TEST_POSTGRES_DSN="postgres://postgres:postgres@127.0.0.1:5432/op_test?sslmode=disable" go test ./internal/store/ -run Conformance -v 2>&1 | tail -20`
Expected: PASS. Läuft kein PostgreSQL, im Task-Report vermerken, dass dieser Schritt übersprungen wurde — nicht stillschweigend auslassen.

- [ ] **Step 6: Commit**

```bash
git add gateway/backend/internal/store gateway/backend/internal/auth
git commit -m "feat(store): per-token unknown-model redirect settings and last-used marker"
```

---

## Task 3: Merker schreiben

**Files:**
- Modify: `gateway/backend/internal/store/sqlite_token.go`, `memory_token.go` (neue Methode)
- Modify: `gateway/backend/internal/store/store.go` (Interface)
- Create: `gateway/backend/internal/gateway/inference_resolve.go`
- Modify: `gateway/backend/internal/gateway/inference_complete.go:41`, `native_passthrough.go:130`, `stream_session.go:85`
- Test: `gateway/backend/internal/gateway/inference_resolve_test.go`

**Interfaces:**
- Consumes: `TokenRecord.LastUsedModel` (Task 2)
- Produces: `func (s *Server) resolveTarget(ctx context.Context, token auth.Token, req inference.Request) (routing.Target, error)`; Store-Methode `SetTokenLastUsedModel(ctx context.Context, tokenID, model string) error`

- [ ] **Step 1: Write the failing test**

```go
func TestResolveTargetRecordsLastUsedModelOnlyOnChange(t *testing.T) {
	// A write per request would double the token table's write load on the hot
	// path; repeated requests for the same model must not write at all.
	var writes []string
	s := newTestServer(t)
	s.LastUsedModelWriter = func(_ context.Context, tokenID, model string) error {
		writes = append(writes, tokenID+"="+model)
		return nil
	}
	token := auth.Token{ID: "tok_1", LastUsedModel: "qwen3-32b"}

	if _, err := s.resolveTarget(context.Background(), token, inference.Request{Model: "qwen3-32b"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(writes) != 0 {
		t.Fatalf("unchanged model wrote %v, want no write", writes)
	}

	if _, err := s.resolveTarget(context.Background(), token, inference.Request{Model: "llama-70b"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(writes) != 1 || writes[0] != "tok_1=llama-70b" {
		t.Fatalf("changed model wrote %v, want [tok_1=llama-70b]", writes)
	}
}

func TestResolveTargetDoesNotRecordOnFailure(t *testing.T) {
	// "Last used" means last SUCCESSFULLY routed — a typo or a dead model must
	// never become the redirect target for every later request.
	var writes []string
	s := newTestServer(t)
	s.LastUsedModelWriter = func(_ context.Context, tokenID, model string) error {
		writes = append(writes, model)
		return nil
	}
	s.Resolver = failingResolver(routing.ErrNoModelRoute)

	if _, err := s.resolveTarget(context.Background(), auth.Token{ID: "tok_1"},
		inference.Request{Model: "nope"}); err == nil {
		t.Fatal("expected the resolver error to surface")
	}
	if len(writes) != 0 {
		t.Fatalf("failed resolve wrote %v, want no write", writes)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd gateway/backend && go test ./internal/gateway/ -run TestResolveTarget -v`
Expected: FAIL, `s.resolveTarget undefined`

- [ ] **Step 3: Implement**

`store.go`, ins Store-Interface:

```go
	// SetTokenLastUsedModel records the gateway model or group name of a token's
	// last successfully routed request. Callers write only on change (see
	// Server.resolveTarget) — this method itself is unconditional.
	SetTokenLastUsedModel(ctx context.Context, tokenID, model string) error
```

SQLite/PostgreSQL:

```go
func (s *SQLStore) SetTokenLastUsedModel(ctx context.Context, tokenID, model string) error {
	_, err := s.exec(ctx, `update api_tokens set last_used_model = ?, updated_at = ? where id = ?`,
		model, time.Now().UTC(), tokenID)
	return err
}
```

Memory-Treiber analog unter dessen Mutex.

`inference_resolve.go`:

```go
// resolveTarget is the single seam through which every inference path resolves
// a target, so the last-used-model marker is recorded in exactly one place
// rather than at each of the three Resolve call sites.
//
// The marker is written only when the effective model differs from what the
// token already carries: the token table is written on every authentication
// already, and a second unconditional write per inference request would double
// that load for no gain. A failed resolve records nothing — "last used" means
// last SUCCESSFULLY routed, so a typo never becomes a token's redirect target.
//
// A write error is logged and swallowed: the marker is a convenience, never a
// reason to fail a request that has a live target.
func (s *Server) resolveTarget(ctx context.Context, token auth.Token, req inference.Request) (routing.Target, error) {
	target, err := s.Resolver.Resolve(ctx, token, req)
	if err != nil {
		return target, err
	}
	if req.Model != "" && req.Model != token.LastUsedModel && s.LastUsedModelWriter != nil {
		if wErr := s.LastUsedModelWriter(ctx, token.ID, req.Model); wErr != nil {
			s.logf("last-used-model write failed for token %s: %v", token.ID, wErr)
		}
	}
	return target, nil
}
```

`Server` bekommt das Feld `LastUsedModelWriter func(ctx context.Context, tokenID, model string) error` (in der Server-Konstruktion auf `store.SetTokenLastUsedModel` gesetzt), damit der Test es ohne Store treiben kann. Die drei Aufrufstellen von `s.Resolver.Resolve(...)` auf `s.resolveTarget(...)` umstellen.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd gateway/backend && go test ./internal/gateway/ ./internal/store/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/gateway gateway/backend/internal/store
git commit -m "feat(gateway): record the last successfully routed model per token"
```

---

## Task 4: Modell-Angebot mit Alias-Overlay

**Files:**
- Create: `gateway/backend/internal/portal/service_model_offering.go`
- Modify: `gateway/backend/internal/portal/service.go:3084-3118` (`modelFlavorSets`)
- Test: `gateway/backend/internal/portal/service_model_offering_test.go`

**Interfaces:**
- Consumes: `auth.Token.ModelOverrideRules` (Task 2)
- Produces:
  ```go
  type ModelOffering struct {
      Offered  map[string]struct{} // names this token sees/uses for one flavor
      Existing map[string]struct{} // names that exist at all for that flavor
  }
  func (s *Service) ModelOfferingFor(ctx context.Context, token auth.Token, flavor string) ModelOffering
  ```

- [ ] **Step 1: Write the failing tests**

```go
func TestOfferedAliasAppearsWithTargetFlavors(t *testing.T) {
	// An alias inherits its target's flavors: an Anthropic-only target must not
	// surface the alias in the OpenAI listing.
	svc := newOfferingTestService(t, mapping{name: "qwen3-32b", flavors: []string{"anthropic"}})
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"claude-x": {To: "qwen3-32b", Offer: true},
	})
	if _, ok := svc.ModelOfferingFor(ctx, token, "anthropic").Offered["claude-x"]; !ok {
		t.Fatal("alias missing from the anthropic listing")
	}
	if _, ok := svc.ModelOfferingFor(ctx, token, "openai").Offered["claude-x"]; ok {
		t.Fatal("alias leaked into the openai listing")
	}
}

func TestUnofferedAliasStaysOut(t *testing.T) {
	svc := newOfferingTestService(t, mapping{name: "qwen3-32b", flavors: []string{"openai"}})
	token := tokenWithRules(map[string]store.ModelOverrideRule{"gpt-4o": {To: "qwen3-32b"}})
	if _, ok := svc.ModelOfferingFor(ctx, token, "openai").Offered["gpt-4o"]; ok {
		t.Fatal("alias offered although Offer is false")
	}
}

func TestHideTargetDropsTargetName(t *testing.T) {
	svc := newOfferingTestService(t, mapping{name: "qwen3-32b", flavors: []string{"openai"}})
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"gpt-4o": {To: "qwen3-32b", Offer: true, HideTarget: true},
	})
	off := svc.ModelOfferingFor(ctx, token, "openai")
	if _, ok := off.Offered["qwen3-32b"]; ok {
		t.Fatal("hidden target still listed")
	}
	// Hiding is a listing concern only: the target still EXISTS, so the redirect
	// in Task 5 must not treat it as unknown.
	if _, ok := off.Existing["qwen3-32b"]; !ok {
		t.Fatal("hidden target dropped from Existing")
	}
}

func TestHideTargetWinsOverASecondRow(t *testing.T) {
	// Two rows onto one target, only one hiding it: a set switch is an
	// instruction, an unset one only its absence — so the target is hidden.
	svc := newOfferingTestService(t, mapping{name: "qwen3-32b", flavors: []string{"openai"}})
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"a": {To: "qwen3-32b", Offer: true, HideTarget: true},
		"b": {To: "qwen3-32b", Offer: true},
	})
	if _, ok := svc.ModelOfferingFor(ctx, token, "openai").Offered["qwen3-32b"]; ok {
		t.Fatal("target visible although one row hides it")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd gateway/backend && go test ./internal/portal/ -run "Alias|HideTarget" -v`
Expected: FAIL, `svc.ModelOfferingFor undefined`

- [ ] **Step 3: Implement**

`modelFlavorSets` so umbauen, dass es die Menge **vor** dem Unterdrücken zurückgeben kann (die Alias-Flavors eines versteckten Ziels müssen weiter ablesbar sein), und das Overlay in `service_model_offering.go`:

```go
// applyOverrideAliases overlays a token's override rules onto a flavor set:
// every rule with Offer adds its requested name, inheriting its target's
// flavors; every rule with HideTarget drops the target's own name.
//
// Flavors come from the pre-suppression set on purpose. A target hidden by
// visibility is not listed under its own name but stays callable, and the alias
// is a DIFFERENT name that does not reveal it — so an explicitly offered alias
// is listed even then.
//
// With several rules onto one target, HideTarget from any of them hides it: a
// set switch is an instruction, an unset one merely its absence.
func applyOverrideAliases(sets map[string]map[string]struct{}, preSuppress map[string]map[string]struct{}, rules map[string]store.ModelOverrideRule) {
	hidden := make(map[string]struct{})
	for name, rule := range rules {
		flavors, ok := preSuppress[rule.To]
		if !ok {
			continue // target does not exist: an alias would be a dead name
		}
		if rule.Offer {
			sets[name] = flavors
		}
		if rule.HideTarget {
			hidden[rule.To] = struct{}{}
		}
	}
	for name := range hidden {
		delete(sets, name)
	}
}
```

Und die neue Fassade:

```go
// ModelOfferingFor answers the two questions the unknown-model redirect asks
// about a requested name: does this token see it, and does it exist at all.
// Existing deliberately ignores per-token visibility and the listing switches —
// only then can the redirect tell "no such model" from "not yours".
//
// Fails open like the listing it shares its source with: a store error yields
// empty sets, which makes the redirect decline rather than send a request
// somewhere unintended.
func (s *Service) ModelOfferingFor(ctx context.Context, token auth.Token, flavor string) ModelOffering {
	out := ModelOffering{Offered: map[string]struct{}{}, Existing: map[string]struct{}{}}
	sets, err := s.modelFlavorSets(ctx, token)
	if err != nil {
		return out
	}
	for name, flavors := range sets {
		if _, ok := flavors[flavor]; ok {
			out.Offered[name] = struct{}{}
		}
	}
	// Existing is built WITHOUT the token filter and without the visibility
	// overlay: a model the token cannot see still exists, and conflating the two
	// would make every invisible model look unknown.
	views, err := s.activeMappingViews(ctx)
	if err != nil {
		return out
	}
	for _, view := range views {
		for _, f := range view.app.APIFlavors {
			if f == flavor {
				out.Existing[view.mapping.GatewayModelName] = struct{}{}
			}
		}
	}
	if entries, _, gErr := s.modelGroupOverlay(ctx, nil); gErr == nil {
		for _, e := range entries {
			if _, ok := e.Flavors[flavor]; ok {
				out.Existing[e.Name] = struct{}{}
			}
		}
	}
	return out
}
```

Die Test-Helfer in `service_model_offering_test.go`: `newOfferingTestService(t, ...mapping)` baut einen `Service` über den Memory-Store mit einer Anwendung je Flavor-Satz und je einem aktiven Mapping (dem Muster der bestehenden Portal-Tests folgen); `mapping` ist ein lokales `struct { name string; flavors []string }`; `tokenWithRules(rules)` liefert `auth.Token{ModelOverrideRules: rules}`.

`ModelsForFlavor` liest weiterhin `Offered`, sodass `/v1/models`, die Anthropic- und die LM-Studio-Liste das Overlay automatisch mitbekommen.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd gateway/backend && go test ./internal/portal/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/portal
git commit -m "feat(portal): offer override aliases in the model listing"
```

---

## Task 5: Umleitung unbekannter Modelle

**Files:**
- Create: `gateway/backend/internal/gateway/inference_redirect.go`
- Modify: `gateway/backend/internal/gateway/inference_handlers.go:488-508` (`inferencePreflight`)
- Test: `gateway/backend/internal/gateway/inference_redirect_test.go`

**Interfaces:**
- Consumes: `portal.ModelOffering` (Task 4), `auth.Token.UnknownModelRedirect*` (Task 2)
- Produces: `func redirectUnknownModel(token auth.Token, requested string, off portal.ModelOffering) string` — liefert `""`, wenn nicht umgeleitet wird

- [ ] **Step 1: Write the failing tests**

```go
func offering(offered, existing []string) portal.ModelOffering {
	out := portal.ModelOffering{Offered: map[string]struct{}{}, Existing: map[string]struct{}{}}
	for _, n := range offered {
		out.Offered[n] = struct{}{}
	}
	for _, n := range existing {
		out.Existing[n] = struct{}{}
	}
	return out
}

func TestRedirectOffWithoutTheSwitch(t *testing.T) {
	tok := auth.Token{LastUsedModel: "a"}
	if got := redirectUnknownModel(tok, "nope", offering([]string{"a"}, []string{"a"})); got != "" {
		t.Fatalf("redirected to %q although the switch is off", got)
	}
}

func TestRedirectUsesLastUsedWhenStillOffered(t *testing.T) {
	tok := auth.Token{UnknownModelRedirect: true, LastUsedModel: "a", UnknownModelFallback: "f"}
	if got := redirectUnknownModel(tok, "nope", offering([]string{"a", "f"}, []string{"a", "f"})); got != "a" {
		t.Fatalf("redirect = %q, want a", got)
	}
}

func TestRedirectFallsBackWhenLastUsedGone(t *testing.T) {
	// The marker survives the model disappearing; the fallback is exactly for it.
	tok := auth.Token{UnknownModelRedirect: true, LastUsedModel: "gone", UnknownModelFallback: "f"}
	if got := redirectUnknownModel(tok, "nope", offering([]string{"f"}, []string{"f"})); got != "f" {
		t.Fatalf("redirect = %q, want f", got)
	}
}

func TestRedirectFallsBackWhenNoLastUsedYet(t *testing.T) {
	tok := auth.Token{UnknownModelRedirect: true, UnknownModelFallback: "f"}
	if got := redirectUnknownModel(tok, "nope", offering([]string{"f"}, []string{"f"})); got != "f" {
		t.Fatalf("redirect = %q, want f", got)
	}
}

func TestRedirectGivesUpWhenFallbackNotOffered(t *testing.T) {
	// Nothing usable left: the request must fail exactly as it does today.
	tok := auth.Token{UnknownModelRedirect: true, LastUsedModel: "gone", UnknownModelFallback: "alsogone"}
	if got := redirectUnknownModel(tok, "nope", offering([]string{"other"}, []string{"other"})); got != "" {
		t.Fatalf("redirect = %q, want no redirect", got)
	}
}

func TestRedirectLeavesBlockedModelsAloneByDefault(t *testing.T) {
	// The model exists but this token does not see it. With the narrow setting a
	// refusal stays a refusal, so a misconfigured allowlist keeps being visible.
	tok := auth.Token{UnknownModelRedirect: true, LastUsedModel: "a"}
	if got := redirectUnknownModel(tok, "secret", offering([]string{"a"}, []string{"a", "secret"})); got != "" {
		t.Fatalf("redirect = %q, want no redirect for a blocked model", got)
	}
}

func TestRedirectCoversBlockedModelsWhenWidened(t *testing.T) {
	tok := auth.Token{UnknownModelRedirect: true, UnknownModelRedirectBlocked: true, LastUsedModel: "a"}
	if got := redirectUnknownModel(tok, "secret", offering([]string{"a"}, []string{"a", "secret"})); got != "a" {
		t.Fatalf("redirect = %q, want a", got)
	}
}

func TestRedirectLeavesKnownModelsAlone(t *testing.T) {
	tok := auth.Token{UnknownModelRedirect: true, LastUsedModel: "a"}
	if got := redirectUnknownModel(tok, "b", offering([]string{"a", "b"}, []string{"a", "b"})); got != "" {
		t.Fatalf("redirect = %q, want no redirect for an offered model", got)
	}
}
```

Dazu zwei Tests, die die Zusicherungen der Spec an der verdrahteten Stelle festnageln — sie gehören in `inference_handlers_test.go`, weil sie das Zusammenspiel prüfen, nicht die reine Funktion:

```go
func TestPreflightCatchAllWinsOverTheRedirect(t *testing.T) {
	// The catch-all is an explicit instruction; turning the redirect on must not
	// silently change what an existing token does.
	token := auth.Token{
		ModelOverride:        "catchall-model",
		UnknownModelRedirect: true,
		LastUsedModel:        "last-model",
	}
	pf, handled := serverWithOffering("catchall-model", "last-model").
		inferencePreflight(httptest.NewRecorder(), newInferenceRequest(t), token, nil,
			inferenceShape{model: "totally-unknown", apiFlavor: "openai"})
	if handled {
		t.Fatal("preflight refused the request")
	}
	if pf.Req.Model != "catchall-model" {
		t.Fatalf("effective model = %q, want catchall-model", pf.Req.Model)
	}
}

func TestPreflightRedirectTargetStillFacesTheAllowlist(t *testing.T) {
	// The invariant: a redirect changes WHICH name is requested, never what a
	// token may reach. A service token whose allowlist excludes the redirect
	// target must be refused, not served.
	token := auth.Token{
		Kind:                 "service",
		AllowedModels:        []string{"allowed-model"},
		UnknownModelRedirect: true,
		LastUsedModel:        "forbidden-model",
	}
	rec := httptest.NewRecorder()
	_, handled := serverWithOffering("allowed-model", "forbidden-model").
		inferencePreflight(rec, newInferenceRequest(t), token, nil,
			inferenceShape{model: "unknown", apiFlavor: "openai"})
	if !handled || rec.Code != http.StatusForbidden {
		t.Fatalf("redirect reached a model outside the allowlist: handled=%v code=%d", handled, rec.Code)
	}
}
```

`serverWithOffering(names...)` baut einen `Server`, dessen `Portal` eine `ModelOffering` mit genau diesen Namen in `Offered` und `Existing` liefert.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd gateway/backend && go test ./internal/gateway/ -run TestRedirect -v`
Expected: FAIL, `undefined: redirectUnknownModel`

- [ ] **Step 3: Implement**

```go
// redirectUnknownModel returns the model to use instead of `requested`, or ""
// when the request keeps its model. It runs AFTER resolveModelOverride, so an
// exact override row and the catch-all have already had their say — the
// redirect only ever sees what neither of them claimed.
//
// The chain: the token's last successfully routed model, but only while it is
// still offered for this flavor; then the configured fallback, under the same
// condition; then nothing, which leaves today's error untouched.
//
// "Does not apply" is deliberately narrow by default: only a name that does not
// exist at all. A model that exists but this token may not use is a refusal,
// and a refusal is a signal about a misconfiguration — silently routing around
// it costs whoever debugs it later. UnknownModelRedirectBlocked widens this to
// cover those too. Either way the caller keeps applying every admission gate to
// the RESULT, so the redirect can never reach further than the token may.
func redirectUnknownModel(token auth.Token, requested string, off portal.ModelOffering) string {
	if !token.UnknownModelRedirect {
		return ""
	}
	if _, offered := off.Offered[requested]; offered {
		return ""
	}
	if _, exists := off.Existing[requested]; exists && !token.UnknownModelRedirectBlocked {
		return ""
	}
	for _, candidate := range []string{token.LastUsedModel, token.UnknownModelFallback} {
		if candidate == "" {
			continue
		}
		if _, ok := off.Offered[candidate]; ok {
			return candidate
		}
	}
	return ""
}
```

In `inferencePreflight` direkt nach der Override-Auflösung, **vor** `modelAllowed`:

```go
	req := inference.Request{Model: resolveModelOverride(token, shape.model), RequestedModel: shape.model, APIFlavor: shape.apiFlavor, Stream: shape.stream}
	// Only tokens that opted in pay for the offering lookup; for every other
	// token this is a single boolean test.
	if token.UnknownModelRedirect {
		off := s.Portal.ModelOfferingFor(r.Context(), token, routing.NormalizeAPIFlavor(shape.apiFlavor))
		if to := redirectUnknownModel(token, req.Model, off); to != "" {
			req.Model = to
		}
	}
```

`req.RequestedModel` bleibt der ursprüngliche Client-Wunsch — die Usage-Events führen ihn bereits, damit bleibt eine Umleitung im Nachhinein nachvollziehbar.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd gateway/backend && go test ./internal/gateway/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/gateway
git commit -m "feat(gateway): redirect unknown models to the token's last-used model"
```

---

## Task 6: Portal-API — DTOs und Validierung

**Files:**
- Modify: `gateway/backend/internal/portal/service.go:744-800` (DTO), `:805-815` (Update-Request), `:1181-1290` (Create/Update), `:1385-1420` (Validierung)
- Test: `gateway/backend/internal/portal/service_token_test.go`

**Interfaces:**
- Consumes: alles aus Task 1–2
- Produces: Wire-Felder `model_override_map` (Objektform), `unknown_model_redirect`, `unknown_model_redirect_blocked`, `unknown_model_fallback`, `last_used_model` (nur lesend)

- [ ] **Step 1: Write the failing tests**

```go
func TestCreateTokenRejectsUnknownFallback(t *testing.T) {
	// Same rule and same error code as an override target: a fallback nobody can
	// route to is a configuration error, not something to discover at runtime.
	_, err := svc.CreateToken(ctx, owner, CreateTokenRequest{
		Name: "t", Scopes: []string{"gateway:use"},
		UnknownModelRedirect: true, UnknownModelFallback: "no-such-model",
	})
	if !errors.Is(err, ErrTokenModelOverrideInvalid) {
		t.Fatalf("err = %v, want ErrTokenModelOverrideInvalid", err)
	}
}

func TestCreateTokenAcceptsGroupAsFallback(t *testing.T) {
	// Models and groups share one namespace in the offering, so a group is a
	// valid fallback and must not be rejected as "no such model".
	tok, err := svc.CreateToken(ctx, owner, CreateTokenRequest{
		Name: "t", Scopes: []string{"gateway:use"},
		UnknownModelRedirect: true, UnknownModelFallback: "fast-group",
	})
	if err != nil {
		t.Fatalf("group fallback rejected: %v", err)
	}
	if tok.UnknownModelFallback != "fast-group" {
		t.Fatalf("fallback = %q, want fast-group", tok.UnknownModelFallback)
	}
}

func TestFallbackClearedWhenRedirectOff(t *testing.T) {
	// Mirrors ServerOverrideForceUnreachable, which is forced false whenever its
	// parent is empty: no stored setting that cannot apply.
	tok, err := svc.CreateToken(ctx, owner, CreateTokenRequest{
		Name: "t", Scopes: []string{"gateway:use"},
		UnknownModelRedirect: false, UnknownModelFallback: "qwen3-32b",
		UnknownModelRedirectBlocked: true,
	})
	if err != nil { t.Fatal(err) }
	if tok.UnknownModelFallback != "" || tok.UnknownModelRedirectBlocked {
		t.Fatalf("settings kept without the redirect: %+v", tok)
	}
}

func TestTokenDTOCarriesLastUsedModel(t *testing.T) {
	// Read-only: the marker is written by the inference path, never by the API.
	seedTokenWithLastUsedModel(t, "tok_1", "qwen3-32b")
	list, err := svc.ListTokens(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if list.Data[0].LastUsedModel != "qwen3-32b" {
		t.Fatalf("last_used_model = %q, want qwen3-32b", list.Data[0].LastUsedModel)
	}
}

func TestUpdateTokenIgnoresLastUsedModelFromTheClient(t *testing.T) {
	// The field exists on the DTO for display; an update request must not be
	// able to forge it, or the redirect target becomes client-controlled.
	seedTokenWithLastUsedModel(t, "tok_1", "qwen3-32b")
	if _, err := svc.UpdateToken(ctx, owner, "tok_1", UpdateTokenRequest{}); err != nil {
		t.Fatal(err)
	}
	rec, _ := store.TokenByID(ctx, "tok_1")
	if rec.LastUsedModel != "qwen3-32b" {
		t.Fatalf("last_used_model changed to %q", rec.LastUsedModel)
	}
}

func TestUpdateTokenKeepsRulesWhenFieldOmitted(t *testing.T) {
	// A nil pointer means "unchanged" throughout this API; the switches must not
	// silently reset when a client PATCHes an unrelated field.
	seedTokenWithRules(t, "tok_1", map[string]store.ModelOverrideRule{
		"gpt-4o": {To: "qwen3-32b", Offer: true, HideTarget: true},
	})
	if _, err := svc.UpdateToken(ctx, owner, "tok_1", UpdateTokenRequest{Name: ptr("renamed")}); err != nil {
		t.Fatal(err)
	}
	rec, _ := store.TokenByID(ctx, "tok_1")
	rules := store.DecodeModelOverrideRules(rec.ModelOverrideMap)
	if !rules["gpt-4o"].Offer || !rules["gpt-4o"].HideTarget {
		t.Fatalf("switches reset by an unrelated update: %#v", rules)
	}
}
```

`seedTokenWithLastUsedModel` / `seedTokenWithRules` schreiben einen Token-Record direkt über den Store, weil beide Werte über die API nicht setzbar sein sollen.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd gateway/backend && go test ./internal/portal/ -run "Fallback|LastUsedModel" -v`
Expected: FAIL, unbekannte Felder

- [ ] **Step 3: Implement**

DTO und Requests um die vier Felder erweitern (`last_used_model` nur im DTO, nicht in Create/Update), `model_override_map` auf `map[string]store.ModelOverrideRule` umstellen. Validierung:

```go
// validateUnknownModelFallback trims the fallback and, when non-empty, requires
// it to be a model or group the owner can actually route to — the same rule and
// the same error as an override target, because it is the same kind of value.
//
// The setting is cleared whenever the redirect itself is off: storing a
// fallback that cannot apply invites the reading "it is configured, so it
// works". Mirrors ServerOverrideForceUnreachable against ServerOverride.
func (s *Service) validateUnknownModelRedirect(ctx context.Context, owner auth.Token, on bool, blocked bool, fallback string) (bool, bool, string, error) {
	if !on {
		return false, false, "", nil
	}
	fallback = strings.TrimSpace(fallback)
	if fallback == "" {
		return true, blocked, "", nil
	}
	if !s.modelOrGroupExists(ctx, owner, fallback) {
		return false, false, "", ErrTokenModelOverrideInvalid
	}
	return true, blocked, fallback, nil
}

// modelOrGroupExists reports whether the owner can route to this name today.
// Both flavors count: a fallback valid for only one of them is still a valid
// setting, and the redirect checks the flavor again per request.
func (s *Service) modelOrGroupExists(ctx context.Context, owner auth.Token, name string) bool {
	for _, flavor := range knownAPIFlavors {
		if _, ok := s.ModelOfferingFor(ctx, owner, flavor).Offered[name]; ok {
			return true
		}
	}
	return false
}
```

Die bestehende `validateModelOverride` prüft ihr Ziel bereits gegen dieselbe Sicht; sie wird auf `modelOrGroupExists` umgestellt, damit es dafür genau eine Regel gibt statt zweier, die auseinanderlaufen können.

`validateModelOverrideMap` auf Regeln umstellen: Ziel wie bisher prüfen, die zwei Schalter unverändert übernehmen.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd gateway/backend && go test ./internal/portal/ && go vet ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/portal
git commit -m "feat(portal): token API for the redirect settings and override rules"
```

---

## Task 7: Frontend — zwei Schalter je Override-Zeile

**Files:**
- Modify: `gateway/frontend/src/components/shared/ModelOverrideEditor.tsx`
- Modify: `gateway/frontend/src/i18n.ts` (de ~Zeile 446, en ~Zeile 2086)
- Test: `gateway/frontend/src/components/shared/ModelOverrideEditor.test.tsx`

**Interfaces:**
- Produces: `type OverrideRow = { from: string; to: string; offer: boolean; hideTarget: boolean }`, `buildOverrideMap(rows): Record<string, {to: string; offer: boolean; hide_target: boolean}>`
- Neue i18n-Schlüssel: `tokenOverrideOffer`, `tokenOverrideOfferHint`, `tokenOverrideHideTarget`, `tokenOverrideHideTargetHint`

- [ ] **Step 1: Write the failing tests**

```tsx
it('serialises both switches into the wire shape', () => {
  expect(buildOverrideMap([{ from: 'gpt-4o', to: 'qwen3-32b', offer: true, hideTarget: false }]))
    .toEqual({ 'gpt-4o': { to: 'qwen3-32b', offer: true, hide_target: false } });
});

it('still drops incomplete rows', () => {
  expect(buildOverrideMap([{ from: '', to: 'x', offer: true, hideTarget: true }])).toEqual({});
});

it('marks offered rows in the list summary', () => {
  expect(overrideSummary(t, {
    model_override: '',
    model_override_map: { 'gpt-4o': { to: 'qwen3-32b', offer: true, hide_target: false } },
  })).toContain('gpt-4o→qwen3-32b');
});

it('toggles the offer switch of one row only', async () => {
  // Two rows share one editor; a toggle that leaked to the sibling would be
  // invisible in a single-row test.
  const onRowsChange = vi.fn();
  renderEditor({
    rows: [
      { from: 'a', to: 'x', offer: false, hideTarget: false },
      { from: 'b', to: 'y', offer: false, hideTarget: false },
    ],
    onRowsChange,
  });
  fireEvent.click(screen.getAllByRole('checkbox', { name: t.tokenOverrideOffer })[1]);
  expect(onRowsChange).toHaveBeenCalledWith([
    { from: 'a', to: 'x', offer: false, hideTarget: false },
    { from: 'b', to: 'y', offer: true, hideTarget: false },
  ]);
});
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd gateway/frontend && npx vitest run src/components/shared/ModelOverrideEditor.test.tsx`
Expected: FAIL

- [ ] **Step 3: Implement**

`OverrideRow` erweitern, `buildOverrideMap` auf die Objektform umstellen, `overrideRowsInvalid` unverändert lassen (die Schalter machen eine Zeile nie unvollständig), und je Zeile zwei `Checkbox` mit den neuen Labels rendern. Beim Einlesen bestehender Tokens beide Schalter aus der Wire-Antwort übernehmen; fehlen sie (ältere Antwort), gilt `false`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd gateway/frontend && npx vitest run src/components/shared/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add gateway/frontend/src/components/shared gateway/frontend/src/i18n.ts
git commit -m "feat(portal): per-row offer and hide-target switches"
```

---

## Task 8: Frontend — Umleitungs-Abschnitt und Merker-Anzeige

**Files:**
- Modify: `gateway/frontend/src/components/TokenList.tsx` (Formularzustand ~122-135, Submit ~231/257, Formular ~404-450)
- Modify: `gateway/frontend/src/components/ServiceTokensSection.tsx`
- Modify: `gateway/frontend/src/i18n.ts`
- Test: `gateway/frontend/src/components/TokenList.test.tsx`

**Interfaces:**
- Neue i18n-Schlüssel: `tokenUnknownRedirect`, `tokenUnknownRedirectHint`, `tokenUnknownRedirectBlocked`, `tokenUnknownRedirectBlockedHint`, `tokenUnknownFallback`, `tokenLastUsedModel`, `tokenLastUsedModelNone`

- [ ] **Step 1: Write the failing tests**

```tsx
it('keeps the sub-settings disabled until the redirect is on', async () => {
  renderTokens();
  fireEvent.click(screen.getByRole('button', { name: t.tokenCreate }));
  expect(screen.getByRole('checkbox', { name: t.tokenUnknownRedirectBlocked })).toBeDisabled();
  fireEvent.click(screen.getByRole('checkbox', { name: t.tokenUnknownRedirect }));
  expect(screen.getByRole('checkbox', { name: t.tokenUnknownRedirectBlocked })).not.toBeDisabled();
});

it('shows the last used model in the edit form', async () => {
  renderTokens({ tokens: [makeToken({ last_used_model: 'qwen3-32b' })] });
  fireEvent.click(await screen.findByRole('button', { name: t.tokenEdit }));
  expect(screen.getByText('qwen3-32b')).toBeInTheDocument();
});

it('shows a placeholder when the token has never been used', async () => {
  renderTokens({ tokens: [makeToken({ last_used_model: '' })] });
  fireEvent.click(await screen.findByRole('button', { name: t.tokenEdit }));
  expect(screen.getByText(t.tokenLastUsedModelNone)).toBeInTheDocument();
});

it('offers models and groups in one fallback picker', async () => {
  renderTokens({ models: [{ id: 'qwen3-32b' }], groups: [{ name: 'fast-group' }] });
  fireEvent.click(screen.getByRole('button', { name: t.tokenCreate }));
  fireEvent.click(screen.getByRole('checkbox', { name: t.tokenUnknownRedirect }));
  fireEvent.mouseDown(screen.getByLabelText(t.tokenUnknownFallback));
  expect(screen.getByRole('option', { name: 'qwen3-32b' })).toBeInTheDocument();
  expect(screen.getByRole('option', { name: 'fast-group' })).toBeInTheDocument();
});

it('sends the redirect settings on submit', async () => {
  const createToken = vi.fn().mockResolvedValue({ id: 'tok_new' });
  renderTokens({ api: { createToken }, models: [{ id: 'qwen3-32b' }] });
  fireEvent.click(screen.getByRole('button', { name: t.tokenCreate }));
  fireEvent.change(screen.getByLabelText(t.tokenName), { target: { value: 'neu' } });
  fireEvent.click(screen.getByRole('checkbox', { name: t.tokenUnknownRedirect }));
  fireEvent.click(screen.getByRole('checkbox', { name: t.tokenUnknownRedirectBlocked }));
  fireEvent.click(screen.getByRole('button', { name: t.tokenSave }));
  await waitFor(() =>
    expect(createToken).toHaveBeenCalledWith(
      expect.objectContaining({
        unknown_model_redirect: true,
        unknown_model_redirect_blocked: true,
      }),
    ),
  );
});
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd gateway/frontend && npx vitest run src/components/TokenList.test.tsx`
Expected: FAIL

- [ ] **Step 3: Implement**

Formularzustand um `unknownRedirect`, `unknownRedirectBlocked`, `unknownFallback` erweitern; Abschnitt unter dem Override-Editor rendern: Checkbox für die Umleitung, darunter — `disabled={!unknownRedirect}` — die Checkbox für gesperrte Modelle und der `SearchableSelect` für den Fallback, gespeist aus derselben Modell-plus-Gruppen-Liste. Darunter die Anzeige `t.tokenLastUsedModel` mit dem Wert bzw. `t.tokenLastUsedModelNone`; im Anlegen-Formular immer der Platzhalter. Denselben Abschnitt in `ServiceTokensSection` einsetzen.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd gateway/frontend && npx vitest run && npm run build`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add gateway/frontend/src
git commit -m "feat(portal): unknown-model redirect settings in the token form"
```

---

## Task 9: Frontend — Spalte „zuletzt genutztes Modell"

**Files:**
- Modify: `gateway/frontend/src/components/TokenList.tsx:317` (Spaltenliste)
- Modify: `gateway/frontend/src/components/ServiceTokensSection.tsx` (Spaltenliste)
- Test: `gateway/frontend/src/components/TokenList.test.tsx`

- [ ] **Step 1: Write the failing test**

```tsx
it('hides the last-used-model column by default and shows it from the column menu', async () => {
  renderTokens({ tokens: [makeToken({ last_used_model: 'qwen3-32b' })] });
  await screen.findByText('token-a');
  expect(screen.queryByText('qwen3-32b')).toBeNull();

  fireEvent.click(screen.getByRole('button', { name: t.listColumns }));
  fireEvent.click(screen.getByRole('checkbox', { name: t.tokenLastUsedModel }));
  expect(screen.getByText('qwen3-32b')).toBeInTheDocument();
});

it('renders the placeholder for a token that has never been used', async () => {
  renderTokens({ tokens: [makeToken({ name: 'token-a', last_used_model: '' })] });
  await screen.findByText('token-a');
  fireEvent.click(screen.getByRole('button', { name: t.listColumns }));
  fireEvent.click(screen.getByRole('checkbox', { name: t.tokenLastUsedModel }));
  expect(screen.getByText('—')).toBeInTheDocument();
});
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd gateway/frontend && npx vitest run src/components/TokenList.test.tsx -t "last-used-model"`
Expected: FAIL

- [ ] **Step 3: Implement**

```tsx
{
  id: 'last_used_model',
  label: t.tokenLastUsedModel,
  value: (row) => row.last_used_model || '—',
  filter: 'text',
  defaultHidden: true,
},
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd gateway/frontend && npx vitest run`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add gateway/frontend/src/components
git commit -m "feat(portal): last-used-model column in the token lists"
```

---

## Task 10: Architekturdokumentation

**Files:**
- Modify: `docs/architecture/cross-cutting/routing-and-model-selection.md`
- Modify: `docs/architecture/cross-cutting/api-surface.md` (Token-Felder in der Portal-API)

- [ ] **Step 1: Auflösungsreihenfolge dokumentieren**

Die vierstufige Kette (exakte Zeile → Catch-all → Merker → Fallback) samt der Invariante festhalten, dass das Umleitungsziel alle Zulassungsprüfungen durchläuft, und dass die Modell-Liste eine Anzeige ist und keine Zugriffsbeschränkung.

- [ ] **Step 2: Mermaid-Diagramm prüfen**

Wird eines ergänzt, vorher rendern lassen — in diesem Repo sind schon mehrfach Mermaid-Syntaxfehler durchgerutscht.

- [ ] **Step 3: Commit**

```bash
git add docs/architecture
git commit -m "docs: unknown-model redirect and offered override aliases"
```

---

## Abschluss

- [ ] Volle Verifikation: `cd gateway/backend && go test ./... && golangci-lint run`, `cd gateway/frontend && npx vitest run && npm run build && npm run lint`
- [ ] Store-Konformität gegen echtes PostgreSQL (Task 2, Schritt 5) — falls übersprungen, im PR benennen
- [ ] Lokales Sonar-Gate (`./scripts/sonar/sonar.sh gate`, dann `./scripts/sonar/branch-findings.sh`) laut AGENTS.md Schritt 9
- [ ] `docs/superpowers/` vor dem PR entfernen — branch-lokale Arbeitsdateien gehören nie auf `main`
