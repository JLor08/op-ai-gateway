// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package compat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Translator between the OpenAI Decisions API (POST /v1/decisions) and the
// System One wire format (POST /v1/systemone), plus the confidence
// normalizer and a usage reader shared by both dialects (ADR-098). Every
// function here is pure: no I/O, no HTTP.

// Error codes a request translation can refuse with. The HTTP handler maps a
// *DecisionsError to a 400 carrying the same code.
const (
	CodeDecisionsInputUnsupported = "decisions.input_unsupported"
	CodeDecisionsQuestionInvalid  = "decisions.question_invalid"
)

// System One question types and OpenAI question types.
const (
	s1TypeNoul   = "noul"
	s1TypeChoice = "choice"
	s1TypeScore  = "score"

	oaTypePredicate = "predicate"
	oaTypeChoice    = "choice"
	oaTypeScore     = "score"

	decisionsTextSeparator = "\n\n"
)

// DecisionsError is a translator refusal. Code is a stable machine code, Message
// a human sentence.
type DecisionsError struct {
	Code    string
	Message string
}

func (e *DecisionsError) Error() string { return e.Code + ": " + e.Message }

func decisionsInputErr(format string, args ...any) *DecisionsError {
	return &DecisionsError{Code: CodeDecisionsInputUnsupported, Message: fmt.Sprintf(format, args...)}
}

func decisionsQuestionErr(format string, args ...any) *DecisionsError {
	return &DecisionsError{Code: CodeDecisionsQuestionInvalid, Message: fmt.Sprintf(format, args...)}
}

// DecisionsUsage is the token usage of one decisions call.
type DecisionsUsage struct {
	InputTokens      int
	OutputTokens     int
	CachedTokens     int
	CacheWriteTokens int
}

// DecisionsDialect names the wire shape of a decisions response body.
type DecisionsDialect int

const (
	// DialectOpenAI is the OpenAI Decisions shape: answers is an array.
	DialectOpenAI DecisionsDialect = iota
	// DialectSystemOne is the System One shape: answers is an object.
	DialectSystemOne
)

// DecisionsAnswerTable records, per question index, what the response
// translation needs to restore the client's view: the System One key, the
// original name, the type, the typed choice values and the original level
// labels. Opaque to callers.
type DecisionsAnswerTable struct {
	questions []decisionsTableQuestion
}

type decisionsTableQuestion struct {
	key     string
	name    *string
	oaType  string
	choices []decisionsTableChoice
	levels  []string
}

type decisionsTableChoice struct {
	key   string
	value json.RawMessage // typed original: a JSON string or boolean
}

// ---------------------------------------------------------------------------
// Request: OpenAI Decisions -> System One
// ---------------------------------------------------------------------------

type decisionsRequestIn struct {
	Input     json.RawMessage   `json:"input"`
	Questions []json.RawMessage `json:"questions"`
}

type decisionsPart struct {
	Type     string          `json:"type"`
	Text     *string         `json:"text"`
	ImageURL *string         `json:"image_url"`
	Data     json.RawMessage `json:"data"`
}

type decisionsItem struct {
	Type    *string         `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// decisionsPiece is one element of the System One state: a text run or a JSON
// value.
type decisionsPiece struct {
	text   string
	data   json.RawMessage
	isJSON bool
}

// TranslateDecisionsRequest converts an OpenAI Decisions request body into a
// System One request body addressed to upstreamModel, and returns the table the
// response translation needs. The System One questions object is written in
// request order.
func TranslateDecisionsRequest(raw []byte, upstreamModel string) ([]byte, DecisionsAnswerTable, *DecisionsError) {
	var table DecisionsAnswerTable
	var in decisionsRequestIn
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, table, decisionsInputErr("the request body is not a valid decisions request")
	}
	pieces, images, derr := decodeDecisionsInput(in.Input)
	if derr != nil {
		return nil, table, derr
	}
	if len(pieces) == 0 && len(images) == 0 {
		return nil, table, decisionsInputErr("input carries no content")
	}
	questions, tq, derr := buildSystemOneQuestions(in.Questions)
	if derr != nil {
		return nil, table, derr
	}
	table.questions = tq

	var buf bytes.Buffer
	buf.WriteString(`{"model":`)
	writeJSONValue(&buf, upstreamModel)
	buf.WriteString(`,"state":`)
	buf.Write(buildDecisionsState(pieces))
	buf.WriteString(`,"questions":`)
	buf.Write(questions)
	if len(images) > 0 {
		buf.WriteString(`,"images":`)
		writeJSONValue(&buf, images)
	}
	buf.WriteByte('}')
	return buf.Bytes(), table, nil
}

// writeJSONValue marshals v into buf. Callers only pass strings and string
// slices, which cannot fail to marshal.
func writeJSONValue(buf *bytes.Buffer, v any) {
	b, _ := json.Marshal(v)
	buf.Write(b)
}

func decodeDecisionsInput(raw json.RawMessage) ([]decisionsPiece, []string, *DecisionsError) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil, decisionsInputErr("input must be a string or an array of message items")
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, nil, decisionsInputErr("input is not a valid string")
		}
		return []decisionsPiece{{text: s}}, nil, nil
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, nil, decisionsInputErr("input is not a valid array")
		}
		return decodeDecisionsItems(items)
	default:
		return nil, nil, decisionsInputErr("input must be a string or an array of message items")
	}
}

func decodeDecisionsItems(items []json.RawMessage) ([]decisionsPiece, []string, *DecisionsError) {
	var pieces []decisionsPiece
	var images []string
	for _, rawItem := range items {
		var item decisionsItem
		if err := json.Unmarshal(rawItem, &item); err != nil {
			return nil, nil, decisionsInputErr("an input item is not a valid object")
		}
		if item.Type != nil && *item.Type != "message" {
			return nil, nil, decisionsInputErr("input items of type %q are not supported; only user messages are", *item.Type)
		}
		if item.Role != "user" {
			return nil, nil, decisionsInputErr("input messages with role %q are not supported; only user messages are", item.Role)
		}
		p, im, derr := decodeDecisionsContent(item.Content)
		if derr != nil {
			return nil, nil, derr
		}
		pieces = append(pieces, p...)
		images = append(images, im...)
	}
	return pieces, images, nil
}

func decodeDecisionsContent(raw json.RawMessage) ([]decisionsPiece, []string, *DecisionsError) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil, decisionsInputErr("a user message needs content")
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, nil, decisionsInputErr("message content is not a valid string")
		}
		return []decisionsPiece{{text: s}}, nil, nil
	case '[':
		var parts []decisionsPart
		if err := json.Unmarshal(trimmed, &parts); err != nil {
			return nil, nil, decisionsInputErr("message content parts are not valid")
		}
		return decodeDecisionsParts(parts)
	default:
		return nil, nil, decisionsInputErr("message content must be a string or an array of parts")
	}
}

func decodeDecisionsParts(parts []decisionsPart) ([]decisionsPiece, []string, *DecisionsError) {
	var pieces []decisionsPiece
	var images []string
	for _, p := range parts {
		switch p.Type {
		case "input_text":
			if p.Text == nil {
				return nil, nil, decisionsInputErr("an input_text part has no text")
			}
			pieces = append(pieces, decisionsPiece{text: *p.Text})
		case "input_json":
			if len(bytes.TrimSpace(p.Data)) == 0 {
				return nil, nil, decisionsInputErr("an input_json part needs data")
			}
			pieces = append(pieces, decisionsPiece{data: p.Data, isJSON: true})
		case "input_image":
			if p.ImageURL == nil || !strings.HasPrefix(*p.ImageURL, "data:") {
				return nil, nil, decisionsInputErr("only input_image parts with a data: image_url are supported; the gateway does not fetch images")
			}
			images = append(images, *p.ImageURL)
		default:
			return nil, nil, decisionsInputErr("input parts of type %q are not supported", p.Type)
		}
	}
	return pieces, images, nil
}

// buildDecisionsState renders the System One state: a string when only text is
// present, the bare JSON value for a lone input_json, otherwise an array of
// the pieces in order.
func buildDecisionsState(pieces []decisionsPiece) []byte {
	hasJSON := false
	for _, p := range pieces {
		hasJSON = hasJSON || p.isJSON
	}
	var buf bytes.Buffer
	switch {
	case !hasJSON:
		texts := make([]string, len(pieces))
		for i, p := range pieces {
			texts[i] = p.text
		}
		writeJSONValue(&buf, strings.Join(texts, decisionsTextSeparator))
	case len(pieces) == 1:
		_ = json.Compact(&buf, pieces[0].data)
	default:
		buf.WriteByte('[')
		for i, p := range pieces {
			if i > 0 {
				buf.WriteByte(',')
			}
			if p.isJSON {
				_ = json.Compact(&buf, p.data)
			} else {
				writeJSONValue(&buf, p.text)
			}
		}
		buf.WriteByte(']')
	}
	return buf.Bytes()
}

type decisionsQuestionIn struct {
	Type         string            `json:"type"`
	Name         json.RawMessage   `json:"name"`
	Instructions json.RawMessage   `json:"instructions"`
	Choices      []json.RawMessage `json:"choices"`
	Levels       []json.RawMessage `json:"levels"`
}

func buildSystemOneQuestions(raws []json.RawMessage) ([]byte, []decisionsTableQuestion, *DecisionsError) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	used := make(map[string]bool, len(raws))
	table := make([]decisionsTableQuestion, 0, len(raws))
	for i, raw := range raws {
		var q decisionsQuestionIn
		if err := json.Unmarshal(raw, &q); err != nil {
			return nil, nil, decisionsQuestionErr("question %d is not a valid object", i)
		}
		name, derr := decodeQuestionName(q.Name, i)
		if derr != nil {
			return nil, nil, derr
		}
		key := uniqueQuestionKey(name, i, used)
		body, tq, derr := buildSystemOneQuestion(q, i)
		if derr != nil {
			return nil, nil, derr
		}
		tq.key, tq.name = key, name
		table = append(table, tq)
		if i > 0 {
			buf.WriteByte(',')
		}
		writeJSONValue(&buf, key)
		buf.WriteByte(':')
		buf.Write(body)
	}
	buf.WriteByte('}')
	return buf.Bytes(), table, nil
}

// decodeQuestionName returns the original name: nil when absent or null.
func decodeQuestionName(raw json.RawMessage, i int) (*string, *DecisionsError) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return nil, decisionsQuestionErr("question %d: name must be a string", i)
	}
	return &s, nil
}

// uniqueQuestionKey picks the System One key: the name, or q<i> when the name
// is absent or empty, suffixed -2, -3, ... until unused.
func uniqueQuestionKey(name *string, i int, used map[string]bool) string {
	base := "q" + strconv.Itoa(i)
	if name != nil && *name != "" {
		base = *name
	}
	key := base
	for n := 2; used[key]; n++ {
		key = base + "-" + strconv.Itoa(n)
	}
	used[key] = true
	return key
}

func buildSystemOneQuestion(q decisionsQuestionIn, i int) ([]byte, decisionsTableQuestion, *DecisionsError) {
	tq := decisionsTableQuestion{oaType: q.Type}
	instructions, derr := decodeQuestionInstructions(q.Instructions)
	if derr != nil {
		return nil, tq, derr
	}
	var buf bytes.Buffer
	buf.WriteString(`{"type":`)
	switch q.Type {
	case oaTypePredicate:
		writeJSONValue(&buf, s1TypeNoul)
		writeInstructionsAndCriteria(&buf, instructions, nil)
	case oaTypeChoice:
		criteria, choices, derr := buildChoiceCriteria(q.Choices, i)
		if derr != nil {
			return nil, tq, derr
		}
		tq.choices = choices
		writeJSONValue(&buf, s1TypeChoice)
		writeInstructionsAndCriteria(&buf, instructions, criteria)
	case oaTypeScore:
		criteria, labels, derr := buildScoreCriteria(q.Levels, i)
		if derr != nil {
			return nil, tq, derr
		}
		tq.levels = labels
		writeJSONValue(&buf, s1TypeScore)
		writeInstructionsAndCriteria(&buf, instructions, criteria)
	default:
		return nil, tq, decisionsQuestionErr("question %d: unknown question type %q", i, q.Type)
	}
	buf.WriteByte('}')
	return buf.Bytes(), tq, nil
}

func writeInstructionsAndCriteria(buf *bytes.Buffer, instructions string, criteria []byte) {
	buf.WriteString(`,"instructions":`)
	writeJSONValue(buf, instructions)
	if criteria != nil {
		buf.WriteString(`,"criteria":`)
		buf.Write(criteria)
	}
}

// decodeQuestionInstructions accepts a string, or an array of input_text parts
// joined with a blank line. Images and JSON parts have no System One
// counterpart in instructions.
func decodeQuestionInstructions(raw json.RawMessage) (string, *DecisionsError) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return "", decisionsQuestionErr("question instructions are not a valid string")
		}
		return s, nil
	case '[':
		var parts []decisionsPart
		if err := json.Unmarshal(trimmed, &parts); err != nil {
			return "", decisionsQuestionErr("question instruction parts are not valid")
		}
		texts := make([]string, 0, len(parts))
		for _, p := range parts {
			if p.Type != "input_text" {
				return "", decisionsInputErr("question instruction parts of type %q are not supported; only input_text is", p.Type)
			}
			if p.Text == nil {
				return "", decisionsInputErr("an input_text part has no text")
			}
			texts = append(texts, *p.Text)
		}
		return strings.Join(texts, decisionsTextSeparator), nil
	default:
		return "", decisionsQuestionErr("question instructions must be a string or an array of parts")
	}
}

type decisionsChoiceIn struct {
	Value       json.RawMessage `json:"value"`
	Description json.RawMessage `json:"description"`
}

// buildChoiceCriteria writes {key: description|null} in choice order and
// returns the typed originals. A string "true" and a boolean true share the
// key "true" and are refused as a collision.
func buildChoiceCriteria(raws []json.RawMessage, qi int) ([]byte, []decisionsTableChoice, *DecisionsError) {
	if len(raws) == 0 {
		return nil, nil, decisionsQuestionErr("question %d: a choice question needs at least one choice", qi)
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	seen := make(map[string]bool, len(raws))
	choices := make([]decisionsTableChoice, 0, len(raws))
	for ci, raw := range raws {
		var c decisionsChoiceIn
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, nil, decisionsQuestionErr("question %d: choice %d is not an object", qi, ci)
		}
		key, typed, derr := choiceKey(c.Value, qi, ci)
		if derr != nil {
			return nil, nil, derr
		}
		desc, derr := choiceDescription(c.Description, qi, ci)
		if derr != nil {
			return nil, nil, derr
		}
		if seen[key] {
			return nil, nil, decisionsQuestionErr("question %d: choice values collide on the key %q (a string and a boolean map to the same System One name)", qi, key)
		}
		seen[key] = true
		choices = append(choices, decisionsTableChoice{key: key, value: typed})
		if ci > 0 {
			buf.WriteByte(',')
		}
		writeJSONValue(&buf, key)
		buf.WriteByte(':')
		if desc == "" {
			buf.WriteString("null")
		} else {
			writeJSONValue(&buf, desc)
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), choices, nil
}

// choiceDescription reads a choice's optional description: absent or null is
// empty, anything but a string is refused.
func choiceDescription(raw json.RawMessage, qi, ci int) (string, *DecisionsError) {
	if !isPresentJSON(raw) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", decisionsQuestionErr("question %d: choice %d description must be a string", qi, ci)
	}
	return s, nil
}

// choiceKey returns the System One option name and the compact typed original
// for a choice value that must be a JSON string or boolean.
func choiceKey(raw json.RawMessage, qi, ci int) (string, json.RawMessage, *DecisionsError) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 {
		switch {
		case trimmed[0] == '"':
			var s string
			if err := json.Unmarshal(trimmed, &s); err == nil && s != "" {
				typed, _ := json.Marshal(s)
				return s, typed, nil
			}
		case bytes.Equal(trimmed, []byte("true")), bytes.Equal(trimmed, []byte("false")):
			return string(trimmed), json.RawMessage(trimmed), nil
		}
	}
	return "", nil, decisionsQuestionErr("question %d: choice %d needs a non-empty string or boolean value", qi, ci)
}

type decisionsLevelIn struct {
	Label       *string `json:"label"`
	Description *string `json:"description"`
}

// buildScoreCriteria writes the ordered level list; a level with a description
// is folded into "label: description". It returns the original labels.
func buildScoreCriteria(raws []json.RawMessage, qi int) ([]byte, []string, *DecisionsError) {
	if len(raws) == 0 {
		return nil, nil, decisionsQuestionErr("question %d: a score question needs at least one level", qi)
	}
	folded := make([]string, 0, len(raws))
	labels := make([]string, 0, len(raws))
	for li, raw := range raws {
		var l decisionsLevelIn
		if err := json.Unmarshal(raw, &l); err != nil || l.Label == nil {
			return nil, nil, decisionsQuestionErr("question %d: level %d needs an object with a label", qi, li)
		}
		labels = append(labels, *l.Label)
		if l.Description != nil && *l.Description != "" {
			folded = append(folded, *l.Label+": "+*l.Description)
		} else {
			folded = append(folded, *l.Label)
		}
	}
	var buf bytes.Buffer
	writeJSONValue(&buf, folded)
	return buf.Bytes(), labels, nil
}

// ---------------------------------------------------------------------------
// Response: System One -> OpenAI Decisions
// ---------------------------------------------------------------------------

type systemOneResponseIn struct {
	Answers map[string]json.RawMessage `json:"answers"`
	Usage   json.RawMessage            `json:"usage"`
}

type systemOneAnswerIn struct {
	Type          string          `json:"type"`
	Noul          *float64        `json:"noul"`
	Choice        json.RawMessage `json:"choice"`
	Score         *float64        `json:"score"`
	Confidence    *float64        `json:"confidence"`
	Probabilities json.RawMessage `json:"probabilities"`
}

type decisionsOut struct {
	Answers []any              `json:"answers"`
	Model   string             `json:"model"`
	Usage   decisionsUsageJSON `json:"usage"`
}

type decisionsUsageJSON struct {
	InputTokens         int                   `json:"input_tokens"`
	InputTokensDetails  decisionsInputDetails `json:"input_tokens_details"`
	OutputTokens        int                   `json:"output_tokens"`
	OutputTokensDetails decisionsOutDetails   `json:"output_tokens_details"`
	TotalTokens         int                   `json:"total_tokens"`
}

type decisionsInputDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

type decisionsOutDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

type predicateAnswerOut struct {
	Type        string  `json:"type"`
	Name        *string `json:"name"`
	Probability float64 `json:"probability"`
}

type choiceAnswerOut struct {
	Type          string          `json:"type"`
	Name          *string         `json:"name"`
	Choice        json.RawMessage `json:"choice"`
	Confidence    float64         `json:"confidence"`
	Probabilities []choiceProbOut `json:"probabilities"`
}

type choiceProbOut struct {
	Value       json.RawMessage `json:"value"`
	Probability float64         `json:"probability"`
}

type scoreAnswerOut struct {
	Type          string         `json:"type"`
	Name          *string        `json:"name"`
	Score         float64        `json:"score"`
	Confidence    float64        `json:"confidence"`
	Probabilities []scoreProbOut `json:"probabilities"`
}

type scoreProbOut struct {
	Value       int     `json:"value"`
	Label       string  `json:"label"`
	Probability float64 `json:"probability"`
}

// TranslateSystemOneResponse converts a System One response body into the
// OpenAI Decisions shape using the table built by TranslateDecisionsRequest.
// model is the client's requested model name. Any malformed or incomplete
// upstream body is an error, never a panic, except a malformed usage, which
// counts as zero usage (decodeSystemOneUsage).
func TranslateSystemOneResponse(body []byte, table DecisionsAnswerTable, clientModel string) ([]byte, DecisionsUsage, error) {
	var in systemOneResponseIn
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, DecisionsUsage{}, fmt.Errorf("the upstream's answer is not a System One response: %w", err)
	}
	if in.Answers == nil {
		return nil, DecisionsUsage{}, errors.New("the upstream's answer carries no answers object")
	}
	usage := decodeSystemOneUsage(in.Usage)
	out := decisionsOut{Answers: make([]any, 0, len(table.questions)), Model: clientModel}
	for i, q := range table.questions {
		raw, ok := in.Answers[q.key]
		if !ok {
			return nil, DecisionsUsage{}, fmt.Errorf("the upstream's answer is incomplete: no answer for question %d (%q)", i, q.key)
		}
		ans, err := translateSystemOneAnswer(q, raw)
		if err != nil {
			return nil, DecisionsUsage{}, fmt.Errorf("the upstream's answer for question %d (%q) is unusable: %w", i, q.key, err)
		}
		out.Answers = append(out.Answers, ans)
	}
	out.Usage = decisionsUsageJSON{
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		TotalTokens:  usage.InputTokens + usage.OutputTokens,
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, DecisionsUsage{}, err
	}
	return encoded, usage, nil
}

// decodeSystemOneUsage reads a System One response's usage object. A missing,
// null or malformed usage (not an object, or counts that are not integers)
// yields zero usage rather than an error, as ParseDecisionsUsage does on the
// passthrough path: a bad usage block must not fail a valid answer set.
func decodeSystemOneUsage(raw json.RawMessage) DecisionsUsage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return DecisionsUsage{}
	}
	var u struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	}
	if err := json.Unmarshal(trimmed, &u); err != nil {
		return DecisionsUsage{}
	}
	return DecisionsUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
}

func translateSystemOneAnswer(q decisionsTableQuestion, raw json.RawMessage) (any, error) {
	var a systemOneAnswerIn
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	want := map[string]string{oaTypePredicate: s1TypeNoul, oaTypeChoice: s1TypeChoice, oaTypeScore: s1TypeScore}[q.oaType]
	if a.Type != "" && a.Type != want {
		return nil, fmt.Errorf("answer type %q, expected %q", a.Type, want)
	}
	confidence := 0.0
	if a.Confidence != nil {
		confidence = *a.Confidence
	}
	switch q.oaType {
	case oaTypePredicate:
		if a.Noul == nil {
			return nil, errors.New("noul answer carries no probability")
		}
		return predicateAnswerOut{Type: oaTypePredicate, Name: q.name, Probability: *a.Noul}, nil
	case oaTypeChoice:
		return translateChoiceAnswer(q, a, confidence)
	default:
		return translateScoreAnswer(q, a, confidence)
	}
}

func translateChoiceAnswer(q decisionsTableQuestion, a systemOneAnswerIn, confidence float64) (any, error) {
	chosen, named := probKeyString(a.Choice)
	if !named {
		return nil, errors.New("choice answer names no choice")
	}
	var chosenValue json.RawMessage
	for _, c := range q.choices {
		if c.key == chosen {
			chosenValue = c.value
		}
	}
	if chosenValue == nil {
		return nil, fmt.Errorf("choice %q is not one of the asked choices", chosen)
	}
	probs, err := decodeProbabilities(a.Probabilities)
	if err != nil {
		return nil, err
	}
	out := choiceAnswerOut{
		Type: oaTypeChoice, Name: q.name, Choice: chosenValue, Confidence: confidence,
		Probabilities: make([]choiceProbOut, 0, len(q.choices)),
	}
	for i, c := range q.choices {
		out.Probabilities = append(out.Probabilities, choiceProbOut{Value: c.value, Probability: probs.lookup(c.key, i)})
	}
	return out, nil
}

func translateScoreAnswer(q decisionsTableQuestion, a systemOneAnswerIn, confidence float64) (any, error) {
	if a.Score == nil {
		return nil, errors.New("score answer carries no score")
	}
	probs, err := decodeProbabilities(a.Probabilities)
	if err != nil {
		return nil, err
	}
	out := scoreAnswerOut{
		Type: oaTypeScore, Name: q.name, Score: *a.Score, Confidence: confidence,
		Probabilities: make([]scoreProbOut, 0, len(q.levels)),
	}
	for i, label := range q.levels {
		out.Probabilities = append(out.Probabilities, scoreProbOut{Value: i, Label: label, Probability: probs.lookup(strconv.Itoa(i), i)})
	}
	return out, nil
}

// probKeyString renders a JSON string as its text and any other literal (a
// number or boolean) as its raw form. The bool is false for an absent or null
// value, so the empty string stays a present, distinct key.
func probKeyString(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", false
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			return s, true
		}
	}
	return string(trimmed), true
}

// decodedProbs holds System One probabilities, which may arrive as a map
// keyed by option name or level index, or as an array (positional numbers, or
// objects with value|name and probability).
type decodedProbs struct {
	byKey   map[string]float64
	entries []probEntry
}

type probEntry struct {
	key    string
	hasKey bool
	p      float64
}

func decodeProbabilities(raw json.RawMessage) (decodedProbs, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return decodedProbs{}, nil
	}
	switch trimmed[0] {
	case '{':
		var m map[string]float64
		if err := json.Unmarshal(trimmed, &m); err != nil {
			return decodedProbs{}, fmt.Errorf("probabilities map is malformed: %w", err)
		}
		return decodedProbs{byKey: m}, nil
	case '[':
		var elems []json.RawMessage
		if err := json.Unmarshal(trimmed, &elems); err != nil {
			return decodedProbs{}, fmt.Errorf("probabilities array is malformed: %w", err)
		}
		entries := make([]probEntry, 0, len(elems))
		for _, e := range elems {
			entry, err := decodeProbEntry(e)
			if err != nil {
				return decodedProbs{}, err
			}
			entries = append(entries, entry)
		}
		return decodedProbs{entries: entries}, nil
	default:
		return decodedProbs{}, errors.New("probabilities must be an object or an array")
	}
}

func decodeProbEntry(raw json.RawMessage) (probEntry, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var o struct {
			Value       json.RawMessage `json:"value"`
			Name        json.RawMessage `json:"name"`
			Probability *float64        `json:"probability"`
		}
		if err := json.Unmarshal(trimmed, &o); err != nil || o.Probability == nil {
			return probEntry{}, errors.New("a probabilities element needs a numeric probability")
		}
		key, hasKey := probKeyString(o.Value)
		if !hasKey {
			key, hasKey = probKeyString(o.Name)
		}
		return probEntry{key: key, hasKey: hasKey, p: *o.Probability}, nil
	}
	var p float64
	if err := json.Unmarshal(trimmed, &p); err != nil {
		return probEntry{}, errors.New("a probabilities element must be a number or an object")
	}
	return probEntry{p: p}, nil
}

// lookup returns the probability for key (map key, or an array element whose
// value|name matches) and falls back to the position pos for unkeyed array
// elements; a missing entry is 0.
func (d decodedProbs) lookup(key string, pos int) float64 {
	if d.byKey != nil {
		return d.byKey[key]
	}
	for _, e := range d.entries {
		if e.hasKey && e.key == key {
			return e.p
		}
	}
	if pos < len(d.entries) && !d.entries[pos].hasKey {
		return d.entries[pos].p
	}
	return 0
}

// values returns every probability, for normalization.
func (d decodedProbs) values() []float64 {
	out := make([]float64, 0, len(d.byKey)+len(d.entries))
	for _, p := range d.byKey {
		out = append(out, p)
	}
	for _, e := range d.entries {
		out = append(out, e.p)
	}
	return out
}

// ---------------------------------------------------------------------------
// Confidence normalization
// ---------------------------------------------------------------------------

// NormalizeDecisionsConfidence rewrites the confidence of every choice and
// score answer that carries parseable probabilities with a positive sum to
// max(p) / sum(p). Predicate answers, answers without usable probabilities and
// every other field are left value-for-value. The bool is false when body is
// not a JSON object of the dialect's shape (best effort: the caller relays the
// body unchanged then); the returned bytes are the input when nothing changed.
func NormalizeDecisionsConfidence(body []byte, d DecisionsDialect) ([]byte, bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		return body, false
	}
	rawAnswers, ok := top["answers"]
	if !ok {
		return body, true
	}
	var rewritten json.RawMessage
	var changed bool
	if d == DialectOpenAI {
		rewritten, changed = normalizeAnswerList(rawAnswers)
	} else {
		rewritten, changed = normalizeAnswerMap(rawAnswers)
	}
	if !changed {
		return body, true
	}
	top["answers"] = rewritten
	out, err := json.Marshal(top)
	if err != nil {
		return body, false
	}
	return out, true
}

func normalizeAnswerList(raw json.RawMessage) (json.RawMessage, bool) {
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return raw, false
	}
	changed := false
	for i, a := range list {
		if n, ok := normalizeAnswer(a); ok {
			list[i], changed = n, true
		}
	}
	if !changed {
		return raw, false
	}
	out, err := json.Marshal(list)
	return out, err == nil
}

func normalizeAnswerMap(raw json.RawMessage) (json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw, false
	}
	changed := false
	for k, a := range m {
		if n, ok := normalizeAnswer(a); ok {
			m[k], changed = n, true
		}
	}
	if !changed {
		return raw, false
	}
	out, err := json.Marshal(m)
	return out, err == nil
}

// normalizeAnswer returns the answer with a recomputed confidence, or false
// when it is not a choice/score answer with usable probabilities.
func normalizeAnswer(raw json.RawMessage) (json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return raw, false
	}
	var typ string
	if err := json.Unmarshal(fields["type"], &typ); err != nil || (typ != s1TypeChoice && typ != s1TypeScore) {
		return raw, false
	}
	probs, err := decodeProbabilities(fields["probabilities"])
	if err != nil {
		return raw, false
	}
	conf, ok := maxShare(probs.values())
	if !ok {
		return raw, false
	}
	encoded, err := json.Marshal(conf)
	if err != nil {
		return raw, false
	}
	fields["confidence"] = encoded
	out, err := json.Marshal(fields)
	return out, err == nil
}

// maxShare returns max/sum for a positive sum.
func maxShare(values []float64) (float64, bool) {
	sum, maxP := 0.0, 0.0
	for _, p := range values {
		sum += p
		if p > maxP {
			maxP = p
		}
	}
	if sum <= 0 {
		return 0, false
	}
	return maxP / sum, true
}

// ---------------------------------------------------------------------------
// Usage
// ---------------------------------------------------------------------------

// ParseDecisionsUsage reads the token usage of a decisions response body. The
// OpenAI dialect also carries cached and cache-write tokens. A malformed body
// yields zero usage.
func ParseDecisionsUsage(body []byte, d DecisionsDialect) DecisionsUsage {
	var in struct {
		Usage struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			InputTokensDetails struct {
				CachedTokens     int `json:"cached_tokens"`
				CacheWriteTokens int `json:"cache_write_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return DecisionsUsage{}
	}
	u := DecisionsUsage{InputTokens: in.Usage.InputTokens, OutputTokens: in.Usage.OutputTokens}
	if d == DialectOpenAI {
		u.CachedTokens = in.Usage.InputTokensDetails.CachedTokens
		u.CacheWriteTokens = in.Usage.InputTokensDetails.CacheWriteTokens
	}
	return u
}
