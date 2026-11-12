// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package compat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Reverse translator: System One (POST /v1/systemone) to OpenAI Decisions
// (POST /v1/decisions) and back, for an upstream that answers OpenAI Decisions
// only (ADR-100). Every function here is pure: no I/O, no HTTP.

// Error codes the reverse request translation can refuse with. The HTTP handler
// maps a *DecisionsError to a 400 carrying the same code.
const (
	CodeSystemOneQuestionTypeUnsupported = "systemone.question_type_unsupported"
	CodeSystemOneFieldUnsupported        = "systemone.field_unsupported"
)

// ErrSystemOneRefused is wrapped by the error TranslateOpenAIDecisionsResponse
// returns when the upstream refused one or more questions. The handler answers
// it with 422 systemone.refused.
var ErrSystemOneRefused = errors.New("the upstream refused to answer")

// SystemOneRefusedError names the question keys the upstream refused. It
// unwraps to ErrSystemOneRefused.
type SystemOneRefusedError struct {
	Keys []string
}

func (e *SystemOneRefusedError) Error() string {
	return ErrSystemOneRefused.Error() + ": " + strings.Join(e.Keys, ", ")
}

func (e *SystemOneRefusedError) Unwrap() error { return ErrSystemOneRefused }

// SystemOneAnswerTable records, per question in request order, what the
// response translation needs: the System One key, the type, the choice keys and
// the level labels. Opaque to callers.
type SystemOneAnswerTable struct {
	questions []s1TableQuestion
}

type s1TableQuestion struct {
	key     string
	s1Type  string
	choices []string
	levels  []string
}

func s1FieldErr(format string, args ...any) *DecisionsError {
	return &DecisionsError{Code: CodeSystemOneFieldUnsupported, Message: fmt.Sprintf(format, args...)}
}

func s1TypeErr(format string, args ...any) *DecisionsError {
	return &DecisionsError{Code: CodeSystemOneQuestionTypeUnsupported, Message: fmt.Sprintf(format, args...)}
}

// s1UnsupportedFields are request fields that have no OpenAI Decisions
// counterpart; a present one is refused (s1FieldAbsent says which values count
// as absent). Top-level instructions are listed first. request_id is not
// among them: it is a correlation id, dropped silently whatever its value,
// since the translated body is built from the known fields only.
var s1UnsupportedFields = []string{
	"instructions", "assets", "audio", "videos", "roles", "options", "preset",
	"states", "joint", "chat_template_kwargs", "cache_salt", "priority",
}

// s1FieldAbsent reports whether the unsupported field name, with the raw value
// raw, counts as absent: a missing member or null always does. Top-level
// instructions and cache_salt also do as the empty string. preset has no
// default. Every other field also does with its default value -- false, 0, []
// or {} -- since a client that sends the default asks for nothing the OpenAI
// Decisions upstream would miss.
func s1FieldAbsent(name string, raw json.RawMessage) bool {
	if !isPresentJSON(raw) {
		return true
	}
	switch name {
	case "instructions", "cache_salt":
		return bytes.Equal(bytes.TrimSpace(raw), []byte(`""`))
	case "preset":
		return false
	default:
		return isDefaultJSON(raw)
	}
}

// isDefaultJSON reports whether raw is false, a numeric zero, an empty array or
// an empty object.
func isDefaultJSON(raw json.RawMessage) bool {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	switch v := v.(type) {
	case bool:
		return !v
	case float64:
		return isZeroNumberLiteral(raw)
	case []any:
		return len(v) == 0
	case map[string]any:
		return len(v) == 0
	default:
		return false
	}
}

// isZeroNumberLiteral reports whether the JSON number literal has an all-zero
// mantissa (0, 0.0, -0, 0e5). A literal that only underflows to zero when
// parsed (1e-400) is not zero: the client asked for a nonzero value.
func isZeroNumberLiteral(raw json.RawMessage) bool {
	t := strings.TrimPrefix(string(bytes.TrimSpace(raw)), "-")
	if i := strings.IndexAny(t, "eE"); i >= 0 {
		t = t[:i]
	}
	t = strings.ReplaceAll(t, ".", "")
	return t != "" && strings.Trim(t, "0") == ""
}

// ---------------------------------------------------------------------------
// Request: System One -> OpenAI Decisions
// ---------------------------------------------------------------------------

// TranslateSystemOneRequest converts a System One request body into an OpenAI
// Decisions request body addressed to upstreamModel, and returns the table the
// response translation needs. The questions array follows the System One
// questions object's key order.
func TranslateSystemOneRequest(raw []byte, upstreamModel string) ([]byte, SystemOneAnswerTable, *DecisionsError) {
	var table SystemOneAnswerTable
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil || top == nil {
		return nil, table, s1FieldErr("the request body is not a valid System One request")
	}
	for _, f := range s1UnsupportedFields {
		if !s1FieldAbsent(f, top[f]) {
			return nil, table, s1FieldErr("the field %q has no OpenAI Decisions counterpart", f)
		}
	}
	input, derr := buildOpenAIInput(top["state"], top["images"])
	if derr != nil {
		return nil, table, derr
	}
	questions, tq, derr := buildOpenAIQuestions(top["questions"])
	if derr != nil {
		return nil, table, derr
	}
	table.questions = tq

	var buf bytes.Buffer
	buf.WriteString(`{"model":`)
	writeJSONValue(&buf, upstreamModel)
	buf.WriteString(`,"input":`)
	buf.Write(input)
	buf.WriteString(`,"questions":`)
	buf.Write(questions)
	buf.WriteByte('}')
	return buf.Bytes(), table, nil
}

func isPresentJSON(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

type orderedMember struct {
	key string
	raw json.RawMessage
}

// decodeOrderedObject reads a JSON object's members in document order. A Go map
// would lose that order.
func decodeOrderedObject(raw json.RawMessage) ([]orderedMember, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if d, ok := tok.(json.Delim); err != nil || !ok || d != '{' {
		return nil, false
	}
	var members []orderedMember
	for dec.More() {
		keyTok, err := dec.Token()
		key, ok := keyTok.(string)
		if err != nil || !ok {
			return nil, false
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		members = append(members, orderedMember{key: key, raw: v})
	}
	if _, err := dec.Token(); err != nil {
		return nil, false
	}
	return members, true
}

type openAIInputPart struct {
	Type     string          `json:"type"`
	Text     *string         `json:"text,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
	ImageURL *string         `json:"image_url,omitempty"`
}

type openAIInputMessage struct {
	Type    string            `json:"type"`
	Role    string            `json:"role"`
	Content []openAIInputPart `json:"content"`
}

// buildOpenAIInput renders the System One state and images as the OpenAI input:
// a plain string for a string state without images, otherwise one user message
// whose first part is the state and whose remaining parts are the images.
func buildOpenAIInput(state, imagesRaw json.RawMessage) ([]byte, *DecisionsError) {
	st := bytes.TrimSpace(state)
	if !isPresentJSON(st) {
		return nil, s1FieldErr("the request needs a state")
	}
	images, derr := decodeS1Images(imagesRaw)
	if derr != nil {
		return nil, derr
	}
	var first openAIInputPart
	if st[0] == '"' {
		var s string
		if err := json.Unmarshal(st, &s); err != nil {
			return nil, s1FieldErr("state is not a valid string")
		}
		if len(images) == 0 {
			return marshalS1(s)
		}
		first = openAIInputPart{Type: "input_text", Text: &s}
	} else {
		if derr := checkS1JSONState(st); derr != nil {
			return nil, derr
		}
		first = openAIInputPart{Type: "input_json", Data: json.RawMessage(st)}
	}
	parts := []openAIInputPart{first}
	for i := range images {
		parts = append(parts, openAIInputPart{Type: "input_image", ImageURL: &images[i]})
	}
	return marshalS1([]openAIInputMessage{{Type: "message", Role: "user", Content: parts}})
}

func marshalS1(v any) ([]byte, *DecisionsError) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, s1FieldErr("the request cannot be encoded")
	}
	return b, nil
}

// checkS1JSONState accepts an object or an array of objects, the shapes an
// input_json part carries.
func checkS1JSONState(st []byte) *DecisionsError {
	switch st[0] {
	case '{':
		return nil
	case '[':
		var elems []json.RawMessage
		if err := json.Unmarshal(st, &elems); err != nil {
			return s1FieldErr("state is not a valid array")
		}
		for i, e := range elems {
			if t := bytes.TrimSpace(e); len(t) == 0 || t[0] != '{' {
				return s1FieldErr("state array element %d must be an object", i)
			}
		}
		return nil
	default:
		return s1FieldErr("state must be a string, an object or an array of objects")
	}
}

func decodeS1Images(raw json.RawMessage) ([]string, *DecisionsError) {
	if !isPresentJSON(raw) {
		return nil, nil
	}
	var entries []*string
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, s1FieldErr("images must be an array of image URL strings")
	}
	images := make([]string, 0, len(entries))
	for i, e := range entries {
		if e == nil || *e == "" {
			return nil, s1FieldErr("image %d must be a non-empty URL string", i)
		}
		images = append(images, *e)
	}
	return images, nil
}

type openAIQuestionOut struct {
	Type         string         `json:"type"`
	Name         string         `json:"name"`
	Instructions string         `json:"instructions"`
	Choices      []openAIChoice `json:"choices,omitempty"`
	Levels       []openAILevel  `json:"levels,omitempty"`
}

type openAIChoice struct {
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

type openAILevel struct {
	Label string `json:"label"`
}

type s1QuestionIn struct {
	Type         string          `json:"type"`
	Instructions json.RawMessage `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria"`
}

func buildOpenAIQuestions(raw json.RawMessage) ([]byte, []s1TableQuestion, *DecisionsError) {
	if !isPresentJSON(raw) {
		return nil, nil, s1FieldErr("the request needs questions")
	}
	members, ok := decodeOrderedObject(raw)
	if !ok || len(members) == 0 {
		return nil, nil, s1FieldErr("questions must be a non-empty object")
	}
	out := make([]openAIQuestionOut, 0, len(members))
	table := make([]s1TableQuestion, 0, len(members))
	seen := make(map[string]bool, len(members))
	for _, m := range members {
		if seen[m.key] {
			return nil, nil, s1FieldErr("question %q appears more than once", m.key)
		}
		seen[m.key] = true
		q, tq, derr := buildOpenAIQuestion(m.key, m.raw)
		if derr != nil {
			return nil, nil, derr
		}
		out = append(out, q)
		table = append(table, tq)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, nil, s1FieldErr("the questions cannot be encoded")
	}
	return encoded, table, nil
}

func buildOpenAIQuestion(key string, raw json.RawMessage) (openAIQuestionOut, s1TableQuestion, *DecisionsError) {
	var in s1QuestionIn
	if !isPresentJSON(raw) {
		return openAIQuestionOut{}, s1TableQuestion{}, s1FieldErr("question %q must be an object", key)
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return openAIQuestionOut{}, s1TableQuestion{}, s1FieldErr("question %q is not a valid object", key)
	}
	instructions, derr := decodeS1Instructions(key, in.Instructions)
	if derr != nil {
		return openAIQuestionOut{}, s1TableQuestion{}, derr
	}
	out := openAIQuestionOut{Name: key}
	tq := s1TableQuestion{key: key, s1Type: in.Type}
	switch in.Type {
	case s1TypeNoul:
		out.Type = oaTypePredicate
		out.Instructions, derr = foldNoulCriteria(key, instructions, in.Criteria)
	case s1TypeChoice:
		out.Type = oaTypeChoice
		out.Instructions = instructions
		out.Choices, tq.choices, derr = buildOpenAIChoices(key, in.Criteria)
	case s1TypeScore:
		out.Type = oaTypeScore
		out.Instructions = instructions
		out.Levels, tq.levels, derr = buildOpenAILevels(key, in.Criteria)
	default:
		derr = s1TypeErr("question %q: type %q is not supported; only noul, choice and score are", key, in.Type)
	}
	if derr != nil {
		return openAIQuestionOut{}, s1TableQuestion{}, derr
	}
	return out, tq, nil
}

// decodeS1Instructions reads a question's instructions: a string, or absent
// (empty).
func decodeS1Instructions(key string, raw json.RawMessage) (string, *DecisionsError) {
	if !isPresentJSON(raw) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", s1FieldErr("question %q: instructions must be a string", key)
	}
	return s, nil
}

// foldNoulCriteria appends the present yes/no descriptions to the instructions,
// since a predicate has no field for them.
func foldNoulCriteria(key, instructions string, raw json.RawMessage) (string, *DecisionsError) {
	if !isPresentJSON(raw) {
		return instructions, nil
	}
	members, ok := decodeOrderedObject(raw)
	if !ok {
		return "", s1FieldErr("question %q: noul criteria must be an object of true/false descriptions", key)
	}
	seen := make(map[string]*string, len(members))
	for _, m := range members {
		if _, dup := seen[m.key]; dup {
			return "", s1FieldErr("question %q: noul criteria key %q appears more than once", key, m.key)
		}
		desc, derr := decodeNoulDescription(key, m)
		if derr != nil {
			return "", derr
		}
		seen[m.key] = desc
	}
	if seen["true"] != nil {
		instructions += decisionsTextSeparator + "Answer yes if: " + *seen["true"]
	}
	if seen["false"] != nil {
		instructions += decisionsTextSeparator + "Answer no if: " + *seen["false"]
	}
	return instructions, nil
}

// decodeNoulDescription reads one noul criteria member: the key must be true or
// false, the value a string or null.
func decodeNoulDescription(key string, m orderedMember) (*string, *DecisionsError) {
	if m.key != "true" && m.key != "false" {
		return nil, s1FieldErr("question %q: noul criteria accepts only the keys true and false, got %q", key, m.key)
	}
	var desc *string
	if isPresentJSON(m.raw) {
		if err := json.Unmarshal(m.raw, &desc); err != nil {
			return nil, s1FieldErr("question %q: the %s description must be a string or null", key, m.key)
		}
	}
	return desc, nil
}

func buildOpenAIChoices(key string, raw json.RawMessage) ([]openAIChoice, []string, *DecisionsError) {
	members, ok := decodeOrderedObject(raw)
	if !ok || len(members) == 0 {
		return nil, nil, s1FieldErr("question %q: choice criteria must be a non-empty object of option descriptions", key)
	}
	choices := make([]openAIChoice, 0, len(members))
	keys := make([]string, 0, len(members))
	seen := make(map[string]bool, len(members))
	for _, m := range members {
		if seen[m.key] {
			return nil, nil, s1FieldErr("question %q: option %q appears more than once", key, m.key)
		}
		seen[m.key] = true
		var desc *string
		if isPresentJSON(m.raw) {
			if err := json.Unmarshal(m.raw, &desc); err != nil {
				return nil, nil, s1FieldErr("question %q: the description of option %q must be a string or null", key, m.key)
			}
		}
		c := openAIChoice{Value: m.key}
		if desc != nil {
			c.Description = *desc
		}
		choices = append(choices, c)
		keys = append(keys, m.key)
	}
	return choices, keys, nil
}

func buildOpenAILevels(key string, raw json.RawMessage) ([]openAILevel, []string, *DecisionsError) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || len(items) == 0 {
		return nil, nil, s1FieldErr("question %q: score criteria must be a non-empty array of level names", key)
	}
	levels := make([]openAILevel, 0, len(items))
	labels := make([]string, 0, len(items))
	for i, it := range items {
		label, ok := levelLabel(it)
		if !ok {
			return nil, nil, s1FieldErr("question %q: level %d must be a string or a number", key, i)
		}
		levels = append(levels, openAILevel{Label: label})
		labels = append(labels, label)
	}
	return levels, labels, nil
}

// levelLabel renders a level name; System One str()s its items, so numbers are
// accepted as their literal text.
func levelLabel(raw json.RawMessage) (string, bool) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return "", false
	}
	if t[0] == '"' {
		var s string
		return s, json.Unmarshal(t, &s) == nil
	}
	var n json.Number
	if json.Unmarshal(t, &n) != nil {
		return "", false
	}
	return n.String(), true
}

// ---------------------------------------------------------------------------
// Response: OpenAI Decisions -> System One
// ---------------------------------------------------------------------------

type openAIResponseIn struct {
	Answers []json.RawMessage `json:"answers"`
}

type openAIAnswerIn struct {
	Type          string          `json:"type"`
	Name          *string         `json:"name"`
	Probability   *float64        `json:"probability"`
	Choice        json.RawMessage `json:"choice"`
	Score         *float64        `json:"score"`
	Confidence    *float64        `json:"confidence"`
	Probabilities json.RawMessage `json:"probabilities"`
}

type openAIProbIn struct {
	Value       json.RawMessage `json:"value"`
	Probability *float64        `json:"probability"`
}

type systemOneOut struct {
	Answers json.RawMessage `json:"answers"`
	Model   string          `json:"model"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type s1NoulOut struct {
	Type          string             `json:"type"`
	Noul          float64            `json:"noul"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
}

type s1ChoiceOut struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
}

type s1ScoreOut struct {
	Type          string             `json:"type"`
	Score         float64            `json:"score"`
	Legend        map[string]string  `json:"legend"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
}

// TranslateOpenAIDecisionsResponse converts an OpenAI Decisions response body
// into the System One shape using the table built by TranslateSystemOneRequest.
// clientModel is the model the client asked for. A refused question yields an
// error wrapping ErrSystemOneRefused (a *SystemOneRefusedError naming every
// refused key); a malformed or incomplete body is an error, never a panic.
func TranslateOpenAIDecisionsResponse(body []byte, table SystemOneAnswerTable, clientModel string) ([]byte, DecisionsUsage, error) {
	var in openAIResponseIn
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, DecisionsUsage{}, fmt.Errorf("the upstream's answer is not an OpenAI Decisions response: %w", err)
	}
	if in.Answers == nil {
		return nil, DecisionsUsage{}, errors.New("the upstream's answer carries no answers array")
	}
	parsed, err := parseOpenAIAnswers(in.Answers)
	if err != nil {
		return nil, DecisionsUsage{}, err
	}
	answers, refused, err := translateOpenAIAnswers(table, parsed)
	if err != nil {
		return nil, DecisionsUsage{}, err
	}
	if len(refused) > 0 {
		return nil, DecisionsUsage{}, &SystemOneRefusedError{Keys: refused}
	}
	usage := ParseDecisionsUsage(body, DialectOpenAI)
	out := systemOneOut{Answers: answers, Model: clientModel}
	out.Usage.InputTokens, out.Usage.OutputTokens = usage.InputTokens, usage.OutputTokens
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, DecisionsUsage{}, err
	}
	return encoded, usage, nil
}

func parseOpenAIAnswers(raws []json.RawMessage) ([]openAIAnswerIn, error) {
	out := make([]openAIAnswerIn, len(raws))
	for i, r := range raws {
		if err := json.Unmarshal(r, &out[i]); err != nil {
			return nil, fmt.Errorf("the upstream's answer %d is not an object: %w", i, err)
		}
	}
	return out, nil
}

// matchOpenAIAnswer finds the answer for question i: by name, else by position
// when the answer there is unnamed or carries a name no question owns.
func matchOpenAIAnswer(table SystemOneAnswerTable, answers []openAIAnswerIn, i int) (openAIAnswerIn, bool) {
	key := table.questions[i].key
	for _, a := range answers {
		if a.Name != nil && *a.Name == key {
			return a, true
		}
	}
	if i >= len(answers) {
		return openAIAnswerIn{}, false
	}
	a := answers[i]
	if a.Name == nil {
		return a, true
	}
	for _, q := range table.questions {
		if q.key == *a.Name {
			return openAIAnswerIn{}, false
		}
	}
	return a, true
}

// translateOpenAIAnswers renders the System One answers object in question
// order and collects the keys of refused questions. A refusal outranks a
// malformed or missing sibling answer: the first such problem is returned only
// when nothing was refused.
func translateOpenAIAnswers(table SystemOneAnswerTable, answers []openAIAnswerIn) (json.RawMessage, []string, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	var refused []string
	var firstErr error
	n := 0
	for i, q := range table.questions {
		a, ok := matchOpenAIAnswer(table, answers, i)
		if !ok {
			firstErr = firstError(firstErr, fmt.Errorf("the upstream's answer is incomplete: no answer for question %d (%q)", i, q.key))
			continue
		}
		if a.Type == "refusal" {
			refused = append(refused, q.key)
			continue
		}
		ans, err := translateOpenAIAnswer(q, a)
		if err != nil {
			firstErr = firstError(firstErr, fmt.Errorf("the upstream's answer for question %d (%q) is unusable: %w", i, q.key, err))
			continue
		}
		encoded, err := json.Marshal(ans)
		if err != nil {
			firstErr = firstError(firstErr, err)
			continue
		}
		if n > 0 {
			buf.WriteByte(',')
		}
		n++
		writeJSONValue(&buf, q.key)
		buf.WriteByte(':')
		buf.Write(encoded)
	}
	buf.WriteByte('}')
	if len(refused) > 0 {
		return nil, refused, nil
	}
	if firstErr != nil {
		return nil, nil, firstErr
	}
	return buf.Bytes(), nil, nil
}

func firstError(cur, next error) error {
	if cur != nil {
		return cur
	}
	return next
}

func translateOpenAIAnswer(q s1TableQuestion, a openAIAnswerIn) (any, error) {
	switch q.s1Type {
	case s1TypeNoul:
		return translatePredicateAnswer(a)
	case s1TypeChoice:
		return translateOpenAIChoice(q, a)
	default:
		return translateOpenAIScore(q, a)
	}
}

func expectOpenAIType(a openAIAnswerIn, want string) error {
	if a.Type != want {
		return fmt.Errorf("answer type %q, expected %q", a.Type, want)
	}
	return nil
}

func translatePredicateAnswer(a openAIAnswerIn) (any, error) {
	if err := expectOpenAIType(a, oaTypePredicate); err != nil {
		return nil, err
	}
	if a.Probability == nil || !validProbability(*a.Probability) {
		return nil, errors.New("predicate answer carries no probability in [0, 1]")
	}
	p := *a.Probability
	return s1NoulOut{
		Type: s1TypeNoul, Noul: p,
		Probabilities: map[string]float64{"yes": p, "no": roundFloat(1 - p)},
		Confidence:    roundFloat(math.Max(p, 1-p)),
	}, nil
}

// roundFloat rounds a computed value to 12 decimals so it carries no float
// noise (1 - 0.8 prints as 0.19999999999999996 otherwise).
func roundFloat(x float64) float64 { return math.Round(x*1e12) / 1e12 }

func validProbability(p float64) bool { return p >= 0 && p <= 1 }

func translateOpenAIChoice(q s1TableQuestion, a openAIAnswerIn) (any, error) {
	if err := expectOpenAIType(a, oaTypeChoice); err != nil {
		return nil, err
	}
	chosen, named := probKeyString(a.Choice)
	if !named {
		return nil, errors.New("choice answer names no choice")
	}
	if !containsString(q.choices, chosen) {
		return nil, fmt.Errorf("choice %q is not one of the asked options", chosen)
	}
	probs, err := decodeOpenAIProbs(a.Probabilities, func(raw json.RawMessage) (string, bool) {
		k, present := probKeyString(raw)
		return k, present && containsString(q.choices, k)
	})
	if err != nil {
		return nil, err
	}
	return s1ChoiceOut{Type: s1TypeChoice, Choice: chosen, Probabilities: probs, Confidence: confidenceOr(a.Confidence, probs)}, nil
}

func translateOpenAIScore(q s1TableQuestion, a openAIAnswerIn) (any, error) {
	if err := expectOpenAIType(a, oaTypeScore); err != nil {
		return nil, err
	}
	if a.Score == nil {
		return nil, errors.New("score answer carries no score")
	}
	probs, err := decodeOpenAIProbs(a.Probabilities, func(raw json.RawMessage) (string, bool) {
		idx, ok := scoreLevelIndex(raw, len(q.levels))
		return strconv.Itoa(idx), ok
	})
	if err != nil {
		return nil, err
	}
	legend := make(map[string]string, len(q.levels))
	for i, label := range q.levels {
		legend[strconv.Itoa(i)] = label
	}
	return s1ScoreOut{Type: s1TypeScore, Score: *a.Score, Legend: legend, Probabilities: probs, Confidence: confidenceOr(a.Confidence, probs)}, nil
}

// scoreLevelIndex reads a score probability's value: a JSON number with an
// integral value (1 and 1.0 both name level 1) inside [0, levels). A string,
// a fraction or an out-of-range number is not a level.
func scoreLevelIndex(raw json.RawMessage, levels int) (int, bool) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || t[0] == '"' {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(t), 64)
	if err != nil || f != math.Trunc(f) || f < 0 || f >= float64(levels) {
		return 0, false
	}
	return int(f), true
}

// decodeOpenAIProbs reads an OpenAI probabilities array into a map keyed by
// what keyOf returns for each element's value; keyOf also vouches for the value.
func decodeOpenAIProbs(raw json.RawMessage, keyOf func(json.RawMessage) (string, bool)) (map[string]float64, error) {
	var entries []openAIProbIn
	if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
		return nil, errors.New("probabilities must be an array")
	}
	out := make(map[string]float64, len(entries))
	for _, e := range entries {
		key, ok := keyOf(e.Value)
		if !ok || e.Probability == nil {
			return nil, errors.New("a probabilities element has an unknown value or no probability")
		}
		out[key] = *e.Probability
	}
	return out, nil
}

// confidenceOr returns the upstream's confidence, else the largest probability.
func confidenceOr(c *float64, probs map[string]float64) float64 {
	if c != nil {
		return *c
	}
	maxP := 0.0
	for _, p := range probs {
		maxP = math.Max(maxP, p)
	}
	return maxP
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
