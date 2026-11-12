// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/storeerr"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

var (
	ErrNoModelRoute  = errors.New("routing.no_model_route")
	ErrNoHealthyHost = errors.New("routing.no_healthy_host")
	// ErrModelNotCapable reports that candidates existed for the requested model
	// but none carries a "yes" verdict for a capability the endpoint requires.
	// It is deliberately NOT ErrNoModelRoute: "this model cannot do that" and
	// "there is no such model" are different facts, and a client that cannot
	// tell them apart cannot act on either.
	ErrModelNotCapable = errors.New("routing.model_not_capable")
	// ErrAdmissionQueueTimeout: a request waited for a free concurrency slot up to the
	// per-app admission_queue_timeout_seconds without one freeing -- an unpinned request,
	// or a pinned one that needs a free live slot on its pinned server (selectPinned).
	// Maps to HTTP 503.
	ErrAdmissionQueueTimeout = errors.New("routing.admission_queue_timeout")
	// ErrAdmissionQueueFull: the admission queue was already at its configured max depth.
	// Maps to HTTP 503.
	ErrAdmissionQueueFull = errors.New("routing.admission_queue_full")
	// ErrServerOverrideServerUnavailable: a server-override request named a server that
	// exists (offers the requested model) but is currently unusable for routing — either
	// disabled, or unhealthy/unreachable and the override did not force through it.
	ErrServerOverrideServerUnavailable = errors.New("routing.server_override_server_unavailable")
	// ErrServerOverrideModelUnavailable: a server-override request named a server that has
	// no USABLE live (active mapping + active app) offering of the requested model —
	// either no such offering at all, or one whose mapping does not carry a "yes" verdict
	// for a capability the endpoint requires (resolveServerOverride's capability gate).
	// The two share one sentinel deliberately: from the caller's side both are "that
	// server cannot serve this model", and an override names ONE server, so there is no
	// second candidate the distinction could steer to.
	ErrServerOverrideModelUnavailable = errors.New("routing.server_override_model_unavailable")
	// ErrVoiceUnavailable: a speech request named a voice that the gateway KNOWS some
	// candidate offers (its cached voice list names it), the voice therefore NARROWED
	// the candidates to the ones that offer it, and none of those is usable right now
	// -- unreachable, unhealthy or busy. It is the answer where ErrNoHealthyHost would
	// otherwise be, and only there: the generic sentinel says "this model has no host",
	// which is false when other servers of the same model are healthy and merely lack
	// the voice, and a client that cannot tell the two apart cannot act on either (retry
	// shortly vs choose another voice). It is NEVER returned for a voice nothing is known
	// to offer: that request is not narrowed at all and fails, if it fails, as it always
	// did. Maps to HTTP 503 like ErrNoHealthyHost, since the condition is the voice's
	// servers being down, which a retry can cure.
	ErrVoiceUnavailable = errors.New("routing.voice_unavailable")
)

// errAllAtCapacity is an INTERNAL sentinel from selectCandidate: every candidate is at its
// effective cap AND an admission controller is wired, so Resolve should queue + retry.
// selectCandidate returns it wrapped in an *atCapacityError, which carries the demand
// the request signals if it parks; errors.Is matches either.
var errAllAtCapacity = errors.New("routing: all candidates at capacity")

// errPinnedInstancesGone is selectPinned's answer when the request already found every
// instance of the pinned mapping on the pinned server gone
// (inference.Request.ExcludeInstanceKeys): an affinity pin is then passed over and kept
// (resolveStandard), and a server override has no host left (ErrNoHealthyHost).
var errPinnedInstancesGone = errors.New("routing: every pinned instance excluded")

// errPinServerUnusable is the answer of resolveAffinity's re-check after a wait: the
// pinned server stopped passing the gates every candidate passes (affinityServer) while
// the request was parked for a slot there.
var errPinServerUnusable = errors.New("routing: pinned server no longer serves")

// atCapacityError is errAllAtCapacity together with what the request would ask the
// demand sink for, decided where the capped set was built and sent only where the
// request is about to wait (signalDemand). demand is nil when no candidate at capacity
// was an instance: another instance could not serve the request sooner. pool is the
// capped set itself -- every candidate that passed the gates, all at their cap -- and
// instancePool whether it holds an instance candidate: a request that must not park
// is served from it (selectWithoutParking) instead of waiting.
//
// The cluster fields carry the cold or forming cluster heads of the same selection
// (cluster_gate.go): clusterPool the heads, never dispatched while the request can
// wait, clusterDemands one demand per head, clusterBound the longest park bound among
// them, and clusterFallback the code a request that can neither wait nor be served
// fails with. pool is empty when the heads are all the request could use.
type atCapacityError struct {
	demand       *DemandSignal
	pool         []MappingCandidate
	instancePool bool

	clusterPool     []MappingCandidate
	clusterDemands  []DemandSignal
	clusterBound    time.Duration
	clusterFallback *ClusterUnavailableError
}

func (e *atCapacityError) Error() string { return errAllAtCapacity.Error() }

func (e *atCapacityError) Unwrap() error { return errAllAtCapacity }

// withCluster adds split's parked cluster heads to e.
func (e *atCapacityError) withCluster(split clusterSplit) *atCapacityError {
	e.clusterPool, e.clusterDemands, e.clusterBound, e.clusterFallback = split.parked, split.demands, split.bound, split.fallback
	return e
}

// overflowDemand is the demand a request that must not park signals once it was served
// on sel from e's pools: the head's own when sel is a parked cluster head, the instance
// demand otherwise (nil when there is none).
func (e *atCapacityError) overflowDemand(sel MappingCandidate) *DemandSignal {
	for i := range e.clusterDemands {
		if slices.Contains(e.clusterDemands[i].MappingIDs, sel.Mapping.ID) {
			return &e.clusterDemands[i]
		}
	}
	return e.demand
}

// parkDemands returns the demands an all-at-capacity err carries: its instance demand,
// if any, and one per parked cluster head.
func parkDemands(err error) []DemandSignal {
	var ac *atCapacityError
	if !errors.As(err, &ac) {
		return nil
	}
	var out []DemandSignal
	if ac.demand != nil {
		out = append(out, *ac.demand)
	}
	return append(out, ac.clusterDemands...)
}

// signalDemand hands the sink each demand a request about to wait in the admission
// queue carries: one per at-capacity member with instance candidates (a plain model's
// pool is its one member), deduplicated by model. It runs in parkTurn alone -- the one
// park turn of resolveStandard, resolveGroupOnce and selectPinned (a pinned request that
// needs a free live slot), after its deadline check -- so a request signals once per
// park turn for each such member: never for a candidate a walk skipped or probed and
// then served around, never on the fail-open path a resolver without a queue takes,
// never for a request whose deadline has already passed, and never for one the queue
// has no room for (AdmissionRoom) -- a request about to be refused as full must not
// start an instance. The other sender, signalOverflow, serves a request on a busy
// candidate where it would otherwise have waited and signals once for it. A check-only
// resolve (inference.Request.CheckOnly) signals nowhere.
func (r *Resolver) signalDemand(demands ...DemandSignal) {
	if r.demand == nil || len(demands) == 0 {
		return
	}
	if room, ok := r.admission.(AdmissionRoom); ok && !room.HasRoom() {
		return
	}
	for _, d := range demands {
		r.demand.Signal(d)
	}
}

// admissionStart is when req's admission began, which a bounded queue timeout counts
// from: req.AdmissionStart when the caller set it (one client call that resolves more
// than once), now otherwise. A start in the future -- a clock the caller does not share
// -- counts as now.
func admissionStart(req inference.Request, now time.Time) time.Time {
	if req.AdmissionStart.IsZero() || req.AdmissionStart.After(now) {
		return now
	}
	return req.AdmissionStart
}

type Target struct {
	RouteID       string // serving model mapping id (kept for usage attribution)
	ServerID      string
	Provider      string
	Endpoint      string
	Model         string
	ProviderModel string
	// InstanceKey names the mapping instance this target addresses,
	// "<mapping_id>#<k>" for every k, instance 0 included (the InstanceKey
	// function), on a target built from a mapping; "" on a vendor target.
	// ProviderModel is that instance's address.
	InstanceKey string
	// InstanceCapable is true when the mapping behind this target runs
	// instances on its server (a replicable spec on an agent that runs them),
	// instance 0 included: its live slot and its load are per instance, and a
	// request that loses its instance may be re-selected onto a sibling.
	// False for every other target, which keeps today's single-slot behaviour.
	//
	// Every path that resolves to a mapping expands it into its instances: a
	// fresh selection -- a plain model's or a group member's, a group pin
	// included -- and a pinned one, a token's affinity pin (resolveAffinity) or
	// a server override (resolveServerOverride), which chooses among the
	// mapping's instances on the pinned server (selectPinned).
	InstanceCapable bool
	// InstancePool is true when the candidates this target was selected from
	// include an instance candidate -- an instance of a replicable spec, of this
	// target's mapping or of another mapping of the same model -- and so always
	// when InstanceCapable is. In such a pool a request that needs a free live
	// slot (a live session, any transcription) is judged by every candidate's
	// live slot, a mapping without instances included: a live session that finds
	// this target's slot taken is selected again, or parks, rather than refused.
	// In a pool without one it is refused busy at once, as it always was. A
	// pinned target's pool is its mapping's instances on the pinned server, so
	// there it is exactly InstanceCapable. False on a vendor target.
	InstancePool bool
	Timeout      time.Duration
	APIFlavor    string
	// APIToken is the resolved application's per-app upstream credential, still
	// SEALED (enc:/plain:). Routing never decrypts it; the cipher-holding gateway
	// layers open it and attach it to the upstream call. Empty = no upstream auth.
	APIToken string
	// APITokenHeader is an optional custom header name for APIToken; empty ⇒ the
	// default "Authorization: Bearer <token>".
	APITokenHeader string
	// APIFlavors / ResponsesMode / MessagesMode are the EFFECTIVE API-variant
	// capability + endpoint modes for this request: the resolved application's
	// values for an ordinary app, or the resolved RuntimeSpec's values for a
	// server_agent mapping that has a spec (the app's values when it has none —
	// see targetFrom). The dispatch layer (native_passthrough) reads them to pick
	// disabled / translate / pass-through and to enforce responsesServed /
	// messagesServed.
	APIFlavors    []string
	ResponsesMode EndpointMode
	MessagesMode  EndpointMode
	// DecisionsMode / SystemOneMode are the EFFECTIVE modes of the two decision
	// endpoints, resolved like ResponsesMode (spec for a server_agent mapping
	// with a spec, else the application).
	DecisionsMode EndpointMode
	SystemOneMode EndpointMode
	// OpportunisticMetrics mirrors the resolved application's per-app toggle: when
	// set, a successful real inference EWMA-updates the served mapping's throughput
	// metrics from the usage event.
	OpportunisticMetrics bool
	// ResponsesLiveTimingsEnabled is the EFFECTIVE per-endpoint live-timings
	// opt-in for this request: the resolved RuntimeSpec's value for a
	// server_agent mapping that has a spec, the resolved application's
	// otherwise -- the same precedence ResponsesMode/MessagesMode use above,
	// and deliberately NOT OpportunisticMetrics' app-only shape, because this
	// flag qualifies a decision (ResponsesMode == passthrough) that comes from
	// the spec for a server_agent child. THE REQUEST PATH READS IT, on every
	// native passthrough request: gateway.proxyNative passes the resolved
	// target to issue #81's part-2 gate, gateway.wantsResponsesLiveTimings,
	// which ANDs this flag with the effective upstream kind, the Responses
	// flavor and the stream flag and lets a recorded live-progress rejection
	// veto the lot; when all of those hold, the body relayed upstream grows
	// llama.cpp's `timings_per_token`. So this field has a live consumer
	// outside its own table test -- a dead-code pass that concludes otherwise
	// is reading a stale claim. The two things that part still owed when this
	// sentence was first written have since SHIPPED on the same branch: the
	// portal renders a visible control for the flag on both surfaces (the
	// shared ApiVariantControls block, on the application form and the
	// launch-spec form), and the `timings` objects the injection buys feed a
	// live per-token count on the running-connections panel -- a column that
	// ships hidden and is switched on from that panel's own column menu, so an
	// operator who has never changed that panel's columns sees the rate fill
	// and no count column at all. No retry accompanies the injection: llama.cpp was
	// measured accepting the injected key on this endpoint, so a retry's
	// trigger could not be exercised against any upstream this repository can
	// point at.
	ResponsesLiveTimingsEnabled bool
	// LiveProgressSupport is the mapping's PERSISTED verdict about whether this
	// upstream tolerates the live-progress parameters: "", "supported",
	// "unsupported". See wantsLiveProgress for how it combines with the shape.
	LiveProgressSupport string
	// LiveProgressSpecType is EffectiveRuntimeSpecType(spec) for a server_agent
	// application, "" otherwise -- the only shape evidence available for a child
	// whose application type says nothing about it.
	LiveProgressSpecType string
	// ExtraHeaders are STATIC headers the dispatch layer attaches to the upstream
	// request verbatim, on top of any credential header. A vendor SUBSCRIPTION
	// (OAuth) Anthropic target carries the required anthropic-version and
	// anthropic-beta oauth headers here; an ordinary self-hosted target leaves it
	// nil. They never carry a secret (the credential rides separately via the
	// upstream-auth context), so they are safe to log.
	ExtraHeaders map[string]string
	// Masquerade selects a client-side request disguise: "" (none) or
	// "claude_code" (MasqueradeClaudeCode), which makes the Anthropic client
	// prepend the Claude-Code system block required on the subscription/OAuth
	// Messages path. Only subscription targets set it.
	Masquerade string
	// VendorAccountID, when non-empty, names the vendor account that serves this
	// target, for per-account USAGE ATTRIBUTION (the recorded usage_events.account_id
	// and the scraped rate-limit snapshot). EVERY vendor target carries it now --
	// api-key and subscription alike -- so it is no longer the subscription-bearer
	// trigger it once was: that trigger moved to the explicit Subscription flag
	// below. A self-hosted target leaves this "".
	VendorAccountID string
	// Subscription marks a SUBSCRIPTION (OAuth) vendor target whose upstream bearer
	// is NOT carried in APIToken but resolved -- and refreshed if stale -- at
	// dispatch time from the account's sealed OAuth tokens (upstreamAuthCtx keys the
	// subscription-bearer path on THIS flag, not on VendorAccountID, so an API-KEY
	// vendor target that now also carries a VendorAccountID keeps using its sealed
	// APIToken). True only on the two subscription targets
	// (vendorSubscription{Anthropic,OpenAI}Target); false on the api-key
	// vendorAccountTarget and every self-hosted target.
	Subscription bool
	// OpenAIPathPrefix is the URL path segment OpenAICompatibleClient puts between
	// Endpoint and the OpenAI-dialect resource ("/chat/completions", "/models"):
	// the composed URLs are {Endpoint}{prefix}/chat/completions and {prefix}/models.
	// EMPTY means "/v1" — the value every self-hosted target, probe target and
	// api.openai.com vendor target has always used — so leaving it unset is
	// byte-identical to the pre-field behaviour. A non-empty value is a path with a
	// leading "/" and no trailing "/" (Kilo "/gateway", Gemini "/v1beta/openai").
	// Only an OpenAI-compatible vendor-account target sets it.
	OpenAIPathPrefix string
}

// OpenAIPathPrefixOrDefault returns the normalised OpenAI-dialect path prefix:
// the trimmed OpenAIPathPrefix with a leading "/" and no trailing "/", or "/v1"
// when it is blank. It is the one place the "/v1" default lives, shared by the
// provider client and the gateway usage-label path.
func (t Target) OpenAIPathPrefixOrDefault() string {
	prefix := strings.TrimRight(strings.TrimSpace(t.OpenAIPathPrefix), "/")
	if prefix == "" {
		return "/v1"
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	return prefix
}

// MasqueradeClaudeCode is the Target.Masquerade value that makes the Anthropic
// client prepend the exact Claude-Code first system block (the subscription /
// OAuth serving path). Kept with Target because it is a Target field value.
const MasqueradeClaudeCode = "claude_code"

// ReachabilityChecker reports whether an application is currently reachable.
// The resolver gates candidate selection and affinity reuse through it so that
// applications whose active reachability probe is failing are not routed to. A
// nil checker (passed to NewResolver) means all-reachable — lenient, preserving
// the pre-reachability behavior.
type ReachabilityChecker interface {
	Reachable(appID string) bool
}

// ServerBusyChecker reports whether a server is temporarily unavailable for routing
// (e.g. a benchmark is running on it). A nil checker means never busy, preserving
// the pre-benchmark behavior.
type ServerBusyChecker interface {
	ServerBusy(serverID string) bool
}

// LoadedModelChecker reports which upstream (app) model names are currently loaded for
// an application on a server, so selection can prefer a server that already has the
// requested model resident (avoiding a cold load/swap). A nil checker (or empty result)
// means loaded state is unknown — no candidate is treated as loaded, so selection is
// unchanged. Satisfied by *gateway.LoadedModelRegistry.
type LoadedModelChecker interface {
	LoadedAppModels(appID, serverID string) []string
}

// ServerActivityChecker reports a server's live inference activity for swap-protection:
// how many requests are in flight and when the last one completed. A nil checker means
// activity is unknown, so no server is protected — selection is unchanged (the P4b
// no-op invariant), exactly like a nil ServerBusyChecker / LoadedModelChecker.
type ServerActivityChecker interface {
	ServerActivity(serverID string) (inFlight int, lastCompletedAt time.Time)
}

// runtimeStateStarting is the lifecycle string RuntimeModelStateChecker reports for a
// loading (not-yet-serving) model instance. A local mirror (a plain lowercase literal,
// not the server-agent's State enum) -- routing must not import the server-agent
// module. "running" never needs its own constant here: a StateRunning instance is
// always covered by the loaded partition instead (LoadedAppModels reports
// StateRunning-only instances), so selectFromPool never has to compare against it.
const runtimeStateStarting = "starting"

// RuntimeModelStateChecker exposes the gateway's live per-model runtime state
// (from the volatile runtime-status registry) to routing. appModelName is an
// instance address (InstanceAddress: the mapping's AppModelName for instance
// 0, "AppModelName#k" for instance k), and the answer is that one instance's:
// state is its lifecycle string ("running"/"starting"/"stopped"/...);
// active/queue are its own live request counts, not the spec's total over its
// instances. ok is false when no runtime status is known for that address on
// that server (a legacy/non-runtime agent, a model with no managed spec, or an
// instance no longer reported) -- callers then fall back to per-server
// telemetry and treat state as unknown.
//
// metricsOK gates ONLY the active/queue metrics, separately from ok/state: it
// is true iff ok AND the reporting agent declared the "runtime_model_probe"
// feature (the capability that actually populates active/queue). A pre-feature
// runtime_manager agent still reports a valid lifecycle STATE (so ok=true,
// prefer-starting keeps working) but its active/queue default to a FABRICATED
// 0 -- metricsOK=false tells the resolver not to overlay that 0 onto per-server
// telemetry. Nil checker = lenient no-op (ok=false, metricsOK=false).
type RuntimeModelStateChecker interface {
	RuntimeModelState(serverID, appModelName string) (state string, active int, queue int, ok bool, metricsOK bool)
}

// AdmissionController parks a request -- an unpinned one, or a pinned one that needs a free
// live slot on its pinned server (selectPinned) -- until a slot frees on one of serverIDs
// (returns nil => Resolve retries selection), or bounds it out (ErrAdmissionQueueTimeout /
// ErrAdmissionQueueFull / a context error). A nil controller disables queuing: selectCandidate
// keeps the CP3 all-at-cap fail-open. Satisfied by *gateway.admissionQueue.
type AdmissionController interface {
	WaitForSlot(ctx context.Context, serverIDs []string, timeout time.Duration) error
}

// AdmissionRoom is implemented by an AdmissionController that can tell, before a
// request parks, whether it would take one more waiter. The resolver asks it right
// before signalling demand (signalDemand), so a request the queue is about to turn away
// as full starts no instance. The answer is a snapshot: a queue that fills between it
// and the wait still refuses a request that signalled, and a place that frees in
// between lets in a request that sent nothing that turn -- it signals on its next one.
// A controller without it is taken to have room.
type AdmissionRoom interface {
	HasRoom() bool
}

// ProvisioningGate decides which of a set of candidate server ids the principal
// may use under resource-group provisioning + the enforcement mode. A nil gate
// (unset) allows all (no-op invariant). Returns a map keyed by server id with
// value true for ALLOWED servers. Satisfied by a Task 3 adapter over the
// resource_group_provisions store.
type ProvisioningGate interface {
	AllowedServerIDs(ctx context.Context, principal auth.Token, serverIDs []string) (map[string]bool, error)
}

// VendorAccountEnumerator lists the vendor accounts a principal may route to: the
// principal's OWN accounts first (id order), then the accounts another user shared
// into a resource group the principal is provisioned into, live-share-level-checked
// (id order). It is the routing-time GRANT for shared vendor accounts (#183, spec
// 2026-10-15 section 8), exposed to routing as a seam because routing cannot import
// internal/portal. Satisfied by portal.Service.VendorAccountsForRouting.
//
// The nil semantics are the OPPOSITE of ProvisioningGate's, and load-bearing: a nil
// (unwired) enumerator falls back to OWNER-ONLY -- the principal's own accounts, the
// behaviour before sharing existed -- never allow-all. An unwired grant must fail
// closed.
type VendorAccountEnumerator interface {
	VendorAccountsForRouting(ctx context.Context, principal auth.Token) ([]VendorAccount, error)
}

// GroupPolicy is a group's selection policy: how members are ordered, which are
// eligible, and how failover behaves. Passed as one value so the seam stays
// readable and extensible.
type GroupPolicy struct {
	FailoverMode            string
	MemberOrder             string
	LoadedOnly              bool
	ClimbSpeedMarginPercent int
	MinTokensPerSecond      float64
	MinSpeedFallback        string

	// suppressWarm mirrors the group's CONFIGURED LoadedOnly and is derived internally
	// (never supplied by a GroupResolver). It exists because LoadedOnly is a filter the
	// relaxation ladder may drop, while "this group must never trigger a model load" is a
	// property of the group itself and must hold on EVERY attempt: once the ladder has
	// relaxed LoadedOnly to false, reading LoadedOnly for the climb_up warm decision would
	// let the relaxed attempt start exactly the load the setting exists to avoid. The
	// relaxation only ever clears LoadedOnly/MinTokensPerSecond, never this field.
	suppressWarm bool
}

// GroupResolver exposes the model-group config to the hot path. A nil resolver
// means no groups exist (preserving the pre-feature single-model behavior — the
// No-Op invariant). Satisfied by *gateway.GroupRegistry.
type GroupResolver interface {
	// Group returns a group's ordered members and its selection policy when name is
	// an active group; ok=false when name is not a group.
	Group(name string) (members []GroupMember, policy GroupPolicy, ok bool)
	// DirectAllowed reports whether a direct (non-group) request for modelName is
	// permitted. false only when the model's ModelSetting.Visibility == "locked"
	// (a group-only model, reachable solely via a group). Model-level, not membership.
	DirectAllowed(modelName string) bool
}

// ModelWarmer triggers a best-effort background load of a gateway model (climb_up
// load-ahead). nil = no-op (climb_up then only switches when a member is already
// loaded by other traffic). Warm MUST be non-blocking and deduplicated. Wired in
// Task 3; the field/setter exist now so Task 3 is a pure additive change.
type ModelWarmer interface {
	Warm(ctx context.Context, gatewayModelName string)
}

// sessionReservation tracks, per server, the distinct sticky sessions recently pinned
// there (an affinity hit OR a fresh pin), so the capacity cap can reserve one slot per
// live session and keep new/unpinned traffic from filling the slots active conversations
// will return to (Decision 5). Purely in-memory + volatile; a request keyed by an empty
// id is not tracked. BOTH touch (on write) and activeCount (on read) prune entries older
// than the window, so a touched server's map stays bounded by its live sessions even when
// the cap's read-time prune never runs for it (an uncapped max_concurrency==0 mapping, or
// a server that only serves pinned traffic and so never appears as a selectCandidate
// candidate) — without the write-time prune those maps would grow with cumulative, not
// live, sessions.
type sessionReservation struct {
	mu       sync.Mutex
	window   time.Duration
	byServer map[string]map[string]time.Time // serverID -> reservationKey -> lastSeen
}

func newSessionReservation(window time.Duration) *sessionReservation {
	return &sessionReservation{window: window, byServer: map[string]map[string]time.Time{}}
}

// touch records/refreshes a reservation for (serverID, key) at now, pruning that server's
// stale entries first so the map stays bounded by live sessions. A zero window or an empty
// serverID/key is a no-op. Nil-safe.
func (s *sessionReservation) touch(serverID, key string, now time.Time) {
	if s == nil || s.window <= 0 || serverID == "" || key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.byServer[serverID]
	if m == nil {
		m = map[string]time.Time{}
		s.byServer[serverID] = m
	}
	// Prune this server's stale entries on write so the map is bounded by live sessions
	// even for servers the cap's read-time activeCount prune never visits (maxC==0 /
	// pinned-only servers).
	for existing, seen := range m {
		if now.Sub(seen) >= s.window {
			delete(m, existing)
		}
	}
	m[key] = now
}

// activeCount returns the number of distinct live reservations on serverID (lastSeen
// within the window), pruning stale entries. Nil-safe / zero-window => 0.
func (s *sessionReservation) activeCount(serverID string, now time.Time) int {
	if s == nil || s.window <= 0 || serverID == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.byServer[serverID]
	if m == nil {
		return 0
	}
	live := 0
	for k, seen := range m {
		if now.Sub(seen) >= s.window {
			delete(m, k)
			continue
		}
		live++
	}
	if len(m) == 0 {
		delete(s.byServer, serverID)
	}
	return live
}

// resolverStore is the narrow slice of Store the Resolver actually calls (see
// score_servers.go's and this file's r.store.* call sites) — Store's other
// ~109 methods (server/service/resource-group/limits/certificate management,
// etc.) are irrelevant to routing resolution. *MemoryStore and
// *store.SQLStore already implement it structurally (Go interfaces are
// implicit), so every existing NewResolver call site is unchanged.
type resolverStore interface {
	ActiveMappingsForModel(ctx context.Context, gatewayModel string, apiFlavor string) ([]MappingCandidate, error)
	TelemetryByServer(ctx context.Context, serverID string) (ServerTelemetry, bool, error)
	Affinity(ctx context.Context, key AffinityKey) (RouteAffinity, bool, error)
	UpsertAffinity(ctx context.Context, affinity RouteAffinity) error
	DeleteAffinity(ctx context.Context, key AffinityKey) error
	ApplicationByID(ctx context.Context, id string) (Application, error)
	AIServerByID(ctx context.Context, id string) (AIServer, error)
	MappingsByApplication(ctx context.Context, applicationID string) ([]ModelMapping, error)
	RuntimeSpecByMapping(ctx context.Context, mappingID string) (RuntimeSpec, bool, error)
	// MappingCapabilities is read by resolveAffinity ONLY, to fill the synthetic
	// MappingCandidate it builds for a sticky-pin hit -- that path's mapping comes
	// from MappingsByApplication, which never joins model_mapping_capabilities.
	// Before this read existed the affinity path served ModelMapping's
	// (unwritten, pre-migration-78) column for the life of the pin -- up to
	// AffinityTTLSeconds.
	MappingCapabilities(ctx context.Context, mappingID string) ([]CapabilityRow, error)
	// MappingCapabilitiesForMappings is filterCapable's bulk read -- one query
	// for the whole candidate list rather than one per candidate. It is already
	// implemented on every driver (chunked in SQLite, mirrored in MemoryStore,
	// wrapped in the generated tracing decorator) and already carries an N+1
	// guard test in internal/portal, so adding it here costs nothing but this
	// line.
	MappingCapabilitiesForMappings(ctx context.Context, mappingIDs []string) (map[string][]CapabilityRow, error)
	// ModelMappingCacheForMappings is the voice filter's bulk read (#251): the
	// voices cache (ModelMappingCacheVoices) of every candidate in one query, read
	// only for a request that names a voice (voiceScopeFor). Like the capability
	// read above it is already implemented on every driver and decorated by the
	// generated tracing wrapper, so the narrow interface gains it for free.
	ModelMappingCacheForMappings(ctx context.Context, mappingIDs []string, cacheType string) (map[string][]ModelMappingCacheRow, error)
	// VendorAccountsByOwner and VendorAccountModels are the per-user vendor-account
	// surface resolveVendorAccount reads: the owner's accounts (sorted by id) -- the
	// OWNER-ONLY fallback routableVendorAccounts uses when no VendorAccountEnumerator
	// is wired -- and, for one account, the gateway models it serves (sorted by
	// GatewayModel). Both
	// are already on routing.Store and both drivers (*MemoryStore, *store.SQLStore)
	// implement them, so widening this narrow interface stays compile-safe and
	// needs no tracing-decorator regeneration (the decorator wraps routing.Store,
	// which already declares them).
	VendorAccountsByOwner(ctx context.Context, userID string) ([]VendorAccount, error)
	VendorAccountModels(ctx context.Context, accountID string) ([]VendorAccountModel, error)
}

type Resolver struct {
	store             resolverStore
	clock             func() time.Time
	checker           ReachabilityChecker
	busy              ServerBusyChecker
	loaded            LoadedModelChecker
	activity          ServerActivityChecker
	runtimeState      RuntimeModelStateChecker
	swapProtectWindow time.Duration
	reservation       *sessionReservation
	admission         AdmissionController
	provisioning      ProvisioningGate
	vendorEnumerator  VendorAccountEnumerator
	groups            GroupResolver
	warmer            ModelWarmer
	instances         InstanceSource
	instanceActivity  InstanceActivityChecker
	demand            DemandSink
	liveSlots         LiveSlotChecker
	clusterGate       ClusterGate // SetClusterGate; nil = no cluster heads
	realtimeMinFactor float64     // SetAudioRealtimeMinFactor; 0 gates nothing
	legacyAffinity    atomic.Bool // true => affinity keys on the explicit header (legacy); false (default) => ClientSessionID
	// vendorEnabled / vendorRoutingMode / vendorSubscriptionsAllowed gate and steer
	// the per-user vendor-account branch (resolveVendorAccount). All three are
	// injected by the gateway from a CACHED read of the vendor_accounts_enabled
	// master flag, the vendor_account_routing_mode setting and the
	// vendor_subscriptions_enabled switch (SetVendorAccountAccessors). Each is
	// nil-safe: a nil vendorEnabled means the whole branch is OFF (the no-op
	// invariant — a resolver built without these accessors is byte-identical to
	// today), and a nil / unknown vendorRoutingMode means vendor_first.
	vendorEnabled     func() bool
	vendorRoutingMode func() string
	// vendorSubscriptionsAllowed reports the vendor_subscriptions_enabled switch.
	// Unlike vendorEnabled it is default-ON and nil-safe in that direction: a nil
	// accessor means subscription accounts are allowed (see subscriptionsAllowed).
	vendorSubscriptionsAllowed func() bool
}

// Vendor-account routing modes, mirrored as local string literals so routing
// does not import internal/portal (which would be an import cycle — the portal
// package imports routing). They must stay equal to portal's
// VendorRoutingModeVendorFirst / VendorRoutingModeFallbackOnly.
const (
	vendorRoutingModeVendorFirst  = "vendor_first"
	vendorRoutingModeFallbackOnly = "fallback_only"
)

// vendorAccountDefaultTimeout is the per-request upstream timeout a vendor-account
// Target carries. Vendor APIs (api.openai.com / api.anthropic.com) are hosted and
// can be slow on a cold large-model request, so this is generous relative to the
// on-prem default; it is a fixed default because a vendor account has no per-app
// TimeoutMS column of its own.
const vendorAccountDefaultTimeout = 120 * time.Second

// vendorRoutePrefix is the RouteID namespace every vendor-account target shares
// ("vendor:<accountID>:<model>"), so the format lives in one place across the
// api_key and the two subscription target builders.
const vendorRoutePrefix = "vendor:"

func NewResolver(store resolverStore, clock func() time.Time, checker ReachabilityChecker) *Resolver {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &Resolver{store: store, clock: clock, checker: checker}
}

// SetServerBusyChecker installs the routing exclusion for benchmarking servers.
// Leaving it unset (nil) means no server is ever considered busy.
func (r *Resolver) SetServerBusyChecker(c ServerBusyChecker) { r.busy = c }

// SetLoadedModelChecker installs the prefer-already-loaded partition source.
// Leaving it unset (nil) means loaded state is unknown, so selection is unchanged.
func (r *Resolver) SetLoadedModelChecker(c LoadedModelChecker) { r.loaded = c }

// SetServerActivityChecker installs the swap-protection activity source and the recency
// window. window <= 0 disables the recency component (an in-flight request still protects).
func (r *Resolver) SetServerActivityChecker(c ServerActivityChecker, window time.Duration) {
	r.activity = c
	r.swapProtectWindow = window
}

// SetRuntimeModelStateChecker installs the live per-model runtime-state source (the
// volatile runtime-status registry, via a gateway adapter). Leaving it unset (nil)
// means per-model state/metrics are unknown, so scoring uses per-server telemetry only
// and no candidate is ever preferred as "already starting" (the no-op invariant).
func (r *Resolver) SetRuntimeModelStateChecker(c RuntimeModelStateChecker) { r.runtimeState = c }

// SetSessionReservation installs the session-slot reservation tracker used by the
// capacity cap (Decision 5). window <= 0 (or leaving it unset) disables reservation
// tracking, so effectiveCap == max_concurrency. Nil-safe.
func (r *Resolver) SetSessionReservation(window time.Duration) {
	if window > 0 {
		r.reservation = newSessionReservation(window)
	}
}

// SetAdmissionController installs the CP4 admission queue. Nil (unset) disables queuing
// (CP3 fail-open on all-at-cap).
func (r *Resolver) SetAdmissionController(c AdmissionController) { r.admission = c }

// SetProvisioningGate installs the resource-group provisioning filter. Leaving it
// unset (nil) means every candidate server is allowed for every principal (the
// no-op invariant) — the concrete gate (a later task) enforces resource-group
// membership + the configured enforcement mode.
func (r *Resolver) SetProvisioningGate(g ProvisioningGate) { r.provisioning = g }

// SetVendorAccountEnumerator installs the shared-vendor-account grant (see
// VendorAccountEnumerator). Leaving it unset (nil) restricts every principal to its
// OWN accounts -- owner-only, NEVER allow-all: the inverse of SetProvisioningGate's
// nil, because here an unwired seam must deny the shared arm, not open it.
func (r *Resolver) SetVendorAccountEnumerator(e VendorAccountEnumerator) { r.vendorEnumerator = e }

// SetGroupResolver installs the model-group config source. Leaving it unset (nil)
// means no groups exist, so Resolve is byte-identical to the single-model path
// (the No-Op invariant).
func (r *Resolver) SetGroupResolver(g GroupResolver) { r.groups = g }

// SetModelWarmer installs the climb_up load-ahead warmer. Leaving it unset (nil)
// makes climb_up purely passive (switch only when a higher-priority member is
// already loaded by other traffic). Consumed by Task 3.
func (r *Resolver) SetModelWarmer(w ModelWarmer) { r.warmer = w }

// SetAffinitySessionMode selects the affinity session-key source. legacy=true keys
// on the explicit X-OP-AI-Gateway-Session-ID header only (byte-identical to the
// pre-feature behavior); legacy=false (the zero value / default) keys on the
// extracted ClientSessionID. Safe to call live (atomic).
func (r *Resolver) SetAffinitySessionMode(legacy bool) { r.legacyAffinity.Store(legacy) }

// SetVendorAccountAccessors installs the three accessors that gate and steer the
// per-user vendor-account branch: enabled reports the vendor_accounts_enabled
// master flag, mode reports the vendor_account_routing_mode, and
// subscriptionsAllowed reports the vendor_subscriptions_enabled switch that
// gates only the subscription-type accounts. The gateway wires these from a
// CACHED read of system settings (invalidated on a settings PUT), so
// resolveVendorAccount never issues a settings store read on the hot path.
// Leaving them unset (nil) keeps the branch OFF (nil enabled), the mode at
// vendor_first (nil mode) and subscription accounts ALLOWED (nil
// subscriptionsAllowed: the switch is default-on, so a resolver built without it
// must not drop them) — the no-op invariant a resolver built without them relies
// on. Mirrors the other optional-dependency setters above.
func (r *Resolver) SetVendorAccountAccessors(enabled func() bool, mode func() string, subscriptionsAllowed func() bool) {
	r.vendorEnabled = enabled
	r.vendorRoutingMode = mode
	r.vendorSubscriptionsAllowed = subscriptionsAllowed
}

// vendorOn reports whether the vendor-account branch is enabled. Nil-safe: a
// resolver with no enabled accessor is OFF.
func (r *Resolver) vendorOn() bool {
	return r.vendorEnabled != nil && r.vendorEnabled()
}

// subscriptionsAllowed reports whether subscription-type vendor accounts may
// serve. Nil-safe in the OPPOSITE direction to vendorOn: the
// vendor_subscriptions_enabled setting is default-on, so a resolver with no
// accessor allows them.
func (r *Resolver) subscriptionsAllowed() bool {
	return r.vendorSubscriptionsAllowed == nil || r.vendorSubscriptionsAllowed()
}

// vendorMode reports the effective vendor routing mode, defaulting to
// vendor_first for a nil accessor or any value that is not fallback_only (a
// lenient read matching portal's own default-on-unknown behaviour).
func (r *Resolver) vendorMode() string {
	if r.vendorRoutingMode == nil {
		return vendorRoutingModeVendorFirst
	}
	if r.vendorRoutingMode() == vendorRoutingModeFallbackOnly {
		return vendorRoutingModeFallbackOnly
	}
	return vendorRoutingModeVendorFirst
}

// resolveTracer is resolved ONCE from the OTel global provider (installed by
// internal/tracing.Setup) and reused per call, so Resolve avoids the two
// process-global tracer-provider lookup mutexes otel.Tracer(name) takes on every
// call — keeping the disabled (default) path cheap. It is resolved directly from
// the global rather than via internal/tracing because the generated routing.Store
// tracing decorator lives in package tracing and imports routing, so routing must
// not import tracing (import cycle). Set at package-init, never reassigned.
var resolveTracer = otel.Tracer("op-ai-gateway")

func (r *Resolver) Resolve(ctx context.Context, token auth.Token, req inference.Request) (Target, error) {
	// When tracing is disabled the global provider yields a cheap non-recording span.
	ctx, span := resolveTracer.Start(ctx, "routing.Resolve")
	defer span.End()
	if span.IsRecording() {
		span.SetAttributes(attribute.String("model", req.Model))
	}
	if r == nil || r.store == nil {
		return Target{}, ErrNoModelRoute
	}
	now := r.clock()
	apiFlavor := NormalizeAPIFlavor(req.APIFlavor)
	// Server-override force-branch: an explicit req.ServerOverrideID short-circuits
	// EVERYTHING below it (model-group dispatch, affinity, the ActiveMappingsForModel
	// candidate block) — it is a distinct, minimal routing path, not a filter layered
	// onto the normal one. See resolveServerOverride for exactly which checks still
	// apply (model-on-server + reachability/force) and which are deliberately bypassed
	// (provisioning gate, affinity, the maintenance-status exclusion).
	if req.ServerOverrideID != "" {
		return r.resolveServerOverride(ctx, req, apiFlavor, now)
	}
	// Vendor-account precedence (Milestone 3). The master flag + routing mode are
	// read through the injected accessors (nil-safe → OFF / vendor_first), and the
	// branch only ever matches a USER principal's OWN active account serving the
	// requested model (see resolveVendorAccount). A server-override request never
	// reaches here (handled above), so a vendor account can never shadow an
	// explicit override.
	//   - vendor_first: an own account wins before any self-hosted/shared route, so
	//     it is tried right after the override short-circuit.
	//   - fallback_only: self-hosted/shared wins; the account is tried only where
	//     the standard path has NO route (every ErrNoModelRoute exit — locked
	//     group, affinity, and fresh selection — funnels through the one check
	//     below, because resolveStandard returns ErrNoModelRoute at each of them).
	mode := r.vendorMode()
	if mode == vendorRoutingModeVendorFirst {
		if target, ok, err := r.resolveVendorAccount(ctx, token, req, apiFlavor); err != nil {
			return Target{}, err
		} else if ok {
			return target, nil
		}
	}
	target, err := r.resolveStandard(ctx, token, req, apiFlavor, now)
	if mode == vendorRoutingModeFallbackOnly && errors.Is(err, ErrNoModelRoute) {
		if vendorTarget, ok, verr := r.resolveVendorAccount(ctx, token, req, apiFlavor); verr != nil {
			return Target{}, verr
		} else if ok {
			return vendorTarget, nil
		}
	}
	return target, err
}

// resolveStandard is the self-hosted/shared resolution path: model-group dispatch,
// route affinity, then fresh candidate selection (ActiveMappingsForModel →
// filterProvisioned → filterServesEndpoint → filterCapable → the fail-open voice filter →
// expandInstances, on every admission turn → selectCandidate → targetFrom). It is
// everything Resolve did below the server-override short-circuit
// before the vendor-account branch was layered on top; extracting it verbatim lets
// Resolve wrap it with the vendor precedence without duplicating any of its
// ErrNoModelRoute exits. now and apiFlavor are passed in, computed once by Resolve.
func (r *Resolver) resolveStandard(ctx context.Context, token auth.Token, req inference.Request, apiFlavor string, now time.Time) (Target, error) {
	affinitySession := req.ClientSessionID
	if r.legacyAffinity.Load() {
		affinitySession = req.SessionID
	}
	key := AffinityKey{APITokenID: token.ID, Model: req.Model, APIFlavor: apiFlavor, SessionID: affinitySession}
	// Model-group dispatch. Gated on r.groups != nil so a resolver with no group
	// seam is byte-identical to today (the No-Op invariant): Group returns ok=false
	// for a plain model and DirectAllowed returns true (not locked), so the whole
	// block is transparent.
	if r.groups != nil {
		if members, policy, ok := r.groups.Group(req.Model); ok {
			if !r.groups.DirectAllowed(req.Model) {
				return Target{}, ErrNoModelRoute // a locked group: not directly requestable
			}
			return r.resolveGroup(ctx, groupResolve{
				token:     token,
				req:       req,
				key:       key,
				apiFlavor: apiFlavor,
				members:   members,
				policy:    policy,
				now:       now,
			})
		}
		if !r.groups.DirectAllowed(req.Model) {
			return Target{}, ErrNoModelRoute // a locked group-only model requested directly
		}
	}
	// A request that names a voice does not take a sticky pin: the pin was chosen
	// without regard to the voice, and following it would bypass the voice filter
	// below. No such pin can exist today -- both pin writers are guarded on the
	// capability list, a speech request always carries one, and AffinityKey's coarse
	// flavor keeps speech keys apart from every text endpoint's -- so this changes
	// nothing for any request that reaches here now; it keeps the voice contract
	// intact should that guard ever be lifted.
	keepPin := false
	// start is when this request's admission began: the queue deadline below counts from
	// it however long the pinned pass waited before handing the request on.
	start := admissionStart(req, now)
	if token.ID != "" && req.RequiredVoice == "" {
		target, ok, err := r.resolveAffinity(ctx, token, key, req, now)
		switch {
		case errors.Is(err, errPinnedInstancesGone):
			// Every instance on the pinned server is one this request already found gone:
			// the fresh selection below serves it, excluding those instances too, and
			// leaves the pin where it is -- an instance going away is no reason to move
			// the session off its server, and its next request goes back there.
			keepPin = true
		case err != nil:
			return Target{}, err
		case ok:
			// The clock read afresh, as refreshAffinity dates the pin: the pinned pass may
			// have parked, and the reservation lives from the time the request woke.
			r.reservation.touch(target.ServerID, affinityID(key), r.clock())
			return target, nil
		}
		// The pinned pass may have parked before it passed the pin over: the fresh
		// selection judges caps and session reservations, and dates the pin it writes, at
		// the time the request woke, not the time it parked.
		now = r.clock()
	}

	candidates, err := r.store.ActiveMappingsForModel(ctx, req.Model, apiFlavor)
	if err != nil {
		return Target{}, fmt.Errorf("resolve mappings: %w", err)
	}
	candidates, err = r.filterProvisioned(ctx, token, candidates)
	if err != nil {
		return Target{}, err
	}
	candidates = filterServesEndpoint(candidates, req.APIFlavor)
	// capableFrom is the pool size the capability filter is about to reduce, and
	// the sentinel below is conditioned on it being non-zero. Without that
	// measurement the sentinel is not a statement about capability at all:
	// filterCapable early-returns on empty input, so EVERY upstream reason the
	// pool is already empty -- no such model, a provisioning denial, an endpoint
	// the application does not serve -- would arrive at the check with
	// len(candidates) == 0 and be reported as "this model cannot do that" about a
	// model that may carry `image: yes`, or may not exist. It also made
	// ErrNoModelRoute unreachable on this path for any capability-carrying
	// request, collapsing the very distinction the sentinel exists to draw.
	//
	// Taken AFTER filterProvisioned/filterServesEndpoint deliberately: a
	// provisioning denial then reads ErrNoModelRoute, which is the same no-leak
	// posture (404, indistinguishable from an unknown model) the rest of the
	// codebase takes and the e2e suite pins for chat.
	capableFrom := len(candidates)
	candidates, err = r.filterCapable(ctx, candidates, req.RequiredCapabilities)
	if err != nil {
		return Target{}, err
	}
	// Checked BEFORE the general empty-candidates case below, so the more
	// specific fact wins: candidates existed for this model but none carried
	// the required capability, which is not the same fact as "no such
	// model". Conditioned on RequiredCapabilities so a chat request (nil)
	// falls through to the existing check unchanged.
	if capableFrom > 0 && len(candidates) == 0 && len(req.RequiredCapabilities) > 0 {
		return Target{}, ErrModelNotCapable
	}
	if len(candidates) == 0 {
		return Target{}, ErrNoModelRoute
	}
	// The voice filter (#251), fail-open: with a voice named AND at least one candidate
	// listing it in its cached voices, only the candidates that list it are selectable;
	// otherwise voice is nil and candidates are untouched. It runs on the pool the
	// capability gate left, AFTER the two empty-pool sentinels above, so those answers
	// are exactly what they were, and BEFORE the admission parameters below, so a
	// request queues on the voice's servers and not on servers it cannot use.
	voice, err := r.voiceScopeFor(ctx, req.RequiredVoice, candidates)
	if err != nil {
		return Target{}, err
	}
	candidates = voice.restrict(candidates)
	// The real-time gate of a voice chat's legs, a hard filter that never fails open:
	// after the voice, so a voice whose servers are all too slow is refused as too slow,
	// and before the admission parameters, so the request never queues on a server it
	// may not use.
	if candidates, err = r.filterRealtime(candidates, req); err != nil {
		return Target{}, err
	}

	// Admission-queue params (CP4): the servers to watch for a freeing slot, and the wait
	// timeout = the MAX admission_queue_timeout_seconds across the candidate apps (0 = at
	// least one app is willing to wait unboundedly => ctx-only wait). Computed once — the
	// model's mappings are stable across a short wait, and the instances each turn adds
	// below run on those same servers under those same applications, so they change
	// neither.
	serverIDs := make([]string, 0, len(candidates))
	seenServer := map[string]struct{}{}
	queueTimeoutSecs := 0
	for _, c := range candidates {
		if _, dup := seenServer[c.Server.ID]; !dup {
			seenServer[c.Server.ID] = struct{}{}
			serverIDs = append(serverIDs, c.Server.ID)
		}
		if c.Application.AdmissionQueueTimeoutSeconds > queueTimeoutSecs {
			queueTimeoutSecs = c.Application.AdmissionQueueTimeoutSeconds
		}
	}
	queueTimeout := time.Duration(queueTimeoutSecs) * time.Second
	// A bounded timeout is an absolute WALL-CLOCK deadline, NOT a fresh per-wait budget:
	// each WaitForSlot gets the REMAINING time. Otherwise the queue's internal liveness
	// re-check (which returns after a sub-second interval to force a cap re-read) would
	// re-arm the full timeout every iteration and a request under sustained saturation
	// would never hit its configured 503. queueTimeout<=0 stays unbounded (ctx-only wait).
	// It counts from the request's admission start (admissionStart): this resolve's own
	// start, or an earlier one the caller resolved for the same client call.
	var deadline time.Time
	if queueTimeout > 0 {
		deadline = start.Add(queueTimeout)
	}
	// clusterBound is the longest cluster park bound any turn of this request parked
	// under (cluster_gate.go).
	var clusterBound time.Duration

	for {
		// The mappings' running instances are read on EVERY turn, not once with the
		// mappings: a request parked below because every candidate was at capacity must
		// be offered, on its next turn, an instance that started while it waited. The
		// capacity filter in selectCandidate therefore judges this turn's instances.
		turn := expandInstances(ctx, candidates, r.instances)
		selected, ok, err := r.selectCandidate(ctx, turn, req, now)
		var ac *atCapacityError
		if neverParks(req) && errors.As(err, &ac) {
			selected, ok, err = r.selectWithoutParking(ctx, ac, req.Model, audioRequest(req), now)
			if d := ac.overflowDemand(selected); ok && d != nil {
				r.signalOverflow(req, *d)
			}
		}
		if errors.As(err, &ac) {
			// All candidates at cap + a wired controller: park until a slot frees, then retry.
			// A turn that parks on cold cluster heads waits for the cluster's own bound
			// (parkDeadline) instead of the queue's alone. The bound stays the request's
			// once it parked for a cluster: woken by the formation and finding the formed
			// head at its cap, it must not time out at once on a queue timeout it outlived
			// while the cluster formed.
			clusterBound = max(clusterBound, ac.clusterBound)
			turnDeadline := parkDeadline(start, deadline, clusterBound, len(ac.pool) == 0)
			if werr := r.parkTurn(ctx, turnDeadline, serverIDs, parkDemands(err)...); werr != nil {
				return Target{}, werr // ErrAdmissionQueueTimeout / ErrAdmissionQueueFull / ctx err
			}
			now = r.clock()
			continue
		}
		if err != nil {
			return Target{}, err
		}
		if !ok {
			// ErrVoiceUnavailable instead when the voice narrowed the pool: the servers
			// that list it are all unusable (see voiceScope.refine).
			return Target{}, voice.refine(ErrNoHealthyHost)
		}
		target, err := r.targetFrom(ctx, selected, apiFlavor)
		if err != nil {
			return Target{}, err
		}
		// The two PIN-CREATING writes -- this one and upsertGroupPin's -- are guarded
		// on the capability list rather than on a flavor string, so the speech and
		// multipart endpoints (#68/#69) inherit the guard. AffinityKey.APIFlavor is
		// COARSE (NormalizeAPIFlavor above), which is what the guard is for: a
		// capability-carrying request whose coarse flavor coincides with a text
		// endpoint's would, unguarded, write its pin under the key that endpoint's
		// client uses and repoint that client at a server chosen for a capability
		// it does not need. The read-side gate (resolveAffinity) does not help
		// there -- it is the WRITE that does the damage. For IMAGES the guard is
		// redundant: their coarse flavor is openai_images, their own, so an image
		// key never matches a chat client's. It stays for the endpoints above,
		// and because it keeps image traffic pin-free.
		//
		// This guard does NOT make every UpsertAffinity call site unreachable for a
		// capability-carrying request: resolveAffinity's own in-place refresh (the
		// LastUsedAt/UpdatedAt touch-up on a pin that already satisfies its
		// capability gate) still runs -- it is not guarded, and it does not need to
		// be. It rewrites the SAME row it just read, advancing only
		// LastUsedAt/UpdatedAt and recording the instance the request was served on;
		// ApplicationID/ServerID/ExpiresAt are unchanged, LastUsedAt is never read
		// for a routing decision anywhere in this backend, and the instance is one of
		// the pinned mapping's own on the pinned server, so that refresh cannot
		// repoint the client at another server or model. It just is not "guarded"
		// the way these two pin-creating writes are.
		if !keepPin && token.ID != "" && selected.Application.AffinityTTLSeconds > 0 && len(req.RequiredCapabilities) == 0 {
			if err := r.store.UpsertAffinity(ctx, RouteAffinity{
				ID:            affinityID(key),
				APITokenID:    token.ID,
				UserID:        token.UserID,
				Model:         req.Model,
				APIFlavor:     apiFlavor,
				SessionID:     key.SessionID,
				ApplicationID: selected.Application.ID,
				ServerID:      selected.Server.ID,
				Instance:      selected.Instance,
				ExpiresAt:     now.Add(time.Duration(selected.Application.AffinityTTLSeconds) * time.Second),
				LastUsedAt:    now,
				CreatedAt:     now,
				UpdatedAt:     now,
			}); err != nil {
				return Target{}, fmt.Errorf("store affinity: %w", err)
			}
			r.reservation.touch(selected.Server.ID, affinityID(key), now)
		}
		return target, nil
	}
}

// resolveVendorAccount resolves the request against the vendor accounts the
// PRINCIPAL may route to (Milestone 3, extended by shared vendor accounts, #183):
// its OWN accounts first, then the accounts another user shared into a resource
// group the principal is provisioned into (see VendorAccountEnumerator, the
// single routing-time grant). It returns (Target, true, nil) on a match,
// (Target{}, false, nil) for no match, and a non-nil error only on a store failure.
//
// It matches only when EVERY precondition holds, each a deliberate guard:
//   - r.vendorOn(): the vendor_accounts_enabled master flag is on. Off → the
//     whole branch is invisible (the no-op invariant).
//   - r.subscriptionsAllowed() (per account): the vendor_subscriptions_enabled
//     switch (default on) is on, or the account is an api_key one. Off → every
//     subscription account is skipped, exactly like a needs_reconnect one.
//   - the principal has an identity (vendorPrincipalEligible): a USER token, or a
//     SERVICE token carrying a service id. A service reaches a shared account only
//     when the grant says so (a direct service provision into the resource group);
//     it owns none of its own. A principal with neither identity is never asked
//     about, whatever the grant would answer.
//   - req.ServerOverrideID == "": a server-override request is a distinct routing
//     path (handled before this is ever called) and must never land on a vendor.
//     Re-checked here so the guarantee holds at this method's own boundary.
//   - len(req.RequiredCapabilities) == 0: capability-gated routing (vision/image
//     verdicts) is a self-hosted-mapping concern; a vendor Target carries no
//     per-capability verdicts, so such a request skips the branch entirely.
//   - apiFlavor != APIFlavorOpenAIImages: the images relay has no vendor path in
//     this milestone (the Target serves the text flavors via translate only).
//
// The matched Target is built BY HAND (not via targetFrom, which only knows
// self-hosted mappings): the sealed account API key is carried as APIToken (the
// cipher-holding gateway layer opens it exactly as for an app credential), the
// provider + endpoint + auth header are chosen from the account's vendor, and the
// effective flavors are [openai, anthropic] with the endpoint modes left zero
// (translate) so the native-passthrough layer translates either inbound dialect
// to the vendor's native wire format -- except an OpenAI account reached over the
// fine openai_responses flavor, whose ResponsesMode is passthrough (see
// vendorAccountTarget). Accounts are iterated in the order the enumerator
// returns them -- the principal's OWN accounts first (id order), then the shared
// ones (id order) -- and the FIRST active account with a model row whose
// token-effective public name (prefix + UpstreamModel, see vendorAccountModelMatch)
// equals the request model wins, so an own account wins a name collision with a
// shared one; a token only uses the accounts its VendorAccess policy allows (all:true
// covers the principal's own accounts, a shared account must be named, see
// TokenVendorPrefix).
// Dispatch trusts the Target's VendorAccountID and is owner-agnostic: the sealed
// credential opens only at the dispatch edge, so a borrower never sees it.
func (r *Resolver) resolveVendorAccount(ctx context.Context, token auth.Token, req inference.Request, apiFlavor string) (Target, bool, error) {
	if !r.vendorAccountRoutingEligible(token, req, apiFlavor) {
		return Target{}, false, nil
	}
	accounts, err := r.routableVendorAccounts(ctx, token)
	if err != nil {
		return Target{}, false, fmt.Errorf("resolve vendor accounts: %w", err)
	}
	for _, acc := range accounts {
		// Only an ACTIVE account serves; needs_reconnect (a dead refresh token) and
		// disabled accounts are skipped and fall through to the standard path.
		if acc.Status != VendorAccountStatusActive {
			continue
		}
		// The vendor_subscriptions_enabled switch: while it is off a SUBSCRIPTION
		// account does not serve (api_key accounts are untouched) and falls through
		// to the standard path like any other skipped account. The account is not
		// deleted, so switching the setting back on restores it. The model listing
		// applies the same skip (portal ownVendorAccountModels); the two must move
		// together or served_flavors parity breaks.
		if acc.AuthType == VendorAuthSubscription && !r.subscriptionsAllowed() {
			continue
		}
		// Per-token access (the opt-in): the grant above decided REACH; a token only
		// USES the accounts its VendorAccess policy covers, under the token-effective
		// prefix. all:true covers the principal's OWN accounts only; an account
		// shared with it is used only when the policy names it (per-account opt-in).
		// The zero policy denies every account, so a token that was never granted
		// vendor access resolves nothing here. TokenVendorPrefix is the same helper
		// the model listing uses, so advertised names equal routable names.
		prefix, allowed := TokenVendorPrefix(token, acc)
		if !allowed {
			continue
		}
		// An OpenAI SUBSCRIPTION account now serves BOTH openai inbound shapes
		// (Milestone 5c): a fine openai_responses request is forwarded LOSSLESSLY to
		// the ChatGPT backend via native passthrough (unchanged M5b behaviour), and a
		// chat-flavor request is TRANSLATED to the Responses protocol by
		// provider.OpenAIResponsesClient. vendorSubscriptionOpenAITarget reads
		// req.APIFlavor (the FINE flavor, not the coarse apiFlavor param, which folds
		// the two to "openai") to pick passthrough vs translate. It does NOT serve the
		// anthropic dialect (the ChatGPT backend speaks Responses only, and the target
		// carries [openai] only), so an anthropic_messages request to one is skipped
		// here and falls through to the standard path and ErrNoModelRoute — the
		// resolveVendorAccount branch bypasses filterServesEndpoint, so this flavor
		// guard is explicit. (Anthropic subscription and every api_key account serve
		// both dialects and are unaffected: translate, apart from an OpenAI api_key
		// account's openai_responses passthrough.)
		if acc.AuthType == VendorAuthSubscription && acc.Vendor == VendorOpenAI && apiFlavor != APIFlavorOpenAI {
			continue
		}
		models, err := r.store.VendorAccountModels(ctx, acc.ID)
		if err != nil {
			return Target{}, false, fmt.Errorf("resolve vendor account models: %w", err)
		}
		if t, ok := vendorAccountModelMatch(acc, models, prefix, req, apiFlavor); ok {
			return t, true, nil
		}
	}
	return Target{}, false, nil
}

// vendorAccountModelMatch returns the dispatch target for the FIRST model row whose
// token-effective public name equals the request model. The public name is
// reverse-mapped from the row: prefix + UpstreamModel, where prefix is what
// TokenVendorPrefix yields for this token and account (the account's own
// ModelPrefix for an all-access or no-override token, an override prefix -- possibly
// empty, i.e. the bare slug -- otherwise). Matching the stored GatewayModel instead
// would pin every token to the account's native prefix; for the native prefix the
// two are identical (GatewayModel == ModelPrefix + UpstreamModel). The dispatch
// target keeps req.Model as the public name and carries the raw UpstreamModel as
// the provider model. ok is false when no row matches, and also when the one match
// is a subscription account with an unknown vendor (vendorAccountModelTarget fails
// closed) — in both cases the caller falls through to the next account.
func vendorAccountModelMatch(acc VendorAccount, models []VendorAccountModel, prefix string, req inference.Request, apiFlavor string) (Target, bool) {
	for _, m := range models {
		if prefix+m.UpstreamModel != req.Model {
			continue
		}
		return vendorAccountModelTarget(acc, m, req, apiFlavor)
	}
	return Target{}, false
}

// vendorAccountRoutingEligible reports whether the request is one the vendor-account
// path may serve at all, before any store lookup. A request that overrides the
// server, demands capabilities, or is the images flavor (none of which a vendor
// account models) is left to the standard path.
func (r *Resolver) vendorAccountRoutingEligible(token auth.Token, req inference.Request, apiFlavor string) bool {
	return r.vendorOn() &&
		vendorPrincipalEligible(token) &&
		req.ServerOverrideID == "" &&
		len(req.RequiredCapabilities) == 0 &&
		apiFlavor != APIFlavorOpenAIImages
}

// vendorPrincipalEligible reports whether the token carries an identity the vendor
// grant can be asked about: a USER (UserID set) or a SERVICE token with a service
// id. It is deliberately NOT "UserID may be empty": a token with neither identity
// (or a service token whose id is blank) owns nothing, is provisioned nowhere, and
// must be denied before any enumerator -- which would have to answer for an empty
// principal -- is consulted.
func vendorPrincipalEligible(token auth.Token) bool {
	return token.UserID != "" || (token.IsService() && token.ServiceID != "")
}

// routableVendorAccounts lists the accounts the token's principal may route to,
// own accounts first. With an enumerator wired that is the grant
// (VendorAccountEnumerator). With NONE wired it is OWNER-ONLY: the principal's own
// accounts, exactly the behaviour before sharing existed -- never allow-all, the
// inverse of the nil ProvisioningGate, because an unwired grant must fail closed.
// A principal with no user identity (a service) owns nothing, so the fallback
// returns no accounts rather than enumerating by an empty owner id.
func (r *Resolver) routableVendorAccounts(ctx context.Context, token auth.Token) ([]VendorAccount, error) {
	if r.vendorEnumerator != nil {
		return r.vendorEnumerator.VendorAccountsForRouting(ctx, token)
	}
	if token.UserID == "" {
		return nil, nil
	}
	return r.store.VendorAccountsByOwner(ctx, token.UserID)
}

// vendorAccountModelTarget builds the dispatch target for a matched account+model
// row. The bool reports whether a target was built: it is false only for a
// subscription account whose vendor is unknown, which FAILS CLOSED (no target)
// rather than defaulting to either vendor's endpoint, which would misroute its
// sealed OAuth token. An api_key account builds a target, except a preset-vendor
// one (OpenAI-compatible or native Gemini) with no stored root URL, which fails
// closed the same way.
func vendorAccountModelTarget(acc VendorAccount, m VendorAccountModel, req inference.Request, apiFlavor string) (Target, bool) {
	if acc.AuthType != VendorAuthSubscription {
		if IsPresetVendor(acc.Vendor) && strings.TrimSpace(acc.BaseURL) == "" {
			// A misconfigured account (no root url) must not route to a broken
			// "/v1/chat/completions" with no host. Create-time validation
			// normally prevents this; fail closed so the request falls through.
			return Target{}, false
		}
		return vendorAccountTarget(acc, m, req.Model, apiFlavor, req.APIFlavor), true
	}
	// Subscription (OAuth): the bearer is resolved + refreshed at dispatch from the
	// account's sealed OAuth tokens, so the target carries VendorAccountID and the
	// vendor's required headers, and NO APIToken.
	switch acc.Vendor {
	case VendorAnthropic:
		// Anthropic: an inbound Messages request is relayed via native passthrough
		// (with the Claude-Code block injected), an openai_* one is translated to
		// Messages; either way the target carries the Claude-Code masquerade + the
		// OAuth version/beta headers. The FINE req.APIFlavor picks which (see
		// vendorSubscriptionAnthropicTarget).
		return vendorSubscriptionAnthropicTarget(acc, m, req.Model, apiFlavor, req.APIFlavor), true
	case VendorOpenAI:
		// OpenAI: native passthrough of an inbound Responses request, or translate of
		// a chat request, both to the ChatGPT backend. The FINE req.APIFlavor picks
		// which (see vendorSubscriptionOpenAITarget).
		return vendorSubscriptionOpenAITarget(acc, m, req.Model, apiFlavor, req.APIFlavor), true
	default:
		return Target{}, false
	}
}

// vendorAccountTarget assembles the Target for a matched vendor account + model
// row. Split out from resolveVendorAccount so the construction is testable on its
// own and so the three vendor kinds (OpenAI-compatible, native Anthropic, and
// native Gemini: vendor_gemini, x-goog-api-key auth, translate-only) read as
// one table. The provider/endpoint/auth-header triple is the only thing the
// account's vendor decides; everything else is the same for all of them, with ONE
// exception per vendor keyed on the FINE request flavor (fineFlavor, not the
// coarse apiFlavor, which folds every openai_* flavor to "openai"): an OpenAI
// account reached over openai_responses, and an Anthropic account reached over
// anthropic_messages, are each served by native passthrough (see the
// ResponsesMode / MessagesMode comments below).
func vendorAccountTarget(acc VendorAccount, m VendorAccountModel, model, apiFlavor, fineFlavor string) Target {
	provider := ProviderVendorOpenAI
	endpoint := "https://api.openai.com"
	tokenHeader := ""
	pathPrefix := "" // empty => /v1 (Target.OpenAIPathPrefix)
	switch {
	case acc.Vendor == VendorAnthropic:
		provider = ProviderVendorAnthropic
		endpoint = "https://api.anthropic.com"
		// The native Anthropic client authenticates with x-api-key, not the
		// Authorization: Bearer default the OpenAI-compatible client uses.
		tokenHeader = "x-api-key"
	case IsNativeGeminiVendor(acc.Vendor):
		// Native Google Gemini: the generateContent client against the
		// account's own root URL, authenticating with x-goog-api-key (not
		// the Bearer default). Translate-only (both endpoint modes stay zero).
		// Checked before the generic OpenAI-compatible case.
		provider = ProviderVendorGemini
		endpoint = acc.BaseURL
		tokenHeader = "x-goog-api-key"
		// pathPrefix stays "" — the client owns /v1beta/models/...:generateContent
	case IsOpenAICompatibleVendor(acc.Vendor):
		// x.ai / OpenRouter / Kilo / Google Gemini shim / custom: the shared
		// OpenAI-compatible client against the account's own root URL, Bearer
		// auth, translate-only (both endpoint modes stay zero below).
		endpoint = acc.BaseURL
		pathPrefix = OpenAIPathPrefixFor(acc.Vendor)
	}
	t := Target{
		RouteID:          vendorRoutePrefix + acc.ID + ":" + model,
		ServerID:         "",
		Provider:         provider,
		Endpoint:         endpoint,
		Model:            model,
		ProviderModel:    m.UpstreamModel,
		Timeout:          vendorAccountDefaultTimeout,
		APIFlavor:        apiFlavor,
		APIToken:         acc.APIKey, // still sealed; upstreamAuthCtx opens it, as for an app credential
		APITokenHeader:   tokenHeader,
		OpenAIPathPrefix: pathPrefix,
		// VendorAccountID names the serving account for USAGE ATTRIBUTION (the
		// recorded usage_events.account_id and the scraped rate-limit snapshot). It
		// is NOT the subscription-bearer trigger -- Subscription stays false, so
		// upstreamAuthCtx keeps using the sealed APIToken above, not an OAuth bearer.
		VendorAccountID: acc.ID,
		// Both inbound dialects are served; the zero endpoint modes mean translate,
		// so native-passthrough converts whichever one the caller used to the
		// vendor's native wire format. The two exceptions are set below: an OpenAI
		// account + openai_responses, and an Anthropic account + anthropic_messages.
		APIFlavors: []string{APIFlavorOpenAI, APIFlavorAnthropic},
	}
	// An inbound Responses request to an OpenAI api-key account is relayed LOSSLESSLY
	// to api.openai.com/v1/responses (native passthrough: only the model field is
	// rewritten) instead of being translated to /v1/chat/completions, which would
	// drop everything the chat shape cannot carry (tools, reasoning, previous
	// response, ...). Chat and every other openai flavor stay translated, and so
	// does the Anthropic api-key branch for every openai_* flavor (its upstream is
	// /v1/messages; there is no Responses surface to pass through to). Mirrors
	// vendorSubscriptionOpenAITarget.
	if acc.Vendor == VendorOpenAI && fineFlavor == inference.APIFlavorOpenAIResponses {
		t.ResponsesMode = EndpointModePassthrough
	}
	// The Anthropic mirror image: an inbound Messages request to an Anthropic
	// api-key account is relayed LOSSLESSLY to api.anthropic.com/v1/messages (native
	// passthrough: only the model field is rewritten, no masquerade block, no other
	// body edit) instead of being parsed to the neutral request and re-rendered, which
	// would drop everything the compat shape cannot carry (cache_control, thinking,
	// server tools, metadata, ...). Only the api-key branch is touched here: chat
	// and Responses requests to the same account, and anthropic_messages to an
	// OpenAI account, stay translated. The Anthropic SUBSCRIPTION target gets the
	// same passthrough mode for the same flavor in vendorSubscriptionAnthropicTarget
	// (with the Claude-Code block injected, which this api-key relay never is).
	//
	// Deliberately NO ExtraHeaders: the credential rides in APIToken as x-api-key
	// (upstreamAuthCtx opens it), and anthropic-version is guaranteed by
	// provider.AnthropicClient.ProxyNative itself, so the target stays minimal.
	if acc.Vendor == VendorAnthropic && fineFlavor == "anthropic_messages" {
		t.MessagesMode = EndpointModePassthrough
	}
	return t
}

// vendorSubscriptionAnthropicTarget assembles the Target for a matched ANTHROPIC
// SUBSCRIPTION (Claude Pro/Max OAuth) account + model row (Milestone 5a). It
// differs from the api_key vendorAccountTarget in exactly the subscription-path
// concerns:
//
//   - The bearer is NOT carried in APIToken. A subscription account holds OAuth
//     tokens that must be opened, refreshed when stale, and attached at dispatch
//     time; VendorAccountID names the account the dispatch layer (upstreamAuthCtx)
//     resolves the bearer from, and APIToken/APITokenHeader stay empty.
//   - Masquerade = claude_code makes the Anthropic client put the exact
//     Claude-Code system block first, which the OAuth Messages path requires:
//     prepended to the rendered request on the translate path, and injected into
//     the relayed body by AnthropicClient.ProxyNative on the passthrough path.
//   - ExtraHeaders carries the two headers the OAuth path needs on every call:
//     anthropic-version and the anthropic-beta oauth opt-in. (anthropic-version is
//     also set intrinsically by the client; repeating it here is harmless and
//     keeps the subscription requirement explicit in one place.) On the
//     passthrough path the client's own anthropic-beta is merged into the oauth
//     one per request (anthropicPassthroughHeaderOverrides); this static map is
//     never mutated.
//
// The FINE request flavor (fineFlavor, not the coarse apiFlavor, which folds every
// openai_* flavor to "openai") decides HOW that one upstream is reached, mirroring
// vendorSubscriptionOpenAITarget:
//
//   - fineFlavor == "anthropic_messages": MessagesMode passthrough, so the
//     native-passthrough layer relays the inbound Messages body/SSE to
//     /v1/messages instead of parsing it to the neutral request and re-rendering
//     it, which would drop everything the compat shape cannot carry (thinking,
//     cache_control, server tools, metadata, ...). The one body edit is the
//     Claude-Code block ProxyNative injects for the masquerade.
//   - any other flavor (openai chat/Responses): MessagesMode left zero ==
//     TRANSLATE, so dispatch renders the neutral request as a Messages body.
//
// Only an Anthropic subscription account reaches here (an OpenAI one builds
// vendorSubscriptionOpenAITarget instead). The values below are the live
// REVERSE-ENGINEERED Claude Code constants; see internal/vendorauth for their
// canonical home and the ToS caveat.
func vendorSubscriptionAnthropicTarget(acc VendorAccount, m VendorAccountModel, model, apiFlavor, fineFlavor string) Target {
	t := Target{
		RouteID:         vendorRoutePrefix + acc.ID + ":" + model,
		ServerID:        "",
		Provider:        ProviderVendorAnthropic,
		Endpoint:        "https://api.anthropic.com",
		Model:           model,
		ProviderModel:   m.UpstreamModel,
		Timeout:         vendorAccountDefaultTimeout,
		APIFlavor:       apiFlavor,
		APIToken:        "", // bearer resolved + refreshed at dispatch from OAuthTokens
		APITokenHeader:  "",
		VendorAccountID: acc.ID,
		Subscription:    true, // OAuth bearer resolved at dispatch -- the subscription-bearer trigger
		Masquerade:      MasqueradeClaudeCode,
		ExtraHeaders: map[string]string{
			"anthropic-version": "2023-06-01",
			"anthropic-beta":    "oauth-2025-04-20",
		},
		// Both inbound dialects are served; the zero endpoint modes mean translate.
		APIFlavors: []string{APIFlavorOpenAI, APIFlavorAnthropic},
	}
	// anthropic_messages is served via native passthrough; every openai_* flavor
	// is translated (MessagesMode left zero == translate).
	if fineFlavor == "anthropic_messages" {
		t.MessagesMode = EndpointModePassthrough
	}
	return t
}

// vendorSubscriptionOpenAITarget assembles the Target for a matched OPENAI
// SUBSCRIPTION (Codex ChatGPT Pro/Plus/Team OAuth) account + model row. The target
// always points at the ChatGPT backend and carries the subscription-bearer trigger;
// the FINE request flavor (fineFlavor) decides HOW that one upstream is reached:
//
//   - fineFlavor == "openai_responses": ResponsesMode passthrough, so the
//     native-passthrough layer relays the inbound Responses body/SSE VERBATIM
//     (lossless Codex passthrough — the Milestone 5b behaviour). The upstream path
//     is the bare /responses (not /v1/responses) — see endpointModeFor.
//   - any other openai flavor (chat/completions, portal chat): ResponsesMode left
//     zero == TRANSLATE, so dispatch calls provider.OpenAIResponsesClient's
//     Complete/CompleteStream, which render the request as a Responses body and
//     parse the Responses SSE back (Milestone 5c). The client POSTs the same bare
//     /responses path.
//
// Either way it differs from the Anthropic subscription target in these respects:
//
//   - Provider ProviderVendorOpenAISubscription (its own client, the ChatGPT
//     backend speaks Responses only — no /v1/chat/completions surface) + the
//     ChatGPT backend Endpoint (https://chatgpt.com/backend-api/codex).
//   - APIFlavors is [openai] ONLY: an OpenAI subscription account does NOT serve the
//     anthropic dialect (resolveVendorAccount's flavor guard enforces the same).
//   - NO Masquerade: the ChatGPT backend wants the real Codex body, not a system
//     block injected by a translate client.
//   - ExtraHeaders carries the two STATIC Codex headers every call needs. The
//     chatgpt-account-id header is NOT here — it is a per-account value resolved
//     from the sealed tokens at dispatch (subscriptionAuthCtx), the same place the
//     bearer is attached.
//
// As with the Anthropic target, VendorAccountID names the account the dispatch
// layer resolves + refreshes the bearer from, and APIToken/APITokenHeader stay
// empty. The header values below are the live REVERSE-ENGINEERED Codex CLI
// constants; see internal/vendorauth for their canonical home and the ToS caveat
// (routing cannot import vendorauth, so they are string literals here, mirroring
// the Anthropic target's approach).
func vendorSubscriptionOpenAITarget(acc VendorAccount, m VendorAccountModel, model, apiFlavor, fineFlavor string) Target {
	t := Target{
		RouteID:         vendorRoutePrefix + acc.ID + ":" + model,
		ServerID:        "",
		Provider:        ProviderVendorOpenAISubscription,
		Endpoint:        "https://chatgpt.com/backend-api/codex",
		Model:           model,
		ProviderModel:   m.UpstreamModel,
		Timeout:         vendorAccountDefaultTimeout,
		APIFlavor:       apiFlavor,
		APIToken:        "", // bearer resolved + refreshed at dispatch from OAuthTokens
		APITokenHeader:  "",
		VendorAccountID: acc.ID,
		Subscription:    true, // OAuth bearer resolved at dispatch -- the subscription-bearer trigger
		ExtraHeaders: map[string]string{
			"OpenAI-Beta": "responses=experimental",
			"originator":  "codex_cli_rs",
		},
		// The anthropic dialect is never served by an OpenAI subscription account.
		APIFlavors: []string{APIFlavorOpenAI},
	}
	// openai_responses is served LOSSLESSLY via native passthrough; every other
	// openai flavor is translated (ResponsesMode left zero == translate).
	if fineFlavor == inference.APIFlavorOpenAIResponses {
		t.ResponsesMode = EndpointModePassthrough
	}
	return t
}

// resolveServerOverride forces routing to exactly req.ServerOverrideID, bypassing the
// provisioning gate, affinity, and the maintenance-status exclusion (serverSelectable is
// deliberately NOT consulted here — a maintenance-status server IS routable via an
// override). The gateway handler has already re-authorized the principal for this
// specific server before calling Resolve; this method only performs the ROUTING checks:
// the requested model must actually be offered by that server (a live active mapping on
// an active application), and the server must be enabled + reachable unless the caller
// explicitly forces through an unhealthy/unreachable one. A requested voice
// (req.RequiredVoice) is deliberately not applied either: the override names ONE server,
// so there is nothing for the voice to choose between, and it does not veto the override.
// On that server the request is served by its mapping's instances as a pinned request is
// (selectPinned), preferring the instance it names (inference.Request.PreferredInstanceKey:
// a voice chat leg's, whose turns an overridden token routes here); a mapping without
// instances is served on its instance 0, as always. A request parked there for a free live
// slot has the server re-checked after every wait (overrideServes), and one that already
// found every instance there gone has no host left (ErrNoHealthyHost, the answer an
// unpinned request gets for it).
func (r *Resolver) resolveServerOverride(ctx context.Context, req inference.Request, apiFlavor string, now time.Time) (Target, error) {
	candidates, err := r.store.ActiveMappingsForModel(ctx, req.Model, apiFlavor)
	if err != nil {
		return Target{}, fmt.Errorf("resolve mappings: %w", err)
	}
	var mine []MappingCandidate
	for _, c := range candidates {
		if c.Server.ID == req.ServerOverrideID {
			mine = append(mine, c)
		}
	}
	mine = filterServesEndpoint(mine, req.APIFlavor)
	mine, err = r.filterCapable(ctx, mine, req.RequiredCapabilities)
	if err != nil {
		return Target{}, err
	}
	if len(mine) == 0 {
		return Target{}, ErrServerOverrideModelUnavailable
	}
	// Unlike the voice, the real-time gate does veto the override: it is a hard gate,
	// and a server measured too slow for a conversation is no better for being named.
	if mine, err = r.filterRealtime(mine, req); err != nil {
		return Target{}, err
	}
	// One server usually offers exactly one mapping for a given gateway model; pick the
	// first (deterministic: the store's ActiveMappingsForModel result is stably ordered).
	pinned := mine[0]
	if !r.overrideServes(pinned.Server, pinned.Application.ID, req) {
		return Target{}, ErrServerOverrideServerUnavailable
	}
	c, _, err := r.selectPinned(ctx, pinned, req, req.PreferredInstanceKey, now, r.overrideServesNow(pinned, req))
	if errors.Is(err, errPinnedInstancesGone) {
		return Target{}, ErrNoHealthyHost
	}
	if err != nil {
		return Target{}, err
	}
	return r.targetFrom(ctx, c, apiFlavor)
}

// overrideServesNow is the re-check selectPinned runs on an overridden request after
// every wait: it reloads pinned's server and answers ErrServerOverrideServerUnavailable
// when the server is gone or no longer passes overrideServes.
func (r *Resolver) overrideServesNow(pinned MappingCandidate, req inference.Request) func(context.Context) error {
	return func(ctx context.Context) error {
		server, err := r.store.AIServerByID(ctx, pinned.Server.ID)
		if errors.Is(err, storeerr.ErrNotFound) || (err == nil && !r.overrideServes(server, pinned.Application.ID, req)) {
			return ErrServerOverrideServerUnavailable
		}
		if err != nil {
			return fmt.Errorf("load override server: %w", err)
		}
		return nil
	}
}

// overrideServes reports whether a server override may route to server, whose
// application appID offers the requested model: the server is not disabled, and it is
// healthy and the application reachable unless req forces through an unhealthy or
// unreachable one (inference.Request.ServerOverrideForceUnreachable). Reachability
// mirrors the two signals serverSelectable + the affinity/selection path use (server
// health + application reachability), EXCEPT the maintenance-status exclusion baked into
// serverSelectable, which the override deliberately bypasses: ServerStatusMaintenance is
// neither ServerStatusDisabled nor unhealthy, so it is routed.
func (r *Resolver) overrideServes(server AIServer, appID string, req inference.Request) bool {
	if server.Status == ServerStatusDisabled {
		return false
	}
	reachable := server.HealthStatus != HealthUnhealthy
	if r.checker != nil {
		reachable = reachable && r.checker.Reachable(appID)
	}
	return reachable || req.ServerOverrideForceUnreachable
}

// filterProvisioned drops candidates whose server the principal may not use under
// resource-group provisioning. A nil gate or empty input is a no-op (unchanged) —
// the invariant that keeps every existing call site byte-identical when no
// ProvisioningGate is wired. A gate error is propagated to the caller (the
// concrete gate decides fail-open vs fail-closed per the enforcement mode).
func (r *Resolver) filterProvisioned(ctx context.Context, principal auth.Token, cands []MappingCandidate) ([]MappingCandidate, error) {
	if r.provisioning == nil || len(cands) == 0 {
		return cands, nil
	}
	ids := make([]string, 0, len(cands))
	seen := map[string]bool{}
	for _, c := range cands {
		if !seen[c.Server.ID] {
			seen[c.Server.ID] = true
			ids = append(ids, c.Server.ID)
		}
	}
	allowed, err := r.provisioning.AllowedServerIDs(ctx, principal, ids)
	if err != nil {
		return nil, err
	}
	out := cands[:0:0]
	for _, c := range cands {
		if allowed[c.Server.ID] {
			out = append(out, c)
		}
	}
	return out, nil
}

// filterCapable drops every candidate whose mapping does not satisfy each of
// the required capabilities (capabilityRowsSatisfy: a "yes" verdict, or for
// CapabilityImageEdit the derivation ImpliedImageEdit -- the one capability
// where an absent row is not "unknown", see there). It is this gateway's FIRST
// filter that genuinely excludes a model for lacking a capability --
// wantsLiveProgress annotates and the models-list fold advertises, but neither
// refuses, and the scorer (scorer.go) reads no verdict at all.
//
// An ABSENT row means unknown, not no, and this filter treats unknown as a
// refusal (image_edit is the documented exception: it is derived from image,
// so an absent image_edit row is not a refusal). That direction is chosen on
// evidence rather than taste: Extra-sourced rows are written yes-only
// (cmd/gateway/app_health.go) and the Ollama detector can structurally never
// emit "no", so treating unknown as permission would refuse nothing in a real
// fleet -- reproducing the exact defect the gate exists to fix. See ADR-042
// for the cost this accepts on day one.
//
// A store error REFUSES rather than failing open, for the same reason.
func (r *Resolver) filterCapable(ctx context.Context, cands []MappingCandidate, required []string) ([]MappingCandidate, error) {
	if len(required) == 0 || len(cands) == 0 {
		return cands, nil
	}
	ids := make([]string, 0, len(cands))
	seen := map[string]bool{}
	for _, c := range cands {
		if !seen[c.Mapping.ID] {
			seen[c.Mapping.ID] = true
			ids = append(ids, c.Mapping.ID)
		}
	}
	caps, err := r.store.MappingCapabilitiesForMappings(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("capability gate: %w", err)
	}
	out := cands[:0:0]
	for _, c := range cands {
		if capabilityRowsSatisfy(caps[c.Mapping.ID], required) {
			out = append(out, c)
		}
	}
	return out, nil
}

// capabilityRowsSatisfy reports whether rows satisfy every required
// capability. A name is satisfied by a literal "yes" verdict row, except
// CapabilityImageEdit, which is DERIVED (ImpliedImageEdit). Shared by
// filterCapable and resolveAffinity's own gate, which reads its rows from a
// different place; every satisfaction decision funnels through here, so the
// derivation has exactly one place to apply in routing.
func capabilityRowsSatisfy(rows []CapabilityRow, required []string) bool {
	byName := CapabilityRowsByName(rows)
	for _, name := range required {
		if name == CapabilityImageEdit {
			if !ImpliedImageEdit(rows) {
				return false
			}
			continue
		}
		// CapabilityAnyDecision is derived, never stored: the mapping answers
		// either decision endpoint (System One or OpenAI /v1/decisions). A
		// row of that name is inert, like an image_edit:yes row.
		if name == CapabilityAnyDecision {
			if byName[CapabilityDecision].Verdict != CapabilityYes && byName[CapabilityDecisionOpenAI].Verdict != CapabilityYes {
				return false
			}
			continue
		}
		if byName[name].Verdict != CapabilityYes {
			return false
		}
	}
	return true
}

// ImpliedImageEdit is THE image-edit derivation: a mapping may serve an image
// edit when it generates images (an image:yes row) AND its operator has not
// opted out (an image_edit:no row). It is the one function every reader of
// the edit capability calls -- the routing gate (capabilityRowsSatisfy), the
// portal's unknown-model redirect and its models-listing fold -- so the
// composer can never offer an edit the gate would refuse. Keep any change to
// the rule HERE; a second copy of it elsewhere is the defect this function
// exists to prevent (ADR-045: one function both read).
//
// "No image_edit row" means "derive from image", NOT "unknown": this is the
// one capability that does not follow ADR-042's refuse-on-unknown rule,
// deliberately, because no probe can produce an answer for it (see
// CapabilityImageEdit). An image_edit:yes row is inert -- it cannot make a
// mapping that does not generate images an edit target -- and the opt-out
// removes edit only: the mapping still serves generation.
//
// A nil or empty row set (a capability read that failed folds to nothing) is
// false, so every caller stays fail-closed.
func ImpliedImageEdit(rows []CapabilityRow) bool {
	byName := CapabilityRowsByName(rows)
	return byName[CapabilityImage].Verdict == CapabilityYes &&
		byName[CapabilityImageEdit].Verdict != CapabilityNo
}

// filterServesEndpoint drops candidates whose application does not serve the
// request's FINE api flavor once the per-endpoint mode is applied. It refines only
// the two coding-agent endpoints (openai_responses / anthropic_messages); for
// openai_chat_completions and any coarse flavor it is a no-op, so the coarse
// openai/anthropic gate the store already applied stands unchanged.
func filterServesEndpoint(cands []MappingCandidate, fineFlavor string) []MappingCandidate {
	if fineFlavor != inference.APIFlavorOpenAIResponses && fineFlavor != "anthropic_messages" {
		return cands
	}
	out := cands[:0:0]
	for _, c := range cands {
		if applicationServesEndpoint(c.Application, fineFlavor) {
			out = append(out, c)
		}
	}
	return out
}

func NormalizeAPIFlavor(apiFlavor string) string {
	normalized := strings.ToLower(strings.TrimSpace(apiFlavor))
	switch {
	// openai_decisions is the fine /v1/decisions flavor; like the images and
	// audio flavors it must be tested BEFORE the generic "openai" prefix.
	case normalized == APIFlavorOpenAIDecisions:
		return APIFlavorDecisions
	// BEFORE the generic "openai" prefix, deliberately: every fine images and
	// audio flavor also starts with "openai", so testing the generic prefix
	// first would fold images and speech into text and defeat the separation
	// entirely.
	case strings.HasPrefix(normalized, APIFlavorOpenAIImages):
		return APIFlavorOpenAIImages
	case strings.HasPrefix(normalized, APIFlavorOpenAIAudio):
		return APIFlavorOpenAIAudio
	case strings.HasPrefix(normalized, "openai"):
		return APIFlavorOpenAI
	case strings.HasPrefix(normalized, "anthropic"):
		return APIFlavorAnthropic
	default:
		return normalized
	}
}

// affinityApplicationStale reports whether a stored affinity's application is no
// longer a usable target for it — affinity disabled, moved to another server,
// inactive, or no longer serving the request's endpoint — in which case the
// caller drops the affinity. Whether it is reachable right now is one of the
// gates every candidate passes, and affinityServer asks those. Extracted from
// resolveAffinity so that method's cognitive complexity stays within budget.
func (r *Resolver) affinityApplicationStale(app Application, affinity RouteAffinity, fineFlavor string) bool {
	return app.AffinityTTLSeconds <= 0 ||
		app.ServerID != affinity.ServerID ||
		app.Status != ServerStatusActive ||
		!applicationServesEndpoint(app, fineFlavor)
}

// affinityServer loads the AI server a stored affinity's application app sits on
// and decides whether the pin may still be served there -- the server must
// exist, and the two must pass the gates every request's candidates pass first
// (CandidateServesNow: the server selectable, the application reachable, the
// server not held by a benchmark), so a pin is kept exactly where a fresh
// selection could go. Extracted from resolveAffinity, alongside
// affinityApplicationStale above, so that method's cognitive complexity stays
// within budget.
//
// The three-way result is what resolveAffinity's own three-way handling needs,
// and the split is deliberate: usable == false is a DROP -- the server is gone
// or fails a gate, and the caller deletes the affinity row -- while a non-nil
// error is a store failure that says nothing about the pin, so the caller
// surfaces it and leaves the row alone. A vanished server
// (storeerr.ErrNotFound) is a drop, not an error, for the same reason a
// vanished application is.
func (r *Resolver) affinityServer(ctx context.Context, app Application) (server AIServer, usable bool, err error) {
	server, err = r.store.AIServerByID(ctx, app.ServerID)
	if errors.Is(err, storeerr.ErrNotFound) {
		return AIServer{}, false, nil
	}
	if err != nil {
		return AIServer{}, false, fmt.Errorf("load affinity server: %w", err)
	}
	if !r.CandidateServesNow(MappingCandidate{Server: server, Application: app}) {
		return AIServer{}, false, nil
	}
	return server, true, nil
}

// affinityCapabilityVerdict performs resolveAffinity's ONE keyed capability read
// and answers the two different questions that read serves: the live-progress
// annotation for the synthetic candidate, and whether the pinned mapping still
// satisfies the capabilities this request requires.
//
// resolveAffinity's mapping comes from activeMappingForApplication
// (MappingsByApplication), NOT ActiveMappingsForModel -- it never joins
// model_mapping_capabilities, so unlike every other targetFrom call site this
// one has to fetch the verdict itself with a dedicated keyed read. That read is
// on a path that already makes four store calls just to reach this point
// (Affinity, ApplicationByID, AIServerByID, activeMappingForApplication's
// MappingsByApplication); one more keyed lookup is the cost of the pin no
// longer serving a stale verdict for its entire TTL. Both answers come off that
// single read: the capability gate costs no extra store call.
//
// The two answers read the SAME rows with deliberately different failure
// postures:
//
//   - LIVE PROGRESS is best-effort: a read failure degrades to ""
//     (never-determined) -- the same reading an absent capability row would
//     produce -- rather than failing an otherwise-servable affinity hit. The
//     cost is that a transient store error can make one pinned request look
//     like the verdict was never determined, which is strictly better than
//     serving the wrong (frozen, possibly stale) column value.
//   - The GATE treats a read error as not-satisfied. Live progress failing open
//     costs one annotation; this failing open would route an image request to a
//     chat model for the whole AffinityTTLSeconds.
//
// satisfied is true for a request that requires nothing (required empty), which
// is every chat request: the gate is skipped, not evaluated, so no verdict can
// refuse a pin that was never asked about a capability.
func (r *Resolver) affinityCapabilityVerdict(ctx context.Context, mappingID string, required []string) (liveProgressSupport string, satisfied bool) {
	caps, capErr := r.store.MappingCapabilities(ctx, mappingID)
	if capErr == nil {
		if row, ok := CapabilityRowsByName(caps)[CapabilityLiveProgress]; ok {
			liveProgressSupport = LiveProgressSupportFromVerdict(row.Verdict)
		}
	}
	if len(required) > 0 && (capErr != nil || !capabilityRowsSatisfy(caps, required)) {
		return liveProgressSupport, false
	}
	return liveProgressSupport, true
}

// resolveAffinity serves req from token's pin under key when the pin is still usable:
// (target, true, nil), or (Target{}, false, nil) to let the caller select afresh -- with
// the pin deleted when it went stale, and kept when only this request cannot use it. On
// the pinned server the request is served by the pinned mapping's instances
// (selectPinned), preferring the instance the pin names, and the pin is refreshed with
// the instance chosen (refreshAffinity), so the next request follows it there. A request
// that already found every instance there gone gets errPinnedInstancesGone, and the
// caller selects afresh without repointing the pin.
//
// The provisioning check runs before any instance is chosen and before the refresh, for
// every mapping, with instances or without: a pin to a server no longer provisioned for
// the principal is neither served nor touched (its LastUsedAt/UpdatedAt stay as they
// were), and the request never waits for an instance there.
func (r *Resolver) resolveAffinity(ctx context.Context, token auth.Token, key AffinityKey, req inference.Request, now time.Time) (Target, bool, error) {
	affinity, app, ok, err := r.loadAffinity(ctx, key, req.APIFlavor, now)
	if err != nil || !ok {
		return Target{}, false, err
	}
	server, usable, err := r.affinityServer(ctx, app)
	if err != nil {
		return Target{}, false, err
	}
	if !usable {
		_ = r.store.DeleteAffinity(ctx, key)
		return Target{}, false, nil
	}
	mapping, ok, err := r.activeMappingForApplication(ctx, app.ID, key.Model)
	if err != nil {
		return Target{}, false, err
	}
	if !ok {
		_ = r.store.DeleteAffinity(ctx, key)
		return Target{}, false, nil
	}
	// A pin to a cluster head that has not formed is passed over and kept: the fresh
	// selection parks the request for the cluster (or excludes the head), which a pinned
	// request, served at once on its pinned server, would not.
	if v := r.clusterVerdict(ctx, MappingCandidate{Server: server, Application: app, Mapping: mapping}); v.State != ClusterGateNone && v.State != ClusterGateFormed {
		return Target{}, false, nil
	}
	// One keyed capability read serves both the live-progress annotation and the
	// capability gate on the pinned mapping; the read, and the different failure
	// posture each answer takes, are documented on affinityCapabilityVerdict.
	// This is the one Resolve branch with no candidate filter at all -- its
	// mapping comes from MappingsByApplication, which never joins
	// model_mapping_capabilities -- so the gate has to live here.
	//
	// The refusal is NON-DESTRUCTIVE: it returns (Target{}, false, nil) and lets
	// the caller fall through to the fresh-candidate path. Every other rejection
	// in this function deletes the affinity row, and that would be wrong here:
	// AffinityKey.APIFlavor is COARSE, so under a key a capability-carrying
	// request shares with a text endpoint, its refusal says nothing about the
	// pin, and deleting would evict that endpoint's working pin. For the same
	// reason the gate is not in affinityApplicationStale. For IMAGES this is
	// redundant: their coarse flavor is openai_images, so an image key never
	// matches a chat client's, and since no capability-carrying request writes a
	// pin (Resolve, upsertGroupPin) there is no image pin for the gate to refuse.
	// It stays for a future capability-carrying endpoint that does share a key.
	//
	// Placed BEFORE the LastUsedAt/UpdatedAt refresh below so a REFUSED request
	// never touches the affinity row at all. This ordering does NOT make the
	// refresh below unreachable in general: for a capability-carrying request
	// whose pin already satisfies the gate, the refusal above is skipped and the
	// refresh runs exactly as it would for a chat request. That is safe -- it
	// rewrites the SAME row it just read, advancing only LastUsedAt/UpdatedAt and
	// the instance served (ApplicationID/ServerID/ExpiresAt untouched, LastUsedAt
	// never read for a routing decision anywhere in this backend, and the instance
	// one of the pinned mapping's own on the pinned server) -- so unlike the two
	// PIN-CREATING writes in Resolve and upsertGroupPin, this one is not guarded
	// on the capability list, and does not need to be.
	liveProgressSupport, capable := r.affinityCapabilityVerdict(ctx, mapping.ID, req.RequiredCapabilities)
	if !capable {
		return Target{}, false, nil
	}
	provisioned, err := r.serverProvisioned(ctx, token, server.ID)
	if err != nil {
		return Target{}, false, err
	}
	if !provisioned {
		// The pinned server is no longer provisioned for this principal: ignore the
		// pin (keep it, do not serve it) and let the caller select afresh, which
		// itself re-applies filterProvisioned so a blocked server is excluded there
		// too. Checked before an instance is chosen, so such a request never waits
		// for an instance there.
		return Target{}, false, nil
	}
	// A request parked for a slot re-checks the pinned server after every wait, with the
	// same gates as above (affinityServesNow).
	sel, parked, err := r.selectPinned(ctx, MappingCandidate{
		Server:              server,
		Application:         app,
		Mapping:             mapping,
		LiveProgressSupport: liveProgressSupport,
	}, req, InstanceKey(mapping.ID, affinity.Instance), now, r.affinityServesNow(app))
	if errors.Is(err, errPinServerUnusable) {
		// The pinned server stopped passing its gates while the request waited: pass the
		// pin over to the fresh selection, which leaves that server out as well. The next
		// request's own checks above drop the pin if the server is still unusable then.
		return Target{}, false, nil
	}
	if err != nil {
		return Target{}, false, err // errPinnedInstancesGone among them, for the caller
	}
	if err := r.refreshAffinity(ctx, key, affinity, sel.Instance, parked, now); err != nil {
		return Target{}, false, err
	}
	target, err := r.targetFrom(ctx, sel, key.APIFlavor)
	if err != nil {
		return Target{}, false, err
	}
	return target, true, nil
}

// loadAffinity reads the pin under key together with its application, for a request of
// the fine flavor fineFlavor. ok is false when there is no pin, and when the pin went
// stale and was deleted: it expired, its application is gone, or the application no
// longer serves it (affinityApplicationStale).
func (r *Resolver) loadAffinity(ctx context.Context, key AffinityKey, fineFlavor string, now time.Time) (affinity RouteAffinity, app Application, ok bool, err error) {
	affinity, ok, err = r.store.Affinity(ctx, key)
	if err != nil {
		return RouteAffinity{}, Application{}, false, fmt.Errorf("lookup affinity: %w", err)
	}
	if !ok {
		return RouteAffinity{}, Application{}, false, nil
	}
	if !affinity.ExpiresAt.After(now) {
		_ = r.store.DeleteAffinity(ctx, key)
		return RouteAffinity{}, Application{}, false, nil
	}
	app, err = r.store.ApplicationByID(ctx, affinity.ApplicationID)
	if errors.Is(err, storeerr.ErrNotFound) {
		_ = r.store.DeleteAffinity(ctx, key)
		return RouteAffinity{}, Application{}, false, nil
	}
	if err != nil {
		return RouteAffinity{}, Application{}, false, fmt.Errorf("load affinity application: %w", err)
	}
	if r.affinityApplicationStale(app, affinity, fineFlavor) {
		_ = r.store.DeleteAffinity(ctx, key)
		return RouteAffinity{}, Application{}, false, nil
	}
	return affinity, app, true, nil
}

// serverProvisioned reports whether resource-group provisioning lets principal use
// serverID; without a provisioning gate every server is provisioned.
func (r *Resolver) serverProvisioned(ctx context.Context, principal auth.Token, serverID string) (bool, error) {
	if r.provisioning == nil {
		return true, nil
	}
	allowed, err := r.provisioning.AllowedServerIDs(ctx, principal, []string{serverID})
	if err != nil {
		return false, err
	}
	return allowed[serverID], nil
}

// affinityServesNow is the re-check selectPinned runs on a request parked for a slot on
// app's pinned server, after every wait: the gates resolveAffinity applied first
// (affinityServer), answering errPinServerUnusable when the server no longer passes them.
func (r *Resolver) affinityServesNow(app Application) func(context.Context) error {
	return func(ctx context.Context) error {
		_, usable, err := r.affinityServer(ctx, app)
		if err == nil && !usable {
			return errPinServerUnusable
		}
		return err
	}
}

// refreshAffinity records on affinity -- key's pin, as resolveAffinity read it -- the
// instance the request is served on and when it was last used. A request that waited for
// its slot (parked) re-reads the row first and refreshes it only while it is still the
// pin it was served from, on the same application and server: a pin another request
// repointed, or one that was deleted, while it waited is left as it is now, never
// overwritten or recreated.
func (r *Resolver) refreshAffinity(ctx context.Context, key AffinityKey, affinity RouteAffinity, instance int, parked bool, now time.Time) error {
	if parked {
		cur, ok, err := r.store.Affinity(ctx, key)
		if err != nil {
			return fmt.Errorf("lookup affinity: %w", err)
		}
		if !ok || cur.ApplicationID != affinity.ApplicationID || cur.ServerID != affinity.ServerID {
			return nil
		}
		affinity, now = cur, r.clock()
	}
	affinity.Instance = instance
	affinity.LastUsedAt = now
	affinity.UpdatedAt = now
	if err := r.store.UpsertAffinity(ctx, affinity); err != nil {
		return fmt.Errorf("update affinity: %w", err)
	}
	return nil
}

// selectPinned chooses the instance a pinned request -- a token's affinity hit, or a
// server override -- is served by: one of pinned's mapping's instances on pinned's server,
// and never a candidate anywhere else, since the pin names that server. Every other gate
// is the caller's: the pin's own checks (resolveAffinity), or the override's model,
// reachability and real-time checks (resolveServerOverride), applied before the first
// turn and, through servesNow, again after every wait -- so a parked request does not go
// on waiting for, or get served on, a server that stopped passing them; servesNow's error
// is then the answer. parked reports whether the request waited at all.
//
// A mapping without instances there is served as pinned, its instance 0, exactly as
// before instances existed. Otherwise the instances are read afresh on every turn
// (expandInstances), so a pinned instance that is gone is simply not a candidate, and one
// the request already found gone (inference.Request.ExcludeInstanceKeys) is passed over;
// with every instance there passed over the answer is errPinnedInstancesGone.
//
//  1. preferred -- the instance the pin names, or a server override's preferred one --
//     when it is a candidate under its cap (underCap; a held live slot counts for a
//     request that needs a free one, needsFreeLiveSlot);
//  2. else the least loaded instance under its cap (pinnedChoice);
//  3. else, every instance at its cap, the request is served at once -- a pinned request
//     has never queued, and its instance takes it in its own queue -- on preferred, or the
//     least loaded instance when preferred is no candidate, among the instances whose
//     live slot is free when it needs one (liveSlotFree). Only a request that cannot be
//     served that way waits: a live session, which takes a slot no other session may
//     share, and a transcription when every live slot on the server is held, since the
//     upstream serves no transcription while a live session holds its slot. It parks for
//     a slot on this server alone (parkTurn), as an unpinned request parks, and is offered
//     every turn's instances again -- unless it must not park (inference.Request.NoPark,
//     CheckOnly) or has no queue to park in, and is then served at once like any other.
//     Either way the request asks the demand sink for another instance on this server
//     (DemandSignal.ServerID): once when served at once (signalOverflow, whether or not a
//     queue is wired, since a pin is served instead of queued), once per park turn when
//     parked (parkTurn) -- and a check-only resolve signals nothing.
//
// The pin itself is never the selection's to delete: whichever instance serves, the
// request stays on the pinned server.
func (r *Resolver) selectPinned(ctx context.Context, pinned MappingCandidate, req inference.Request, preferred string, now time.Time, servesNow func(context.Context) error) (sel MappingCandidate, parked bool, err error) {
	var deadline time.Time
	if secs := pinned.Application.AdmissionQueueTimeoutSeconds; secs > 0 {
		deadline = admissionStart(req, now).Add(time.Duration(secs) * time.Second)
	}
	judgeLive := r.liveSlots != nil && needsFreeLiveSlot(req)
	for {
		turn := expandInstances(ctx, []MappingCandidate{pinned}, r.instances)
		if !turn[0].InstanceCapable {
			return pinned, parked, nil
		}
		pool := slices.DeleteFunc(turn, func(c MappingCandidate) bool {
			return slices.Contains(req.ExcludeInstanceKeys, InstanceKey(c.Mapping.ID, c.Instance))
		})
		if len(pool) == 0 {
			return MappingCandidate{}, parked, errPinnedInstancesGone
		}
		sel, d, served := r.pinnedTurn(pool, req, preferred, now, judgeLive, pinned.Server.ID)
		if served {
			return sel, parked, nil
		}
		if err := r.parkTurn(ctx, deadline, []string{pinned.Server.ID}, *d); err != nil {
			return MappingCandidate{}, parked, err
		}
		parked = true
		if err := servesNow(ctx); err != nil {
			return MappingCandidate{}, parked, err
		}
		now = r.clock()
	}
}

// pinnedTurn is one turn of selectPinned over pool, the pinned request's instance
// candidates on serverID that are not passed over. served reports that the request is
// served at once by sel (steps 1 to 3, signalling overflow demand when every instance is
// at its cap); otherwise it parks for a slot on serverID, carrying demand.
func (r *Resolver) pinnedTurn(pool []MappingCandidate, req inference.Request, preferred string, now time.Time, judgeLive bool, serverID string) (sel MappingCandidate, demand *DemandSignal, served bool) {
	if free := slices.DeleteFunc(slices.Clone(pool), func(c MappingCandidate) bool { return !r.underCap(c, now, judgeLive) }); len(free) > 0 {
		return r.pinnedChoice(free, preferred), nil, true
	}
	slotFree := r.liveSlotFree(pool, judgeLive)
	d := instanceDemand(pool, req)
	d.ServerID = serverID
	waits := req.LiveSession || len(slotFree) == 0
	if !waits || neverParks(req) || r.admission == nil {
		if len(slotFree) == 0 {
			slotFree = pool
		}
		r.signalOverflow(req, *d)
		return r.pinnedChoice(slotFree, preferred), nil, true
	}
	return MappingCandidate{}, d, false
}

// liveSlotFree is the candidates of pool whose live slot is free when judgeLive says the
// slots count (a request that needs a free one, with a live-slot source wired), and pool
// itself otherwise.
func (r *Resolver) liveSlotFree(pool []MappingCandidate, judgeLive bool) []MappingCandidate {
	if !judgeLive {
		return pool
	}
	return slices.DeleteFunc(slices.Clone(pool), func(c MappingCandidate) bool {
		return r.liveSlots.LiveHeld(InstanceKey(c.Mapping.ID, c.Instance))
	})
}

// parkTurn is one turn of a request parked in the admission queue, the same for
// resolveStandard, resolveGroupOnce and selectPinned: with a bounded deadline already
// passed it answers ErrAdmissionQueueTimeout at once; otherwise it hands the sink the
// demands the request carries (signalDemand) and waits on servers for the time left --
// until ctx ends when deadline is zero (no bound). The deadline is absolute, never a fresh
// budget per turn, so the queue's own liveness re-check cannot re-arm it. nil means a slot
// may have freed and the caller selects again; any other answer is the request's
// (ErrAdmissionQueueTimeout, ErrAdmissionQueueFull, or ctx's error).
func (r *Resolver) parkTurn(ctx context.Context, deadline time.Time, servers []string, demands ...DemandSignal) error {
	wait := time.Duration(0)
	if !deadline.IsZero() {
		if wait = deadline.Sub(r.clock()); wait <= 0 {
			return ErrAdmissionQueueTimeout
		}
	}
	r.signalDemand(demands...)
	return r.admission.WaitForSlot(ctx, servers, wait)
}

// pinnedChoice is the instance of pool, a pinned request's instance candidates on its
// server, that the request is served by: the preferred one when pool holds it, else the
// least loaded one (leastLoadedInstances) -- among equally loaded ones the first whose
// model is loaded, so an idle instance 0 that was unloaded does not cold-start while a
// running sibling idles -- marked as selected from an instance pool.
func (r *Resolver) pinnedChoice(pool []MappingCandidate, preferred string) MappingCandidate {
	i := slices.IndexFunc(pool, func(c MappingCandidate) bool { return InstanceKey(c.Mapping.ID, c.Instance) == preferred })
	var sel MappingCandidate
	if i >= 0 {
		sel = pool[i]
	} else {
		least := leastLoadedInstances(pool, r.instanceActivity)
		sel = least[0]
		if r.loaded != nil {
			if j := slices.IndexFunc(least, func(c MappingCandidate) bool { return modelLoadedOn(r.loaded, c) }); j >= 0 {
				sel = least[j]
			}
		}
	}
	sel.InstancePool = true
	return sel
}

// estInputTokens is a deliberately LENIENT estimate of a request's prompt token count
// (word count via strings.Fields over each message's text, times ~1.3 tokens/word); it
// under-counts dense/code/CJK text, which is the correct-by-design direction for a
// filter that should reject ONLY what clearly cannot fit and otherwise fails open in
// selectCandidate. Images/tool JSON are ignored (Text() is text-only).
func estInputTokens(req inference.Request) int {
	words := 0
	for _, msg := range req.Messages {
		words += len(strings.Fields(msg.Text()))
	}
	return int(float64(words)*1.3) + 1
}

// requestFitsContext reports whether a request needing `need` tokens fits a mapping
// whose context window is `contextSize`. An unknown context (<= 0) always "fits".
func requestFitsContext(contextSize, need int) bool {
	return contextSize <= 0 || need <= contextSize
}

func (r *Resolver) selectCandidate(ctx context.Context, candidates []MappingCandidate, req inference.Request, now time.Time) (MappingCandidate, bool, error) {
	model := req.Model
	// A pathological or overflowing MaxTokens that drives need non-positive simply
	// disables the filter (every mapping "fits", so the pool is unchanged) — fail-safe,
	// route anyway; no clamp is needed.
	need := estInputTokens(req) + req.MaxTokens

	// Pass 1: the reachable/selectable/non-busy candidates (CandidateServesNow, the one
	// statement of these gates), less any instance the request already tried and found
	// gone (ExcludeInstanceKeys) -- a hard filter on every admission turn, so a parked
	// request never comes back to it -- and less every cluster head that has not formed
	// (cluster_gate.go): a cold or forming one is set aside to park on, an excluded one
	// dropped.
	var split clusterSplit
	who := clusterDemandOf{apiFlavor: NormalizeAPIFlavor(req.APIFlavor), live: req.LiveSession}
	reachable := make([]MappingCandidate, 0, len(candidates))
	for _, c := range candidates {
		if len(req.ExcludeInstanceKeys) > 0 && slices.Contains(req.ExcludeInstanceKeys, InstanceKey(c.Mapping.ID, c.Instance)) {
			continue
		}
		if !r.CandidateServesNow(c) {
			continue
		}
		if !split.gate(r.clusterVerdict(ctx, c), c, who) {
			continue
		}
		reachable = append(reachable, c)
	}
	if len(reachable) == 0 {
		return r.selectClusterOnly(ctx, split, req, now)
	}

	// Context-fit hard filter, with fail-open: keep candidates whose known context
	// window can hold the request; if that empties the set, fall back to all reachable
	// (better to answer than to refuse) and warn.
	pool := make([]MappingCandidate, 0, len(reachable))
	for _, c := range reachable {
		if requestFitsContext(c.Mapping.ContextSize, need) {
			pool = append(pool, c)
		}
	}
	if len(pool) == 0 {
		slog.Warn("context-fit filter empty; routing without it", "model", model, "need_tokens", need)
		pool = reachable
	}

	// Whether the pool holds an instance candidate decides how live slots are judged
	// (selectUnderCap) and is carried on the selection (Target.InstancePool).
	instancePool := slices.ContainsFunc(pool, func(c MappingCandidate) bool { return c.InstanceCapable })
	sel, ok, err := r.selectUnderCap(ctx, pool, req, now, instancePool)
	if ok {
		sel.InstancePool = instancePool
	}
	// Every candidate that may be dispatched is at its cap: the request parks on them and
	// on the cold cluster heads alike, and a cold head's demand starts its cluster.
	var ac *atCapacityError
	if errors.As(err, &ac) {
		return sel, ok, ac.withCluster(split)
	}
	if !ok && err == nil && len(split.parked) > 0 {
		return r.selectClusterOnly(ctx, split, req, now)
	}
	return sel, ok, err
}

// selectClusterOnly is selectCandidate's answer when no candidate that may be
// dispatched is left. Cold or forming cluster heads park the request (an
// all-at-capacity error carrying them); without a queue to park in, a head is
// served at once -- its cluster is signalled to start, and the gateway retries a
// request the head refuses as not ready once it formed. With no parkable head, an
// excluded one answers the cluster's code; with none at all there is no candidate.
func (r *Resolver) selectClusterOnly(ctx context.Context, split clusterSplit, req inference.Request, now time.Time) (MappingCandidate, bool, error) {
	if len(split.parked) == 0 {
		if split.excluded != nil {
			return MappingCandidate{}, false, split.excluded
		}
		return MappingCandidate{}, false, nil
	}
	ac := (&atCapacityError{}).withCluster(split)
	if r.admission != nil {
		return MappingCandidate{}, false, ac
	}
	sel, ok, err := r.selectWithoutParking(ctx, ac, req.Model, audioRequest(req), now)
	if d := ac.overflowDemand(sel); ok && d != nil {
		r.signalOverflow(req, *d)
	}
	return sel, ok, err
}

// selectUnderCap is selectCandidate's last step: it selects from pool, the candidates
// that passed every gate, preferring the ones under their cap. instancePool says pool
// holds an instance candidate.
func (r *Resolver) selectUnderCap(ctx context.Context, pool []MappingCandidate, req inference.Request, now time.Time, instancePool bool) (MappingCandidate, bool, error) {
	model, audio := req.Model, audioRequest(req)
	// Capacity cap, CP3/CP4: PREFER the candidates under their cap (underCap); when none
	// is, queue the request (CP4) if an admission controller is wired. The caps:
	//
	//   - A candidate of a mapping without instances keeps the reservation-aware server
	//     cap: in-flight on its server below effectiveCap = max_concurrency −
	//     activeReservedSessions(server). It is defined only with a known max_concurrency
	//     (>0) AND a live server activity signal (r.activity != nil); otherwise such a
	//     candidate is never capped (today's behaviour).
	//   - An instance candidate -- every candidate of a mapping expandInstances found
	//     instance-capable, instance 0 included -- is judged by its OWN in-flight count
	//     against the spec's instance parallelism (always at least 1), read from
	//     r.instanceActivity: its instances share the server, so the server's count cannot
	//     say whether this one instance takes another request. Without that source an
	//     instance is never capped.
	//   - In a pool that holds an instance candidate, a request that needs a live slot
	//     free -- a live session, and every transcription (needsFreeLiveSlot) -- also
	//     finds every candidate at its cap while its one live slot is held (r.liveSlots):
	//     an instance, and a mapping without instances beside it alike, so the request
	//     goes to a free slot or parks for one (and signals demand for another
	//     instance). A pool without an instance candidate is not judged by its slots:
	//     a live session there takes its one slot or is refused busy at once by its own
	//     acquire, and a buffered transcription by its held check, as before.
	//
	// The block runs when any of those sources is wired, and is a no-op otherwise.
	// A pinned request never reaches here: it chooses among its pinned server's
	// instances itself (selectPinned) and returns first. When the
	// under-cap subset is empty the request queues if an admission controller is wired --
	// with an instance among the candidates it does so even though no mapping sets a
	// max_concurrency, and carries the demand it signals once it actually parks
	// (atCapacityError, signalDemand), with the capped pool a request that must not park
	// is served from (selectWithoutParking) -- and fails open onto the full pool otherwise.
	// When the subset is non-empty but yields no Score-viable pick, it also fails open
	// rather than refuse a request an at-capacity-but-viable server could still serve.
	// The emptiness-only variant was a verification-caught MAJOR: it could strip the sole
	// viable-but-at-cap candidate and leave only under-cap non-viable ones →
	// ErrNoHealthyHost. So with every cap undefined (the under-cap subset == the full
	// pool) this is exactly the pre-CP3 behaviour.
	judgeLive := instancePool && r.liveSlots != nil && needsFreeLiveSlot(req)
	preferred := r.preferredInstance(pool, req, now, judgeLive)
	if r.activity != nil || r.instanceActivity != nil || judgeLive {
		capped := r.underCapPool(pool, now, judgeLive)
		if len(capped) == 0 {
			if r.admission != nil {
				// Every candidate is at its effective cap: let Resolve queue + retry (CP4)
				// instead of failing open. Pinned requests never reach here (selectPinned
				// serves them earlier, and queues only a pinned request that needs a free
				// live slot, on its pinned server). A nil controller keeps the CP3
				// fail-open below.
				return MappingCandidate{}, false, &atCapacityError{demand: instanceDemand(pool, req), pool: pool, instancePool: instancePool}
			}
			slog.Warn("capacity-cap filter empty; routing without it (all candidates at capacity)", "model", model)
		} else if len(capped) < len(pool) {
			if sel, ok, err := r.selectWithSwapProtection(ctx, capped, model, audio, now, preferred); err != nil || ok {
				return sel, ok, err
			}
			slog.Warn("capacity-cap survivors non-viable; routing without it", "model", model)
		}
	}
	return r.selectWithSwapProtection(ctx, pool, model, audio, now, preferred)
}

// underCapPool is the candidates of pool under their cap (underCap), in pool's order.
func (r *Resolver) underCapPool(pool []MappingCandidate, now time.Time, judgeLive bool) []MappingCandidate {
	capped := make([]MappingCandidate, 0, len(pool))
	for _, c := range pool {
		if r.underCap(c, now, judgeLive) {
			capped = append(capped, c)
		}
	}
	return capped
}

// preferredInstance is the gateway instance key req prefers
// (inference.Request.PreferredInstanceKey) when the selection may honour it, "" otherwise.
// It may when the key names an instance candidate of pool -- an instance of a replicable
// spec that passed every filter this request's candidates pass, the real-time gate and an
// instance excluded as gone among them -- that is under its cap (underCap, a held live
// slot included when judgeLive says slots count). A mapping without instances is never
// preferred, so a spec that did not opt in is routed exactly as without a preference.
// The key only takes the place of the score's ranking (preferScored): the cap, the swap
// protection and the loaded and starting partitions still decide which candidates the
// score ranks, so the preference never moves a request onto a cold or swapped-out model
// or evicts one in use, and it never refuses, parks or signals demand by itself.
func (r *Resolver) preferredInstance(pool []MappingCandidate, req inference.Request, now time.Time, judgeLive bool) string {
	if req.PreferredInstanceKey == "" {
		return ""
	}
	i := slices.IndexFunc(pool, func(c MappingCandidate) bool {
		return c.InstanceCapable && InstanceKey(c.Mapping.ID, c.Instance) == req.PreferredInstanceKey
	})
	if i < 0 || !r.underCap(pool[i], now, judgeLive) {
		return ""
	}
	return req.PreferredInstanceKey
}

// preferScored selects from pool, a partition the selection settled on, the candidate
// whose gateway instance key is preferred (preferredInstance) when pool holds it and it is
// Score-viable, and the best-scoring candidate otherwise. An empty preferred is
// argmaxByScore.
func (r *Resolver) preferScored(ctx context.Context, pool []MappingCandidate, model string, audio bool, now time.Time, preferred string) (MappingCandidate, bool, error) {
	if preferred != "" {
		if i := slices.IndexFunc(pool, func(c MappingCandidate) bool { return InstanceKey(c.Mapping.ID, c.Instance) == preferred }); i >= 0 {
			if sel, ok, err := r.argmaxByScore(ctx, pool[i:i+1], model, audio, now); err != nil || ok {
				return sel, ok, err
			}
		}
	}
	return r.argmaxByScore(ctx, pool, model, audio, now)
}

// neverParks reports whether req must be answered without waiting in the admission
// queue: a gate that only selects (inference.Request.NoPark) or a check-only resolve
// (inference.Request.CheckOnly). Either is served from the capped pool when every
// candidate is at its cap (selectWithoutParking).
func neverParks(req inference.Request) bool {
	return req.NoPark || req.CheckOnly
}

// signalOverflow hands the sink d for a request just served on a busy candidate where
// it would otherwise have waited -- one that must not park (inference.Request.NoPark),
// served from the capped pool (selectWithoutParking), or a pinned request served on a
// busy instance of its pinned server (selectPinned) -- once and without asking the queue
// for room -- signalDemand does both for a parking request: the request is served now,
// so no park turn will signal for it, and a selection that found no free instance is
// demand already known. A check-only resolve signals nothing: the request it checks for
// resolves, parks and signals on its own.
func (r *Resolver) signalOverflow(req inference.Request, d DemandSignal) {
	if r.demand == nil || req.CheckOnly {
		return
	}
	r.demand.Signal(d)
}

// selectWithoutParking selects, for a request that never parks (neverParks), from ac's
// pool, whose every candidate is at its cap. It prefers the least loaded instance
// candidates (leastLoadedInstances) and falls back to the whole pool -- the way a
// resolver without a queue fails open -- when there is none, or none of them is
// viable. It signals nothing: its caller hands the sink the demand once the selection
// succeeded (signalOverflow), so a request that could not be served signals no demand.
// model only labels the log lines of the selection; audio is as for argmaxByScore.
//
// The cold cluster heads ac carries come last: a request that cannot wait is served on
// a running candidate at its cap rather than on a head whose cluster still has to form,
// and on a head rather than not at all (the gate that only selects starts the cluster
// itself). A request no pool can serve fails with the cluster's code when there were
// heads, and with no candidate otherwise.
func (r *Resolver) selectWithoutParking(ctx context.Context, ac *atCapacityError, model string, audio bool, now time.Time) (MappingCandidate, bool, error) {
	var sel MappingCandidate
	ok := false
	var err error
	if least := leastLoadedInstances(ac.pool, r.instanceActivity); len(least) > 0 {
		if sel, ok, err = r.selectWithSwapProtection(ctx, least, model, audio, now, ""); err != nil {
			return MappingCandidate{}, false, err
		}
	}
	if !ok && len(ac.pool) > 0 {
		if sel, ok, err = r.selectWithSwapProtection(ctx, ac.pool, model, audio, now, ""); err != nil {
			return MappingCandidate{}, false, err
		}
	}
	if ok {
		sel.InstancePool = ac.instancePool
		return sel, true, nil
	}
	if len(ac.clusterPool) > 0 {
		if sel, ok, err = r.selectWithSwapProtection(ctx, ac.clusterPool, model, audio, now, ""); err != nil || ok {
			return sel, ok, err
		}
	}
	if ac.clusterFallback != nil {
		return MappingCandidate{}, false, ac.clusterFallback
	}
	return MappingCandidate{}, false, nil
}

// leastLoadedInstances is the instance candidates of pool with the fewest requests in
// flight (activity; with no source every instance's load is unknown and they all tie),
// so a request that must be served while every instance is busy queues inside the one
// closest to taking it. nil when pool holds no instance candidate.
func leastLoadedInstances(pool []MappingCandidate, activity InstanceActivityChecker) []MappingCandidate {
	var least []MappingCandidate
	low := 0
	for _, c := range pool {
		if !c.InstanceCapable {
			continue
		}
		load := 0
		if activity != nil {
			load = activity.InstanceActivity(InstanceKey(c.Mapping.ID, c.Instance))
		}
		switch {
		case least == nil || load < low:
			least, low = []MappingCandidate{c}, load
		case load == low:
			least = append(least, c)
		}
	}
	return least
}

// needsFreeLiveSlot reports whether req can only be served by an instance whose live
// slot is free: a live session's resolve, which takes that slot for its whole life, and
// every transcription, buffered and details alike, which the upstream cannot serve while
// a live session holds it -- audio.cpp serves one transcription at a time. Judged on the
// request's capability list, which a transcription always carries (and only a
// transcription carries this capability), so a buffered request is steered off a
// held instance even where the instance's parallelism would still take it.
func needsFreeLiveSlot(req inference.Request) bool {
	return req.LiveSession || slices.Contains(req.RequiredCapabilities, CapabilityTranscription)
}

// underCap reports whether candidate c can take a request now. With judgeLive set (a
// request that needs a free live slot, in a pool that holds an instance candidate, with a
// live-slot source wired) any candidate whose live slot is held is at its cap. Beyond
// that, an instance candidate is under its cap while its own in-flight count is below the
// parallelism its expansion carried (with no instance activity source its load is unknown
// and that part never caps it). Every other candidate keeps the server cap: in-flight on
// its server below max_concurrency minus the server's reserved sessions, and no cap at all
// when max_concurrency is unset (<= 0) or no server activity source is wired.
func (r *Resolver) underCap(c MappingCandidate, now time.Time, judgeLive bool) bool {
	if judgeLive && r.liveSlots.LiveHeld(InstanceKey(c.Mapping.ID, c.Instance)) {
		return false
	}
	if c.InstanceCapable {
		return r.instanceActivity == nil || r.instanceActivity.InstanceActivity(InstanceKey(c.Mapping.ID, c.Instance)) < c.InstanceParallelism
	}
	maxC := c.Mapping.MaxConcurrency
	if maxC <= 0 || r.activity == nil {
		return true
	}
	k, _ := r.activity.ServerActivity(c.Server.ID)
	return k < maxC-r.reservation.activeCount(c.Server.ID, now)
}

// instanceDemand is the demand a request finding every candidate of pool at its cap
// signals once it parks: for the gateway model of pool's first instance candidate (a
// member's own name in a group walk, which hands selectCandidate one member's pool), the
// distinct instance-capable mappings of that model among pool -- the ones this request
// passed every filter for. nil when pool holds no instance candidate.
func instanceDemand(pool []MappingCandidate, req inference.Request) *DemandSignal {
	var d *DemandSignal
	for _, c := range pool {
		if !c.InstanceCapable {
			continue
		}
		if d == nil {
			d = &DemandSignal{Model: c.Mapping.GatewayModelName, APIFlavor: NormalizeAPIFlavor(req.APIFlavor), Live: req.LiveSession}
		}
		if c.Mapping.GatewayModelName == d.Model && !slices.Contains(d.MappingIDs, c.Mapping.ID) {
			d.MappingIDs = append(d.MappingIDs, c.Mapping.ID)
		}
	}
	return d
}

// selectWithSwapProtection applies the swap-protection filter to pool, then selects.
// A NOT-loaded candidate whose server is actively serving (a request in flight, or a
// completion within swapProtectWindow) would evict that live model if routed here — prefer
// to exclude it this pass to avoid thrashing an in-use model. A loaded candidate is never
// protected (serving the requested model is no swap). Fail-open on BOTH emptiness AND
// viability: if protection removes some candidates but the survivors yield no viable pick
// (or it removes them all), fall back to the full pool rather than refuse a request a busy
// server could still serve. Gated on r.activity != nil (the no-op invariant). preferred
// is as for preferScored, applied only within the partition selected here.
func (r *Resolver) selectWithSwapProtection(ctx context.Context, pool []MappingCandidate, model string, audio bool, now time.Time, preferred string) (MappingCandidate, bool, error) {
	if r.activity != nil {
		protected := make([]MappingCandidate, 0, len(pool))
		for _, c := range pool {
			if !r.swapProtected(c, now) {
				protected = append(protected, c)
			}
		}
		if len(protected) == 0 {
			slog.Warn("swap-protection filter empty; routing without it", "model", model)
		} else if len(protected) < len(pool) {
			if sel, ok, err := r.selectFromPool(ctx, protected, model, audio, now, preferred); err != nil || ok {
				return sel, ok, err
			}
			slog.Warn("swap-protection survivors non-viable; routing without it", "model", model)
		}
	}
	return r.selectFromPool(ctx, pool, model, audio, now, preferred)
}

// selectFromPool applies the prefer-loaded partition (dominant, fail-open) and then the
// bounded-tiebreak score to choose a candidate from pool -- or, within the chosen
// partition, the preferred instance when it is there and viable (preferScored). Returns
// ok=false if no candidate in pool is viable (Score gate).
func (r *Resolver) selectFromPool(ctx context.Context, pool []MappingCandidate, model string, audio bool, now time.Time, preferred string) (MappingCandidate, bool, error) {
	// Prefer already-loaded (DOMINANT over the score): if any candidate in the pool has
	// the requested model resident on its server, prefer that partition — avoiding a cold
	// load/swap. When loaded state is unknown (nil checker / no data), no candidate is
	// "loaded", so the pool is unchanged (the P4a no-op invariant). This is the "avoid
	// swaps" precedence; argmaxByScore then ranks within the chosen partition. The
	// preference is dominant only when the loaded partition yields a viable pick — like
	// the context-fit filter above it, it FAILS OPEN: if none of the loaded candidates is
	// Score-viable it spills over to the full pool rather than refuse a request an
	// unloaded server could serve.
	if r.loaded != nil {
		loadedHere := func(c MappingCandidate) bool { return modelLoadedOn(r.loaded, c) }
		if sel, ok, err := r.preferPartition(ctx, pool, loadedHere, model, audio, now, preferred); err != nil || ok {
			return sel, ok, err
		}
	}
	// Prefer a model instance that is already STARTING (loading) over cold-starting a
	// stopped one: the loading instance will soon be ready, so routing to it avoids
	// spinning up a redundant second process. Strictly BELOW the loaded partition above
	// -- a StateRunning instance is ALWAYS in the loaded set (the agent reports
	// LoadedModels StateRunning-only), so it is chosen there before this ever runs; this
	// guarantees "running beats starting". Fail-open: spills to the full pool if the
	// starting subset is non-viable (or empty / no checker installed).
	if r.runtimeState != nil {
		if sel, ok, err := r.preferPartition(ctx, pool, r.modelStartingOn, model, audio, now, preferred); err != nil || ok {
			return sel, ok, err
		}
	}
	return r.preferScored(ctx, pool, model, audio, now, preferred)
}

// preferPartition selects (preferScored) from the candidates of pool that in accepts, in
// pool's order: ok=false when there are none or none is viable, so the caller spills
// over to a wider pool.
func (r *Resolver) preferPartition(ctx context.Context, pool []MappingCandidate, in func(MappingCandidate) bool, model string, audio bool, now time.Time, preferred string) (MappingCandidate, bool, error) {
	part := make([]MappingCandidate, 0, len(pool))
	for _, c := range pool {
		if in(c) {
			part = append(part, c)
		}
	}
	if len(part) == 0 {
		return MappingCandidate{}, false, nil
	}
	return r.preferScored(ctx, part, model, audio, now, preferred)
}

// swapProtected reports whether routing to candidate c would evict a live model: c's
// server is actively serving (in flight, or a completion within swapProtectWindow) AND
// c is not already loaded for the requested model on that server (a loaded candidate is
// no swap). The r.loaded != nil guard also prevents a nil-checker panic in modelLoadedOn.
func (r *Resolver) swapProtected(c MappingCandidate, now time.Time) bool {
	if r.loaded != nil && modelLoadedOn(r.loaded, c) {
		return false
	}
	inFlight, lastCompletedAt := r.activity.ServerActivity(c.Server.ID)
	if inFlight > 0 {
		return true
	}
	if r.swapProtectWindow > 0 && !lastCompletedAt.IsZero() && now.Sub(lastCompletedAt) < r.swapProtectWindow {
		return true
	}
	return false
}

// modelLoadedOn reports whether the candidate's upstream model is currently loaded on
// its server/application, per the loaded-model checker. It asks for the candidate's
// instance address: an agent reports each running instance under its own address, so
// instance 0 being down while instance 2 serves reads as instance 2 loaded and instance
// 0 not, and swap protection and the loaded partitions judge each instance by its own.
func modelLoadedOn(checker LoadedModelChecker, c MappingCandidate) bool {
	address := InstanceAddress(c.Mapping.AppModelName, c.Instance)
	for _, name := range checker.LoadedAppModels(c.Application.ID, c.Server.ID) {
		if name == address {
			return true
		}
	}
	return false
}

// runtimeModelState is the nil-safe accessor for r.runtimeState: a nil checker (unset,
// or NewResolver's default) returns ("", 0, 0, false, false) -- unknown state, no live
// per-model metrics -- so every caller downstream (mergeRuntimeModelMetrics,
// modelStartingOn) is automatically a no-op, matching the P4a/P4b nil-checker lenient
// invariant. metricsOK is threaded through untouched: it gates the active/queue overlay
// (see mergeRuntimeModelMetrics) separately from ok/state (which drives prefer-starting).
// It asks for the candidate's instance address, so each instance reads its own lifecycle
// state and its own active/queue, never instance 0's.
func (r *Resolver) runtimeModelState(c MappingCandidate) (state string, active int, queue int, ok bool, metricsOK bool) {
	if r.runtimeState == nil {
		return "", 0, 0, false, false
	}
	return r.runtimeState.RuntimeModelState(c.Server.ID, InstanceAddress(c.Mapping.AppModelName, c.Instance))
}

// modelStartingOn reports whether the candidate's upstream model is currently reported
// "starting" (loading) on its server, per the runtime-model-state checker. It keys off
// ok/state ONLY (not metricsOK): the lifecycle state predates runtime_model_probe and is
// valid for any runtime_manager agent, so prefer-starting keeps working without the flag.
func (r *Resolver) modelStartingOn(c MappingCandidate) bool {
	state, _, _, ok, _ := r.runtimeModelState(c)
	return ok && state == runtimeStateStarting
}

// mergeRuntimeModelMetrics overlays live per-model active/queue (from the
// runtime-status registry) onto the per-server telemetry when available, so
// scoring reflects THIS model's load rather than the whole server's. The
// overlay is gated on metricsOK, NOT merely ok: an agent that reports a valid
// lifecycle state but has NOT declared runtime_model_probe carries a fabricated
// active=0/queue=0, and overlaying that would make its model look artificially
// idle (a high score => wrong routing). When !metricsOK the per-server
// telemetry is left untouched (the correct fallback); when metricsOK the real
// per-model counts override it.
func (r *Resolver) mergeRuntimeModelMetrics(c MappingCandidate, tel ServerTelemetry, hasTel bool, now time.Time) (ServerTelemetry, bool) {
	_, active, queue, _, metricsOK := r.runtimeModelState(c)
	if !metricsOK {
		return tel, hasTel
	}
	if !hasTel { // live per-model data is fresh telemetry
		tel.ServerID = c.Server.ID
		tel.ReportedAt = now
	}
	tel.ActiveRequests = active
	tel.QueueDepth = queue
	return tel, true
}

// telemetryPass reads each server's telemetry at most once for one pass over a set of
// candidates. The instances of a mapping share their server, so without it a pass over N
// instances would read the same row N times; one pass scores them all against the one
// reading. A failed read is returned, never remembered: it ends the pass.
type telemetryPass struct {
	store resolverStore
	rows  map[string]telemetryRow
}

type telemetryRow struct {
	telemetry ServerTelemetry
	ok        bool
}

func (r *Resolver) newTelemetryPass() *telemetryPass {
	return &telemetryPass{store: r.store, rows: map[string]telemetryRow{}}
}

// read returns serverID's telemetry, from the store on the pass's first ask for it.
func (p *telemetryPass) read(ctx context.Context, serverID string) (ServerTelemetry, bool, error) {
	if row, hit := p.rows[serverID]; hit {
		return row.telemetry, row.ok, nil
	}
	telemetry, ok, err := p.store.TelemetryByServer(ctx, serverID)
	if err != nil {
		return ServerTelemetry{}, false, err
	}
	p.rows[serverID] = telemetryRow{telemetry: telemetry, ok: ok}
	return telemetry, ok, nil
}

func (r *Resolver) argmaxByScore(ctx context.Context, pool []MappingCandidate, model string, audio bool, now time.Time) (MappingCandidate, bool, error) {
	var selected MappingCandidate
	bestScore := 0.0
	found := false
	pass := r.newTelemetryPass()
	for _, candidate := range pool {
		telemetry, ok, err := pass.read(ctx, candidate.Server.ID)
		if err != nil {
			return MappingCandidate{}, false, fmt.Errorf("load server telemetry: %w", err)
		}
		k := telemetry.ActiveRequests
		if r.activity != nil {
			inflight, _ := r.activity.ServerActivity(candidate.Server.ID)
			k = inflight
		}
		telemetry, ok = r.mergeRuntimeModelMetrics(candidate, telemetry, ok, now)
		route := scoringRoute(candidate, telemetry, ok, k, model, audio)
		score, ok := Score(route, model, now)
		if !ok {
			continue
		}
		if !found || score > bestScore {
			selected = candidate
			bestScore = score
			found = true
		}
	}
	return selected, found, nil
}

// candidateEffectiveGenTPS is a candidate's load-aware effective generation speed,
// assembled exactly as argmaxByScore assembles its scoring route so the floor and
// the ordering agree with the scorer. An unmeasured mapping yields 0. pass is the
// caller's telemetry pass, so a loop over a server's instances reads it once.
func (r *Resolver) candidateEffectiveGenTPS(ctx context.Context, pass *telemetryPass, c MappingCandidate, model string) (float64, error) {
	telemetry, ok, err := pass.read(ctx, c.Server.ID)
	if err != nil {
		return 0, fmt.Errorf("load server telemetry: %w", err)
	}
	k := telemetry.ActiveRequests
	if r.activity != nil {
		inflight, _ := r.activity.ServerActivity(c.Server.ID)
		k = inflight
	}
	return effectiveGenTPS(scoringRoute(c, telemetry, ok, k, model, false)), nil
}

func (r *Resolver) activeMappingForApplication(ctx context.Context, applicationID string, gatewayModel string) (ModelMapping, bool, error) {
	mappings, err := r.store.MappingsByApplication(ctx, applicationID)
	if err != nil {
		return ModelMapping{}, false, fmt.Errorf("load application mappings: %w", err)
	}
	for _, mapping := range mappings {
		if mapping.Status == ServerStatusActive && mapping.GatewayModelName == gatewayModel {
			return mapping, true, nil
		}
	}
	return ModelMapping{}, false, nil
}

func serverSelectable(server AIServer) bool {
	return server.Status == ServerStatusActive && server.HealthStatus != HealthUnhealthy
}

// targetFrom builds the Target for a resolved candidate (server + application +
// mapping, plus the mapping's joined capability verdicts). It addresses the
// candidate's instance: ProviderModel is the instance's address, so the body
// and path rewrites reach that instance unchanged, and InstanceKey names it,
// while RouteID stays the mapping id that usage, metrics and capability
// lookups attribute by. The Target's
// flavors, endpoint modes and live-timings flag are the mapping's
// EffectiveFields: the application's own for an ordinary application, and for
// a server_agent mapping the RESOLVED RuntimeSpec's, the sole authority for
// its model (the application's values are only the fallback for a mapping
// that has no spec at all). The model listings resolve a mapping's flavors
// with the same function, so the two cannot disagree on which row wins.
func (r *Resolver) targetFrom(ctx context.Context, c MappingCandidate, apiFlavor string) (Target, error) {
	server, app, mapping := c.Server, c.Application, c.Mapping
	var spec RuntimeSpec
	hasSpec := false
	var liveProgressSpecType string
	if app.Type == ProviderServerAgent {
		loaded, ok, err := r.store.RuntimeSpecByMapping(ctx, mapping.ID)
		if err != nil {
			return Target{}, fmt.Errorf("load runtime spec: %w", err)
		}
		if ok {
			spec, hasSpec = loaded, true
		}
		// EffectiveRuntimeSpecType is the only shape evidence available for a
		// server_agent child: its application type says nothing about what
		// actually serves. Set from the already-loaded spec (ok=false leaves
		// spec zero-valued, whose empty Binary/Type detects to "custom"), so
		// this costs no extra store lookup.
		liveProgressSpecType = string(EffectiveRuntimeSpecType(spec))
	}
	eff := EffectiveFields(app, spec, hasSpec)
	// SpecUpstreamAuth resolves the SEALED token + effective header for this
	// mapping (spec is zero-valued for non-server_agent apps and for a
	// server_agent mapping with no spec, which SpecUpstreamAuth treats as
	// "app" -- today's behaviour, unchanged).
	apiToken, apiTokenHeader := SpecUpstreamAuth(spec, app)
	return Target{
		RouteID:              mapping.ID,
		ServerID:             server.ID,
		Provider:             app.Type,
		Endpoint:             ApplicationEndpoint(server, app),
		Model:                mapping.GatewayModelName,
		ProviderModel:        InstanceAddress(mapping.AppModelName, c.Instance),
		InstanceKey:          InstanceKey(mapping.ID, c.Instance),
		InstanceCapable:      c.InstanceCapable,
		InstancePool:         c.InstancePool,
		Timeout:              time.Duration(app.TimeoutMS) * time.Millisecond,
		APIFlavor:            apiFlavor,
		APIToken:             apiToken,
		APITokenHeader:       apiTokenHeader,
		APIFlavors:           append([]string(nil), eff.APIFlavors...),
		ResponsesMode:        eff.ResponsesMode,
		MessagesMode:         eff.MessagesMode,
		DecisionsMode:        eff.DecisionsMode,
		SystemOneMode:        eff.SystemOneMode,
		OpportunisticMetrics: app.OpportunisticMetricsEnabled,
		// ResponsesLiveTimingsEnabled carries EffectiveFields' spec-then-app
		// resolution, NOT app.ResponsesLiveTimingsEnabled: the flag qualifies
		// ResponsesMode, so it comes from whichever row ResponsesMode came from.
		ResponsesLiveTimingsEnabled: eff.ResponsesLiveTimingsEnabled,
		// LiveProgressSupport reads the candidate's capability verdict
		// (MappingCandidate.LiveProgressSupport). #49-3 moved every writer
		// onto a "live_progress" capability row and migration 79 then dropped
		// the column, so the mapping carries no verdict of its own to read by
		// mistake. Both callers supply a FRESH verdict, by different routes:
		// the candidate query joins the row, and resolveAffinity -- whose
		// mapping comes from MappingsByApplication, which cannot carry a
		// verdict -- does its own keyed MappingCapabilities read. That read
		// exists precisely because forwarding the unwritten column was the
		// bug; this is deliberately NOT a pass-through of prior behaviour.
		LiveProgressSupport:  c.LiveProgressSupport,
		LiveProgressSpecType: liveProgressSpecType,
	}, nil
}

func affinityID(key AffinityKey) string {
	sum := sha256.Sum256([]byte(key.APITokenID + "\x00" + key.Model + "\x00" + key.APIFlavor + "\x00" + key.SessionID))
	return "aff_" + hex.EncodeToString(sum[:12])
}

// memberStatus classifies a group member's availability for the priority walk.
// memberNoMapping (no live mapping at all) is kept distinct from memberUnavailable
// (has live mappings but all gated) so resolveGroup can pick ErrNoModelRoute vs
// ErrNoHealthyHost per §3g.
type memberStatus int

const (
	memberNoMapping   memberStatus = iota // no live mapping at all (unknown-model material)
	memberUnavailable                     // has live mapping(s) but all gated / non-viable
	memberOK                              // a candidate was selected (sel valid)
	memberAtCapacity                      // every candidate is at its effective cap (queue material)
)

// modeClimbUp is the climb_up value of GroupPolicy.FailoverMode.
const modeClimbUp = "climb_up"

// memberPool is the pool one gateway model name (a group member, or the model itself)
// offers a request BEFORE any policy or request preference narrows it: the store read
// (active mappings on active applications serving apiFlavor), the provisioning gate,
// the endpoint-mode refinement and the capability gate, in that order. It is the one
// definition of "could serve this request" that eligibleCandidates (the walk and the
// speed ordering) and groupVoiceScope (the voice pre-pass over the flattened group)
// both start from, so the pre-pass can never count a candidate the walk would not see.
func (r *Resolver) memberPool(ctx context.Context, token auth.Token, name, apiFlavor, fineFlavor string, required []string) ([]MappingCandidate, error) {
	cands, err := r.store.ActiveMappingsForModel(ctx, name, apiFlavor)
	if err != nil {
		return nil, fmt.Errorf("resolve member mappings: %w", err)
	}
	cands, err = r.filterProvisioned(ctx, token, cands)
	if err != nil {
		return nil, err
	}
	cands = filterServesEndpoint(cands, fineFlavor)
	// Gated BEFORE live is taken (by the caller): a member whose only live mappings are
	// all capability-gated must read as memberNoMapping (unknown model), the same
	// no-leak posture the provisioning gate above already uses, not as
	// memberUnavailable (a gated-but-real member).
	return r.filterCapable(ctx, cands, required)
}

// groupVoiceScope is the group-level voice PRE-PASS: before any member is visited it
// flattens the group -- every distinct member's memberPool, one flat set of mappings --
// and asks voiceScopeFor over that whole set. The answer is both halves of the filter at
// once: "does at least one candidate ANYWHERE in the group list the voice" (the
// fail-open switch; none does -> nil, the group routes as it always did) and the allowed
// mapping set the walk then restricts every member to.
//
// It has to be group-level, not a per-member rule. eligibleCandidates sees one member's
// candidates at a time, so "restrict this member only if one of ITS candidates lists the
// voice" leaves a member without the voice fully eligible, and priority or speed order
// can then pick it ahead of the member that has it: such a filter never narrows a group.
//
// Computed once per request and threaded (groupResolve.voice) through the whole
// resolveGroup ladder: the loaded-only and speed-floor filters relax between attempts,
// the voice does not, so a relaxed attempt never reaches a member the voice excluded.
// Nil for a request that names no voice, without reading anything.
func (r *Resolver) groupVoiceScope(ctx context.Context, g groupResolve) (*voiceScope, error) {
	if g.req.RequiredVoice == "" {
		return nil, nil
	}
	pool, err := r.flatGroupPool(ctx, g)
	if err != nil {
		return nil, err
	}
	return r.voiceScopeFor(ctx, g.req.RequiredVoice, pool)
}

// flatGroupPool is every distinct member's memberPool of g, one flat set of mappings: what
// the group as a whole could serve the request on before any policy narrows it.
func (r *Resolver) flatGroupPool(ctx context.Context, g groupResolve) ([]MappingCandidate, error) {
	var pool []MappingCandidate
	seen := make(map[string]struct{}, len(g.members))
	for _, m := range g.members {
		if _, dup := seen[m.MemberGatewayName]; dup {
			continue // a duplicated member name is read once
		}
		seen[m.MemberGatewayName] = struct{}{}
		cands, err := r.memberPool(ctx, g.token, m.MemberGatewayName, g.apiFlavor, g.req.APIFlavor, g.req.RequiredCapabilities)
		if err != nil {
			return nil, err
		}
		pool = append(pool, cands...)
	}
	return pool, nil
}

// groupRealtimeCheck is the group-level real-time PRE-PASS, for a request that must keep
// up with real time: over the flattened group, restricted to g's voice scope, it refuses
// with ErrNoRealtimeCandidate when there were candidates and every one is too slow --
// the answer the walk could not give, since a member it finds too slow reads as one
// without candidates. Nil, without reading anything, for every other request.
func (r *Resolver) groupRealtimeCheck(ctx context.Context, g groupResolve) error {
	if !g.req.RealtimeRequired {
		return nil
	}
	pool, err := r.flatGroupPool(ctx, g)
	if err != nil {
		return err
	}
	_, err = r.filterRealtime(g.voice.restrict(pool), g.req)
	return err
}

// groupWalk carries the two things threaded unchanged through ONE group resolution
// attempt: the policy (which the relaxation ladder rewrites between attempts) and the voice
// pre-pass scope (which it must NOT). Keeping voice a sibling field here, never a
// GroupPolicy field, preserves the invariant that the ladder can relax the policy but never
// the voice. It is built from a groupResolve at each call site (groupResolve.walk), so the
// voice is only ever copied, never recomputed between attempts. realtime is the request's
// real-time marker (inference.Request.RealtimeRequired), which no attempt relaxes either.
type groupWalk struct {
	policy   GroupPolicy
	voice    *voiceScope
	realtime bool
}

// eligibleCandidates resolves one group member's candidates and applies every
// eligibility filter the group's policy imposes: the store read, the provisioning gate,
// the request's voice restriction (nil for none), the speed floor, then the loaded-only
// filter. Both the priority walk (selectMember) and the speed ordering go through it, so
// they share ONE definition of eligibility instead of two that can drift — the order can
// never rank a member the walk would refuse.
//
// live reports whether the member had ANY live, provisioned mapping BEFORE the policy
// filters ran. It is what keeps the two distinguishable "empty" outcomes apart:
//
//   - no candidates, live == false: the member has no live mapping at all — unknown-model
//     material (memberNoMapping, mapping to ErrNoModelRoute).
//   - no candidates, live == true: the member is real and otherwise routable, but every
//     candidate is gated by the floor or the loaded-only filter (memberUnavailable,
//     mapping to ErrNoHealthyHost) — gated by policy, exactly like a down/busy/non-viable
//     candidate, not "unknown".
//
// Conflating the two would misreport a live-but-gated member as an unknown model. The
// provisioning gate deliberately runs BEFORE live is taken (mirroring the main Resolve
// path): a member whose only live mappings sit on servers the principal is not
// provisioned for is live == false — the same no-leak posture the codebase uses elsewhere
// (404 rather than a distinguishable "exists but forbidden" signal).
//
// The two policy filters themselves live in filterBySpeedFloor and filterLoaded; both are
// no-ops for a group that did not opt in, so an opted-out group costs exactly what it did
// before those settings existed.
func (r *Resolver) eligibleCandidates(ctx context.Context, token auth.Token, name, apiFlavor, fineFlavor string, required []string, gw groupWalk) (cands []MappingCandidate, live bool, err error) {
	cands, err = r.memberPool(ctx, token, name, apiFlavor, fineFlavor, required)
	if err != nil {
		return nil, false, err
	}
	// The group's voice restriction (nil = none), taken over the FLATTENED group by
	// groupVoiceScope before any member was visited: this member keeps only the
	// candidates whose mapping is in that set. Applied with the capability gate, BEFORE
	// live is taken, for the same reason: a member whose every mapping the voice excludes
	// reads as memberNoMapping, not as a real-but-gated member. It is a sibling of policy in
	// groupWalk, never a GroupPolicy field, so the relaxation ladder (which only ever
	// rewrites the policy) cannot relax it.
	cands = gw.voice.restrict(cands)
	// The real-time gate, applied like the voice and for the same reason before live is
	// taken: a member measured too slow for a voice chat has nothing to offer one.
	// resolveGroup's pre-pass (groupRealtimeCheck) has already refused a group whose
	// every candidate is too slow, so this only narrows.
	if gw.realtime {
		cands = slices.DeleteFunc(slices.Clone(cands), r.tooSlowForRealtime)
	}
	if len(cands) == 0 {
		return nil, false, nil
	}
	// A member's running instances are candidates like its mappings, read on every call:
	// the group walk calls this afresh on each admission turn, so a parked group request
	// sees an instance that started while it waited. Expanded BEFORE the loaded-only
	// filter, which then keeps an instance by its own address.
	cands = expandInstances(ctx, cands, r.instances)
	cands, err = r.filterBySpeedFloor(ctx, cands, name, gw.policy.MinTokensPerSecond)
	if err != nil {
		return nil, true, err
	}
	return r.filterLoaded(cands, gw.policy.LoadedOnly), true, nil
}

// filterBySpeedFloor drops the candidates whose effective generation speed falls short of
// the group's minimum-speed floor (GroupPolicy.MinTokensPerSecond). The floor is applied
// CANDIDATE by candidate, before selectCandidate ever sees the slice, so a member is never
// served on a too-slow candidate just because its fast one is busy. A floor of zero (or
// below) disables the pass entirely — the no-op invariant: no extra store read, cands
// returned unchanged. name is the member's gateway model name, used for the telemetry
// lookup.
func (r *Resolver) filterBySpeedFloor(ctx context.Context, cands []MappingCandidate, name string, minTokensPerSecond float64) ([]MappingCandidate, error) {
	if minTokensPerSecond <= 0 {
		return cands, nil
	}
	kept := make([]MappingCandidate, 0, len(cands))
	pass := r.newTelemetryPass()
	for _, c := range cands {
		tps, err := r.candidateEffectiveGenTPS(ctx, pass, c, name)
		if err != nil {
			return nil, err
		}
		if tps >= minTokensPerSecond {
			kept = append(kept, c)
		}
	}
	return kept, nil
}

// filterLoaded drops the candidates whose upstream model is not currently loaded on its
// server, for a group whose policy asks for loaded members only. It is a no-op when
// loadedOnly is false, and equally when r.loaded is nil: unknown loaded state excludes
// nothing, so a group without a wired checker never becomes a dead end from this filter
// alone.
func (r *Resolver) filterLoaded(cands []MappingCandidate, loadedOnly bool) []MappingCandidate {
	if !loadedOnly || r.loaded == nil {
		return cands
	}
	kept := make([]MappingCandidate, 0, len(cands))
	for _, c := range cands {
		if modelLoadedOn(r.loaded, c) {
			kept = append(kept, c)
		}
	}
	return kept
}

// parkMaterial is what the members a group walk found at capacity give a request that
// parks: the distinct candidate server ids to watch, the MAX
// admission_queue_timeout_seconds across their candidates (mirroring the main Resolve
// admission-param computation), and the demand each member whose candidates include an
// instance signals -- one per member, by its own model and mappings. first is the
// earliest at-capacity member in walk order, with its capped selection: what a request
// that must not park is served from instead of waiting.
//
// clusterBound is the longest park bound of the cold cluster heads among them, and
// capped whether any member had a candidate other than such a head at its cap
// (parkDeadline). clusterErr is the first cluster error a member was unavailable with:
// what the group answers when nothing serves it and nothing parks.
type parkMaterial struct {
	servers      []string
	queueSecs    int
	demand       []DemandSignal
	first        *atCapacityMember
	clusterBound time.Duration
	capped       bool
	clusterErr   *ClusterUnavailableError
}

// atCapacityMember is one group member found at capacity: its name and the capped
// selection selectCandidate refused it with.
type atCapacityMember struct {
	name string
	ac   *atCapacityError
}

// add folds one more at-capacity member's material into p: its servers, its queue
// timeout if larger, its demand unless a member of the same model already gave one
// (a duplicated member name is walked twice but needs one more instance once), and the
// member itself when it is the first.
func (p *parkMaterial) add(m parkMaterial) {
	p.servers = append(p.servers, m.servers...)
	p.queueSecs = max(p.queueSecs, m.queueSecs)
	p.clusterBound = max(p.clusterBound, m.clusterBound)
	p.capped = p.capped || m.capped
	if p.first == nil {
		p.first = m.first
	}
	if p.clusterErr == nil {
		p.clusterErr = m.clusterErr
	}
	for _, d := range m.demand {
		if !slices.ContainsFunc(p.demand, func(have DemandSignal) bool { return have.Model == d.Model && have.ClusterHead == d.ClusterHead }) {
			p.demand = append(p.demand, d)
		}
	}
}

// selectMember resolves one group member's eligible candidates and reports its status.
// For a memberAtCapacity result it also returns the member's park material (its
// distinct candidate server ids, the MAX queue timeout across those candidates, and the
// demand it signals if the request parks). memberAtCapacity is only possible when an
// AdmissionController is wired (selectCandidate returns errAllAtCapacity only then);
// otherwise an all-at-cap member fails open inside selectCandidate and comes back
// memberOK, so a group without a wired admission controller never queues.
//
// Eligibility (and the memberNoMapping/memberUnavailable distinction it drives) lives in
// eligibleCandidates; this function only turns that outcome into a status and selects.
func (r *Resolver) selectMember(ctx context.Context, token auth.Token, name, apiFlavor string, req inference.Request, now time.Time, gw groupWalk) (MappingCandidate, memberStatus, parkMaterial, error) {
	cands, live, err := r.eligibleCandidates(ctx, token, name, apiFlavor, req.APIFlavor, req.RequiredCapabilities, gw)
	if err != nil {
		return MappingCandidate{}, memberUnavailable, parkMaterial{}, err
	}
	if len(cands) == 0 {
		if !live {
			return MappingCandidate{}, memberNoMapping, parkMaterial{}, nil // no live mapping at all
		}
		return MappingCandidate{}, memberUnavailable, parkMaterial{}, nil // live, but every candidate gated
	}
	selected, ok, serr := r.selectCandidate(ctx, cands, req, now)
	var ac *atCapacityError
	if errors.As(serr, &ac) {
		pm := parkMaterial{
			servers: distinctServerIDs(cands), queueSecs: maxQueueTimeoutSecs(cands), demand: parkDemands(serr),
			first: &atCapacityMember{name: name, ac: ac}, clusterBound: ac.clusterBound, capped: len(ac.pool) > 0,
		}
		return MappingCandidate{}, memberAtCapacity, pm, nil
	}
	// A member whose heads cannot serve is unavailable like any gated member; the
	// walk goes on, and the group answers the cluster's code only when nothing else
	// serves it.
	var ce *ClusterUnavailableError
	if errors.As(serr, &ce) {
		return MappingCandidate{}, memberUnavailable, parkMaterial{clusterErr: ce}, nil
	}
	if serr != nil {
		return MappingCandidate{}, memberUnavailable, parkMaterial{}, serr
	}
	if !ok {
		return MappingCandidate{}, memberUnavailable, parkMaterial{}, nil
	}
	return selected, memberOK, parkMaterial{}, nil
}

// orderMembersBySpeed returns members re-sorted by their fastest ELIGIBLE candidate's
// effective generation speed, descending. Eligibility is eligibleCandidates — the very
// filters the walk applies — so the order can never rank a member the walk would refuse,
// and a member scores on the candidate it would actually be served on rather than on one
// the policy has already excluded. The metric is the load-aware effective speed, never the
// raw stored gen_tokens_per_second, so it agrees with the scorer and the speed floor.
//
// A member with no measurement, and equally one whose candidates are all gated, scores 0
// and therefore sorts LAST; SliceStable compares speed only, so ties keep the manual order
// and a group with no measurements anywhere behaves exactly as it did before speed ordering
// existed.
//
// Cost: this reads EVERY member's mappings (and their telemetry) on every request, where a
// priority-ordered walk stops at the first available member. That is the price of the
// ordering, and only groups that opt into it pay it.
func (r *Resolver) orderMembersBySpeed(ctx context.Context, token auth.Token, members []GroupMember, apiFlavor, fineFlavor string, required []string, gw groupWalk) ([]GroupMember, error) {
	speed := make(map[string]float64, len(members))
	pass := r.newTelemetryPass()
	for _, m := range members {
		if _, done := speed[m.MemberGatewayName]; done {
			continue // a duplicated member name is scored once
		}
		best := 0.0
		cands, _, err := r.eligibleCandidates(ctx, token, m.MemberGatewayName, apiFlavor, fineFlavor, required, gw)
		if err != nil {
			return nil, err
		}
		for _, c := range cands {
			tps, tErr := r.candidateEffectiveGenTPS(ctx, pass, c, m.MemberGatewayName)
			if tErr != nil {
				return nil, tErr
			}
			if tps > best {
				best = tps
			}
		}
		speed[m.MemberGatewayName] = best
	}
	out := append([]GroupMember(nil), members...) // never reorder the caller's slice
	sort.SliceStable(out, func(i, j int) bool {
		return speed[out[i].MemberGatewayName] > speed[out[j].MemberGatewayName]
	})
	return out, nil
}

// speedMarginMet reports whether the candidate is enough faster than the pinned one to
// justify moving a session: strictly MORE than the configured margin above it. A margin of
// 0 is a legitimate value meaning "no margin required" — any strictly faster candidate then
// wins; only a negative margin is clamped. An unmeasured pin (<= 0) always yields true —
// anything measurable beats unknown.
func (r *Resolver) speedMarginMet(ctx context.Context, best, pinned MappingCandidate, bestModel, pinnedModel string, policy GroupPolicy) (bool, error) {
	pass := r.newTelemetryPass()
	pinnedTPS, err := r.candidateEffectiveGenTPS(ctx, pass, pinned, pinnedModel)
	if err != nil {
		return false, err
	}
	if pinnedTPS <= 0 {
		return true, nil
	}
	bestTPS, err := r.candidateEffectiveGenTPS(ctx, pass, best, bestModel)
	if err != nil {
		return false, err
	}
	margin := policy.ClimbSpeedMarginPercent
	if margin < 0 {
		margin = 0
	}
	return bestTPS > pinnedTPS*(1+float64(margin)/100), nil
}

// firstAvailable walks members top-down — in whatever order the slice arrives in, which is
// the manual priority order or the speed-sorted one — and returns the first memberOK member
// (its name + selection). If none is OK it returns the park material accumulated across
// the at-capacity members (for queuing on the union of their servers), plus anyLive =
// whether ANY member had at least one live mapping (a member with live-but-gated
// candidates or at-capacity counts; a member with no mapping does not). anyLive lets
// resolveGroup pick ErrNoModelRoute (no live mapping anywhere) vs ErrNoHealthyHost (§3g).
// A hard store/selection error is propagated. The material is only that: nothing is
// signalled here, because a caller that finds a member (or only probes, like
// climbTarget) parks no request.
func (r *Resolver) firstAvailable(ctx context.Context, token auth.Token, members []GroupMember, apiFlavor string, req inference.Request, now time.Time, gw groupWalk) (string, MappingCandidate, parkMaterial, bool, error) {
	var park parkMaterial
	anyLive := false
	for _, m := range members {
		sel, status, pm, err := r.selectMember(ctx, token, m.MemberGatewayName, apiFlavor, req, now, gw)
		if err != nil {
			return "", MappingCandidate{}, parkMaterial{}, false, err
		}
		switch status {
		case memberOK:
			return m.MemberGatewayName, sel, park, true, nil
		case memberAtCapacity:
			anyLive = true
			park.add(pm)
		case memberUnavailable:
			anyLive = true // has a live mapping, just gated / non-viable
			if park.clusterErr == nil {
				park.clusterErr = pm.clusterErr
			}
		}
		// memberNoMapping contributes nothing (no live mapping).
	}
	return "", MappingCandidate{}, park, anyLive, nil
}

// groupPin returns the concrete member a group session is pinned to, or "" when there
// is no usable pin. A pin is usable only when it exists, is unexpired, carries a
// non-empty ResolvedModel, and that member is still part of the group. A stale/invalid
// (but present) pin is best-effort deleted; a transient store error yields "" without a
// delete (never tears down a good pin on a blip).
func (r *Resolver) groupPin(ctx context.Context, key AffinityKey, members []GroupMember, now time.Time) string {
	aff, ok, err := r.store.Affinity(ctx, key)
	if err != nil || !ok {
		return ""
	}
	if aff.ExpiresAt.After(now) && aff.ResolvedModel != "" && memberNameExists(members, aff.ResolvedModel) {
		return aff.ResolvedModel
	}
	_ = r.store.DeleteAffinity(ctx, key)
	return ""
}

// upsertGroupPin stores/refreshes the group affinity pinned to the resolved member.
// It NEVER writes an empty ResolvedModel (that would be an unusable row that also leaks
// a capacity reservation) and only pins at all when the token carries an id and the
// serving application enables affinity. Mirrors the main Resolve pin: on an UpsertAffinity
// failure it returns the error (a pin that could not be stored is a hard failure, exactly
// as the single-model path treats it).
//
// It also mirrors the main pin's capability guard: a capability-carrying request
// (required non-empty) never writes a pin here either, for the same reason -- key
// is COARSE, so a request sharing its coarse flavor with a text endpoint would
// repoint that endpoint's client. For images the guard is redundant (their
// coarse flavor, openai_images, keys them apart) and keeps them pin-free. See
// the guard in Resolve for the full rationale.
func (r *Resolver) upsertGroupPin(ctx context.Context, token auth.Token, key AffinityKey, name string, sel MappingCandidate, required []string, now time.Time) error {
	if token.ID == "" || name == "" || sel.Application.AffinityTTLSeconds <= 0 || len(required) > 0 {
		return nil
	}
	id := affinityID(key)
	if err := r.store.UpsertAffinity(ctx, RouteAffinity{
		ID:            id,
		APITokenID:    token.ID,
		UserID:        token.UserID,
		Model:         key.Model, // the group name
		ResolvedModel: name,      // the concrete served member (never empty here)
		APIFlavor:     key.APIFlavor,
		SessionID:     key.SessionID,
		ApplicationID: sel.Application.ID,
		ServerID:      sel.Server.ID,
		Instance:      sel.Instance,
		ExpiresAt:     now.Add(time.Duration(sel.Application.AffinityTTLSeconds) * time.Second),
		LastUsedAt:    now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		return fmt.Errorf("store group affinity: %w", err)
	}
	r.reservation.touch(sel.Server.ID, id, now)
	return nil
}

// groupResolve bundles the per-request inputs of ONE group resolution: everything the
// group path needs besides the context (which stays a separate first parameter, as Go
// convention requires). It is passed BY VALUE, so a callee that adjusts a field — the
// relaxation ladder swapping in this attempt's policy, resolveGroupOnce re-ordering
// members by speed or advancing now across an admission wait — only ever changes its own
// copy, never its caller's.
type groupResolve struct {
	token     auth.Token
	req       inference.Request
	key       AffinityKey
	apiFlavor string
	members   []GroupMember
	policy    GroupPolicy
	now       time.Time
	// voice is the request's voice restriction over the whole flattened group (nil = none),
	// set once by resolveGroup's pre-pass (groupVoiceScope) and read by every attempt of
	// the ladder, the speed ordering, the pin check, the climb and the walk. It is a
	// field of its own and not part of policy precisely so that the ladder, which only
	// ever rewrites policy, cannot relax it.
	voice *voiceScope
}

// walk is the groupWalk of g's current attempt.
func (g groupResolve) walk() groupWalk {
	return groupWalk{policy: g.policy, voice: g.voice, realtime: g.req.RealtimeRequired}
}

// resolveGroup runs resolveGroupOnce under progressively relaxed eligibility filters.
// The order encodes the spec's precedence: the loaded-only filter is dropped before the
// speed floor is, and the floor is dropped only when min_speed_fallback says so. Each
// relaxation is CUMULATIVE — built by mutating one carried-forward `relaxed` value, not by
// rebuilding from the original `policy` — so the ladder is monotone: once a filter is
// dropped it STAYS dropped in every later attempt. Rebuilding from `policy` each time would
// let a later attempt resurrect an earlier-dropped filter (e.g. loaded_only reappearing once
// the floor is also dropped), silently breaking both settings' documented promise that
// nothing eligible => the restriction is dropped, never a dead end. The first attempt that
// resolves wins; the LAST attempt's error is the one returned, so the caller still sees
// ErrNoModelRoute vs ErrNoHealthyHost from a real walk.
//
// A requested voice is the one restriction the ladder does NOT relax. It is decided once,
// up front, over the flattened group (groupVoiceScope), and carried unchanged into every
// attempt: loaded_only and the speed floor are preferences that give way when nothing
// satisfies them, but a voice the group is KNOWN to offer on some members is a
// requirement, and serving it from a member without it would be the wrong answer, not a
// degraded one. When the members that offer it are all unusable the final no-healthy-host
// is reported as ErrVoiceUnavailable (voiceScope.refine).
func (r *Resolver) resolveGroup(ctx context.Context, g groupResolve) (Target, error) {
	voice, err := r.groupVoiceScope(ctx, g)
	if err != nil {
		return Target{}, err
	}
	g.voice = voice
	if err := r.groupRealtimeCheck(ctx, g); err != nil {
		return Target{}, err
	}
	// Latch the configured loaded_only BEFORE any relaxation, so the climb_up warm gate
	// keeps reading the group's own setting on every attempt (see GroupPolicy.suppressWarm).
	g.policy.suppressWarm = g.policy.LoadedOnly
	attempts := []GroupPolicy{g.policy}
	relaxed := g.policy
	if g.policy.LoadedOnly {
		relaxed.LoadedOnly = false
		attempts = append(attempts, relaxed)
	}
	if g.policy.MinTokensPerSecond > 0 && g.policy.MinSpeedFallback == MinSpeedFallbackIgnore {
		relaxed.MinTokensPerSecond = 0
		attempts = append(attempts, relaxed)
	}
	var lastErr error
	for _, attempt := range attempts {
		once := g
		once.policy = attempt
		target, err := r.resolveGroupOnce(ctx, once)
		if err == nil {
			return target, nil
		}
		if !errors.Is(err, ErrNoHealthyHost) && !errors.Is(err, ErrNoModelRoute) {
			return Target{}, err // a real failure (store error, admission timeout) — never retried
		}
		lastErr = err
	}
	return Target{}, voice.refine(lastErr)
}

// resolveGroupOnce expands a group into its ordered members and serves the first
// available one (failover) under ONE fixed policy, honoring a sticky pin. Two failover
// modes (§3f):
//
//   - sticky: the pin is preferred while it is available; if it is down or at-capacity the
//     turn falls through walkPriority and re-pins, but never climbs back to a
//     higher-priority member.
//   - climb_up: like sticky, but when the pin IS available AND a higher-priority member is
//     also available, it either switches to the better member NOW (when that member is
//     already loaded — a free climb, no cold-start stall) or fires a best-effort background
//     r.warmer.Warm and keeps serving the pin THIS turn (a later turn climbs once the better
//     member is loaded). The asymmetry: climbing UP happens only when the target is loaded
//     (you already have a working pin — never take a cold start to climb); falling DOWN when
//     the pin is unavailable is immediate even to a cold member (you have no working model).
//     So climb_up differs from sticky only when the pin is available AND a higher-priority
//     member is available. A nil r.warmer makes climb_up purely passive (it only switches
//     once a higher-priority member is loaded by other traffic), and so does a configured
//     loaded_only (GroupPolicy.suppressWarm) — on every attempt of the relaxation ladder.
//
// Under member_order=speed the members are re-sorted by measured speed first, so
// everything above reads "faster" wherever it says "higher-priority", and a climb_up climb
// additionally has to clear the group's climb speed margin before it moves a session.
//
// Errors map per §3g: zero members OR no member with any live mapping -> ErrNoModelRoute
// (unknown model); members with live mappings but all gated (down/busy/non-viable/at-cap)
// -> ErrNoHealthyHost.
func (r *Resolver) resolveGroupOnce(ctx context.Context, g groupResolve) (Target, error) {
	if len(g.members) == 0 {
		return Target{}, ErrNoModelRoute
	}

	// Speed order: re-sort the member slice, because everything downstream reads order
	// from it (firstAvailable walks it; memberIndex reads a member's rank from its
	// position), so a re-sort here makes the whole rest of the function — the pin check,
	// the climb comparison, the walk — mean "fastest" wherever it said "highest priority".
	// The pass is skipped entirely for the default priority order, so that path costs
	// exactly what it did before. Ordering happens per ATTEMPT, not once for the whole
	// relaxation ladder, because eligibility (and therefore a member's score) depends on
	// this attempt's filters: an attempt that has dropped loaded_only must rank members on
	// the candidates it can actually use.
	if g.policy.MemberOrder == MemberOrderSpeed {
		ordered, err := r.orderMembersBySpeed(ctx, g.token, g.members, g.apiFlavor, g.req.APIFlavor, g.req.RequiredCapabilities, g.walk())
		if err != nil {
			return Target{}, err
		}
		g.members = ordered
	}

	// serve builds the target for a selected member and (re)pins the group affinity to it.
	serve := func(name string, sel MappingCandidate) (Target, error) {
		if err := r.upsertGroupPin(ctx, g.token, g.key, name, sel, g.req.RequiredCapabilities, g.now); err != nil {
			return Target{}, err
		}
		return r.targetFrom(ctx, sel, g.apiFlavor)
	}

	// The pin (and, under climb_up, the climb dance) decides this turn's member; an empty
	// name means there is nothing usable to serve from the pin and the walk below takes
	// over.
	pinned, pinSel, err := r.pinnedMember(ctx, g)
	if err != nil {
		return Target{}, err
	}
	if pinned != "" {
		return serve(pinned, pinSel)
	}

	// Fresh request or a fallen-through pin: walk priorities; queue only if every member
	// is at capacity (§3c/§3h). The admission timeout is an absolute wall-clock deadline
	// established on the first queue so the queue's internal liveness re-check cannot re-arm
	// the full budget every iteration (mirrors the main Resolve deadline semantics), counted
	// from the request's admission start (admissionStart) when the caller set one.
	var deadline, start time.Time
	var clusterBound time.Duration
	for {
		name, sel, park, anyLive, err := r.firstAvailable(ctx, g.token, g.members, g.apiFlavor, g.req, g.now, g.walk())
		if err != nil {
			return Target{}, err
		}
		if name != "" {
			return serve(name, sel)
		}
		if neverParks(g.req) && park.first != nil {
			// Every member at capacity and the request must not wait: it is served by
			// the first of them in walk order. The demand it signals is the walk's
			// first at-capacity member with instance candidates -- the served member's
			// own when it has them, a later one's behind a plain model -- since that
			// is the member another instance would relieve.
			sel, ok, serr := r.selectWithoutParking(ctx, park.first.ac, park.first.name, audioRequest(g.req), g.now)
			if serr != nil {
				return Target{}, serr
			}
			if !ok {
				return Target{}, ErrNoHealthyHost
			}
			// A cold cluster head that serves signals its own cluster's demand.
			if d := park.first.ac.overflowDemand(sel); d != nil && d.ClusterHead != "" {
				r.signalOverflow(g.req, *d)
			} else if len(park.demand) > 0 {
				r.signalOverflow(g.req, park.demand[0])
			}
			return serve(park.first.name, sel)
		}
		if len(park.servers) > 0 && r.admission != nil {
			queueTimeout := time.Duration(park.queueSecs) * time.Second
			if start.IsZero() {
				start = admissionStart(g.req, g.now)
			}
			if queueTimeout > 0 && deadline.IsZero() {
				deadline = start.Add(queueTimeout)
			}
			// Once a finite deadline is established it is enforced on EVERY subsequent
			// wake (parkTurn) — even if the current at-capacity union is now all-unbounded
			// (e.g. the sole bounded-timeout member went down mid-queue). A finite wait must
			// never be silently promoted back to unbounded.
			//
			// A turn that parks on cold cluster heads waits for the cluster's own bound
			// instead (parkDeadline), and the longest cluster bound any turn parked under
			// stays the request's, as on the plain path.
			clusterBound = max(clusterBound, park.clusterBound)
			turnDeadline := parkDeadline(start, deadline, clusterBound, !park.capped)
			if werr := r.parkTurn(ctx, turnDeadline, distinctStrings(park.servers), park.demand...); werr != nil {
				return Target{}, werr // ErrAdmissionQueueTimeout / ErrAdmissionQueueFull / ctx err
			}
			g.now = r.clock()
			continue
		}
		// No servable member and nothing to queue. §3g: if NO member has any live
		// mapping the group is effectively an unknown model (ErrNoModelRoute); if
		// members DO have live mappings but all are gated it is ErrNoHealthyHost.
		if !anyLive {
			return Target{}, ErrNoModelRoute
		}
		if park.clusterErr != nil {
			return Target{}, park.clusterErr
		}
		return Target{}, ErrNoHealthyHost
	}
}

// pinnedMember reports the member a pinned group session is to be served on THIS turn,
// with its selection — the pin itself, or (under climb_up only) a better-ranked member the
// session may climb to right now. The pin's availability is checked exactly once here, and
// the climb dance runs ONLY when the pin is available.
//
// An empty name means "nothing to serve from the pin": there is no usable pin, or the pin
// is down / at-capacity. The caller then falls straight through to the ordinary walk and
// re-pins — an immediate fall-DOWN, even to a cold member, which is the intended asymmetry
// (no warm). A store/selection error is propagated and never falls through.
func (r *Resolver) pinnedMember(ctx context.Context, g groupResolve) (string, MappingCandidate, error) {
	pinned := r.groupPin(ctx, g.key, g.members, g.now)
	if pinned == "" {
		return "", MappingCandidate{}, nil
	}
	pinSel, pinStatus, _, err := r.selectMember(ctx, g.token, pinned, g.apiFlavor, g.req, g.now, g.walk())
	if err != nil {
		return "", MappingCandidate{}, err
	}
	if pinStatus != memberOK {
		return "", MappingCandidate{}, nil // pin down OR at-capacity
	}
	if g.policy.FailoverMode == modeClimbUp {
		best, bestSel, cErr := r.climbTarget(ctx, g, pinned, pinSel)
		if cErr != nil {
			return "", MappingCandidate{}, cErr
		}
		if best != "" {
			return best, bestSel, nil // CLIMB
		}
	}
	return pinned, pinSel, nil
}

// climbTarget runs the climb_up dance for an AVAILABLE pin: it reports the better-ranked
// member the session should move to right now, or "" to keep serving the pin this turn.
//
// A climb happens only when a better-ranked member is available AND is already loaded — a
// free climb, no cold-start stall. When it is not loaded the session stays on the pin and
// a best-effort background warm is fired instead, so a later turn can climb once the
// better member is loaded. Under speed ordering "better" means "faster", and a marginally
// faster member is not worth moving a live session to: the configured climb margin has to
// be cleared first. That gate sits ON TOP of the free-climb rule, never replacing it, and
// is consulted only for a speed-ordered group, so a priority-ordered one climbs exactly as
// it always did.
func (r *Resolver) climbTarget(ctx context.Context, g groupResolve, pinned string, pinSel MappingCandidate) (string, MappingCandidate, error) {
	// Is a better-ranked member available? The pin is memberOK, so firstAvailable stops
	// at-or-before it in the effective order → best index <= pin index (the memberIndex
	// guard below is belt-and-suspenders for that invariant).
	best, bestSel, _, _, err := r.firstAvailable(ctx, g.token, g.members, g.apiFlavor, g.req, g.now, g.walk())
	if err != nil {
		return "", MappingCandidate{}, err
	}
	if best == "" || best == pinned || memberIndex(g.members, best) >= memberIndex(g.members, pinned) {
		return "", MappingCandidate{}, nil
	}
	if g.policy.MemberOrder == MemberOrderSpeed {
		met, mErr := r.speedMarginMet(ctx, bestSel, pinSel, best, pinned, g.policy)
		if mErr != nil {
			return "", MappingCandidate{}, mErr
		}
		if !met {
			// Not materially faster: keep the session where it is, and do not warm a member
			// we would refuse to climb to anyway.
			return "", MappingCandidate{}, nil
		}
	}
	if r.memberLoaded(bestSel) {
		return best, bestSel, nil // the better member is already loaded
	}
	// suppressWarm, not LoadedOnly: a loaded_only group must never trigger a load,
	// including on an attempt whose loaded-only FILTER the relaxation ladder has already
	// dropped.
	if r.warmer != nil && !g.policy.suppressWarm {
		r.warmer.Warm(ctx, best) // load-ahead (non-blocking); keep serving the pin this turn
	}
	return "", MappingCandidate{}, nil
}

// memberNameExists reports whether name is one of the group's member gateway names.
func memberNameExists(members []GroupMember, name string) bool {
	for _, m := range members {
		if m.MemberGatewayName == name {
			return true
		}
	}
	return false
}

// memberIndex returns the priority index of name among the group's members (0 = highest
// priority), or -1 when name is not a member. Used by the climb_up dance to confirm a
// candidate is strictly higher-priority than the current pin.
func memberIndex(members []GroupMember, name string) int {
	for i, m := range members {
		if m.MemberGatewayName == name {
			return i
		}
	}
	return -1
}

// memberLoaded reports whether the selected candidate's upstream model is currently loaded
// on its server (per the loaded-model checker). Nil-checker-safe: unknown loaded state is
// treated as NOT loaded, so climb_up never claims a cold member is loaded and thus never
// climbs into a cold start.
func (r *Resolver) memberLoaded(sel MappingCandidate) bool {
	return r.loaded != nil && modelLoadedOn(r.loaded, sel)
}

// distinctServerIDs returns the deduped server ids of a candidate slice (order-stable).
func distinctServerIDs(cands []MappingCandidate) []string {
	seen := make(map[string]struct{}, len(cands))
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		if _, dup := seen[c.Server.ID]; dup {
			continue
		}
		seen[c.Server.ID] = struct{}{}
		out = append(out, c.Server.ID)
	}
	return out
}

// distinctStrings dedupes a string slice (order-stable), used to build the queue union
// across at-capacity members (a server may serve more than one member).
func distinctStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// maxQueueTimeoutSecs returns the MAX admission_queue_timeout_seconds across a candidate
// slice's applications (0 = at least one app is willing to wait unboundedly).
func maxQueueTimeoutSecs(cands []MappingCandidate) int {
	maxSecs := 0
	for _, c := range cands {
		if c.Application.AdmissionQueueTimeoutSeconds > maxSecs {
			maxSecs = c.Application.AdmissionQueueTimeoutSeconds
		}
	}
	return maxSecs
}
