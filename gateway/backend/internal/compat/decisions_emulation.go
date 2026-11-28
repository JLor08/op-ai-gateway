// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package compat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"op-ai-gateway/internal/inference"
	"strconv"
	"strings"
)

// Decision emulation (#380): an OpenAI Decisions request answered by a chat
// model through structured output. BuildDecisionEmulation turns the Decisions
// body into one neutral chat request whose strict JSON schema asks, per
// question, for a probability per option; ParseDecisionEmulation turns the
// model's JSON text back into an OpenAI Decisions answer, deriving every
// verdict from those numbers. Both are pure: no I/O, no HTTP.

// decisionEmulationInstructions is the fixed system prompt of an emulated
// decision call.
const decisionEmulationInstructions = "You answer classification questions about the state given in the user message. " +
	"For every question, give your honest probability estimate for every option, each a number between 0 and 1 " +
	"(for a yes/no question, the probability that the answer is yes). " +
	"Reply with only a JSON object in the exact schema provided, and nothing else."

// decisionEmulationReasoningEffort is the reasoning effort of an emulated call:
// enough for classification, the cheapest the subscription backend accepts.
const decisionEmulationReasoningEffort = "low"

// decisionEmulationSchemaName names the structured-output schema upstream.
const decisionEmulationSchemaName = "decisions"

// DecisionEmulationTable records, per question in request order, what
// ParseDecisionEmulation needs to restore the client's view: the original
// name, the type, the typed choice values and the level labels. Opaque to
// callers.
type DecisionEmulationTable struct {
	questions []decisionsTableQuestion
}

// BuildDecisionEmulation converts an OpenAI Decisions request body into the
// neutral chat request that emulates it on upstreamModel, plus the table
// ParseDecisionEmulation needs. The input and question rules are matrix A's
// (TranslateDecisionsRequest), with its refusal codes, except that an
// input_image with an http(s) image_url is accepted (the vendor fetches it,
// the gateway does not).
func BuildDecisionEmulation(raw []byte, upstreamModel string) (inference.Request, DecisionEmulationTable, *DecisionsError) {
	var table DecisionEmulationTable
	var in decisionsRequestIn
	if err := json.Unmarshal(raw, &in); err != nil {
		return inference.Request{}, table, decisionsInputErr("the request body is not a valid decisions request")
	}
	pieces, derr := decodeDecisionsInput(in.Input, decisionsInputRules{remoteImages: true})
	if derr != nil {
		return inference.Request{}, table, derr
	}
	if len(pieces) == 0 {
		return inference.Request{}, table, decisionsInputErr("input carries no content")
	}
	if len(in.Questions) == 0 {
		return inference.Request{}, table, decisionsQuestionErr("at least one question is required")
	}
	questions := make([]decisionsParsedQuestion, 0, len(in.Questions))
	for i, rq := range in.Questions {
		pq, derr := parseDecisionsQuestion(rq, i, decisionsQuestionRules{})
		if derr != nil {
			return inference.Request{}, table, derr
		}
		questions = append(questions, pq)
		table.questions = append(table.questions, emulationTableQuestion(pq, i))
	}

	content := make([]inference.ContentPart, 0, len(pieces)+1)
	for _, p := range pieces {
		content = append(content, emulationStatePart(p))
	}
	content = append(content, inference.ContentPart{Type: inference.ContentText, Text: emulationQuestionList(questions)})

	req := inference.Request{
		Model:           upstreamModel,
		Stream:          false,
		ReasoningEffort: decisionEmulationReasoningEffort,
		Messages: []inference.Message{
			{Role: inference.RoleSystem, Content: []inference.ContentPart{{Type: inference.ContentText, Text: decisionEmulationInstructions}}},
			{Role: inference.RoleUser, Content: content},
		},
		OutputSchema: &inference.OutputSchema{
			Name:   decisionEmulationSchemaName,
			Schema: emulationSchema(questions),
			Strict: true,
		},
	}
	return req, table, nil
}

func emulationQuestionKey(i int) string { return "q" + strconv.Itoa(i) }
func emulationOptionKey(j int) string   { return "o" + strconv.Itoa(j) }
func emulationLevelKey(k int) string    { return "l" + strconv.Itoa(k) }

func emulationTableQuestion(pq decisionsParsedQuestion, i int) decisionsTableQuestion {
	tq := decisionsTableQuestion{key: emulationQuestionKey(i), name: pq.name, oaType: pq.oaType}
	for j, c := range pq.choices {
		tq.choices = append(tq.choices, decisionsTableChoice{key: emulationOptionKey(j), value: c.value})
	}
	for _, l := range pq.levels {
		tq.levels = append(tq.levels, l.label)
	}
	return tq
}

// emulationStatePart renders one input piece as a user content part: text as
// is, JSON pretty-printed as text, an image as an image part.
func emulationStatePart(p decisionsPiece) inference.ContentPart {
	switch {
	case p.isImage:
		return inference.ContentPart{Type: inference.ContentImage, ImageURL: p.image}
	case p.isJSON:
		var buf bytes.Buffer
		if err := json.Indent(&buf, bytes.TrimSpace(p.data), "", "  "); err != nil {
			// Unreachable: the data was decoded as JSON. Keep the raw text.
			return inference.ContentPart{Type: inference.ContentText, Text: string(p.data)}
		}
		return inference.ContentPart{Type: inference.ContentText, Text: buf.String()}
	default:
		return inference.ContentPart{Type: inference.ContentText, Text: p.text}
	}
}

// emulationQuestionList renders the numbered question list: per question its
// schema key, type, name and instructions, and its options or levels next to
// their schema keys.
func emulationQuestionList(questions []decisionsParsedQuestion) string {
	var b strings.Builder
	b.WriteString("Questions:\n")
	for i, q := range questions {
		key := emulationQuestionKey(i)
		fmt.Fprintf(&b, "\n%d. Key %s (%s)\n", i+1, key, q.oaType)
		if q.name != nil && *q.name != "" {
			fmt.Fprintf(&b, "Name: %s\n", *q.name)
		}
		if q.instructions != "" {
			fmt.Fprintf(&b, "Instructions: %s\n", q.instructions)
		}
		writeEmulationAnswerSpec(&b, q, key)
	}
	return b.String()
}

// writeEmulationAnswerSpec writes the per-type part of a question: its options
// or levels and where the answer goes.
func writeEmulationAnswerSpec(b *strings.Builder, q decisionsParsedQuestion, key string) {
	switch q.oaType {
	case oaTypePredicate:
		fmt.Fprintf(b, "Answer: the probability that the answer is yes, as answers.%s.yes.\n", key)
	case oaTypeChoice:
		b.WriteString("Options:\n")
		for j, c := range q.choices {
			writeEmulationListItem(b, emulationOptionKey(j), string(c.value), c.description)
		}
		fmt.Fprintf(b, "Answer: a probability for each option, as answers.%s.probabilities.\n", key)
	default: // oaTypeScore
		b.WriteString("Levels:\n")
		for k, l := range q.levels {
			writeEmulationListItem(b, emulationLevelKey(k), l.label, l.description)
		}
		fmt.Fprintf(b, "Answer: a probability for each level, as answers.%s.probabilities.\n", key)
	}
}

// writeEmulationListItem writes one "- key: label (description)" line.
func writeEmulationListItem(b *strings.Builder, key, label, description string) {
	fmt.Fprintf(b, "- %s: %s", key, label)
	if description != "" {
		fmt.Fprintf(b, " (%s)", description)
	}
	b.WriteByte('\n')
}

// strictObject is a strict JSON schema object: every property required, no
// additional properties. keys gives the property order.
func strictObject(keys []string, props map[string]any) map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             keys,
		"additionalProperties": false,
	}
}

func numberSchema() map[string]any { return map[string]any{"type": "number"} }

// numberObject is a strict object of number properties named keys.
func numberObject(keys []string) map[string]any {
	props := make(map[string]any, len(keys))
	for _, k := range keys {
		props[k] = numberSchema()
	}
	return strictObject(keys, props)
}

// emulationSchema builds {answers: {q<i>: <per-type object>}} (spec §4.4).
func emulationSchema(questions []decisionsParsedQuestion) map[string]any {
	keys := make([]string, 0, len(questions))
	props := make(map[string]any, len(questions))
	for i, q := range questions {
		key := emulationQuestionKey(i)
		var answer map[string]any
		switch q.oaType {
		case oaTypePredicate:
			answer = numberObject([]string{"yes"})
		case oaTypeChoice:
			opts := make([]string, 0, len(q.choices))
			for j := range q.choices {
				opts = append(opts, emulationOptionKey(j))
			}
			answer = strictObject([]string{"probabilities"}, map[string]any{"probabilities": numberObject(opts)})
		default: // oaTypeScore
			levels := make([]string, 0, len(q.levels))
			for k := range q.levels {
				levels = append(levels, emulationLevelKey(k))
			}
			answer = strictObject([]string{"probabilities"}, map[string]any{"probabilities": numberObject(levels)})
		}
		answer["description"] = fmt.Sprintf("Answer to question %d (%s, %s)", i+1, key, q.oaType)
		keys = append(keys, key)
		props[key] = answer
	}
	return strictObject([]string{"answers"}, map[string]any{"answers": strictObject(keys, props)})
}

// ---------------------------------------------------------------------------
// Answer: model JSON -> OpenAI Decisions
// ---------------------------------------------------------------------------

type emulationAnswerIn struct {
	Yes           *float64            `json:"yes"`
	Probabilities map[string]*float64 `json:"probabilities"`
}

// ParseDecisionEmulation turns the emulating model's text into an OpenAI
// Decisions answer body for clientModel. The text must be the schema's JSON
// object; a surrounding Markdown code fence is tolerated, anything else
// (prose, a missing question or option, a choice or score whose probabilities
// sum to 0) is an error. A predicate probability is clamped to [0, 1]. Choice
// and score probabilities are floored at 0 and renormalized to sum 1 (so they
// end in [0, 1] too, and a model that answers in relative weights such as 1
// and 3 keeps their ratio); the verdict is the argmax (ties go to the first
// option in request order) and the confidence its probability;
// all derived numbers are rounded to 12 decimals. usage is spelled as the
// Responses usage object.
func ParseDecisionEmulation(text string, table DecisionEmulationTable, clientModel string, usage inference.Usage) ([]byte, error) {
	var in struct {
		Answers map[string]json.RawMessage `json:"answers"`
	}
	if err := json.Unmarshal([]byte(stripCodeFence(text)), &in); err != nil {
		return nil, fmt.Errorf("the model's answer is not the requested JSON: %w", err)
	}
	if in.Answers == nil {
		return nil, errors.New("the model's answer carries no answers object")
	}
	out := decisionsOut{Answers: make([]any, 0, len(table.questions)), Model: clientModel}
	for i, q := range table.questions {
		raw, ok := in.Answers[q.key]
		if !ok {
			return nil, fmt.Errorf("the model's answer has no answer for question %d (%s)", i, q.key)
		}
		var a emulationAnswerIn
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, fmt.Errorf("the model's answer for question %d (%s) is malformed: %w", i, q.key, err)
		}
		ans, err := emulationAnswer(q, a)
		if err != nil {
			return nil, fmt.Errorf("the model's answer for question %d (%s) is unusable: %w", i, q.key, err)
		}
		out.Answers = append(out.Answers, ans)
	}
	total := usage.TotalTokens
	if total == 0 {
		total = usage.InputTokens + usage.OutputTokens
	}
	out.Usage = decisionsUsageJSON{
		InputTokens: usage.InputTokens,
		InputTokensDetails: decisionsInputDetails{
			CachedTokens:     usage.CachedTokens,
			CacheWriteTokens: usage.CacheWriteTokens,
		},
		OutputTokens: usage.OutputTokens,
		// inference.Usage carries no reasoning-token count, so it is 0.
		OutputTokensDetails: decisionsOutDetails{},
		TotalTokens:         total,
	}
	return json.Marshal(out)
}

// stripCodeFence removes one Markdown code fence (``` or ```json) around the
// text, and surrounding whitespace.
func stripCodeFence(text string) string {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "```") || !strings.HasSuffix(t, "```") || len(t) < 6 {
		return t
	}
	inner := t[3 : len(t)-3]
	if nl := strings.IndexByte(inner, '\n'); nl >= 0 {
		lang := strings.TrimSpace(inner[:nl])
		if lang == "" || strings.EqualFold(lang, "json") {
			inner = inner[nl+1:]
		}
	}
	return strings.TrimSpace(inner)
}

func clampProbability(p float64) float64 {
	switch {
	case p < 0:
		return 0
	case p > 1:
		return 1
	default:
		return p
	}
}

func emulationAnswer(q decisionsTableQuestion, a emulationAnswerIn) (any, error) {
	switch q.oaType {
	case oaTypePredicate:
		if a.Yes == nil {
			return nil, errors.New("no yes probability")
		}
		return predicateAnswerOut{Type: oaTypePredicate, Name: q.name, Probability: roundFloat(clampProbability(*a.Yes))}, nil
	case oaTypeChoice:
		keys := make([]string, len(q.choices))
		for j, c := range q.choices {
			keys[j] = c.key
		}
		probs, best, err := emulationDistribution(a.Probabilities, keys)
		if err != nil {
			return nil, err
		}
		out := choiceAnswerOut{
			Type: oaTypeChoice, Name: q.name, Choice: q.choices[best].value, Confidence: probs[best],
			Probabilities: make([]choiceProbOut, 0, len(q.choices)),
		}
		for j, c := range q.choices {
			out.Probabilities = append(out.Probabilities, choiceProbOut{Value: c.value, Probability: probs[j]})
		}
		return out, nil
	default: // oaTypeScore
		keys := make([]string, len(q.levels))
		for k := range q.levels {
			keys[k] = emulationLevelKey(k)
		}
		probs, best, err := emulationDistribution(a.Probabilities, keys)
		if err != nil {
			return nil, err
		}
		out := scoreAnswerOut{
			Type: oaTypeScore, Name: q.name, Score: float64(best), Confidence: probs[best],
			Probabilities: make([]scoreProbOut, 0, len(q.levels)),
		}
		for k, label := range q.levels {
			out.Probabilities = append(out.Probabilities, scoreProbOut{Value: k, Label: label, Probability: probs[k]})
		}
		return out, nil
	}
}

// emulationDistribution reads the probability of every key, floors each at 0,
// renormalizes them to sum 1 and returns them rounded, in key order,
// with the index of the first largest one.
func emulationDistribution(in map[string]*float64, keys []string) ([]float64, int, error) {
	clamped := make([]float64, len(keys))
	sum := 0.0
	for j, k := range keys {
		p, ok := in[k]
		if !ok || p == nil {
			return nil, 0, fmt.Errorf("no probability for %s", k)
		}
		clamped[j] = max(*p, 0)
		sum += clamped[j]
	}
	if sum <= 0 {
		return nil, 0, errors.New("the probabilities sum to 0")
	}
	best := 0
	for j, p := range clamped {
		if p > clamped[best] {
			best = j
		}
	}
	out := make([]float64, len(keys))
	for j, p := range clamped {
		out[j] = roundFloat(p / sum)
	}
	return out, best, nil
}
