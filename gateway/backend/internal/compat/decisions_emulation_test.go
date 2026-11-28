// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package compat

import (
	"encoding/json"
	"op-ai-gateway/internal/inference"
	"sort"
	"strings"
	"testing"
)

// emulationBody is a three-question OpenAI Decisions body: a nameless
// predicate, a choice with an empty name and a boolean option, and a named
// score with two levels.
const emulationBody = `{
	"model": "client-model",
	"input": "The ticket says: my invoice is wrong.",
	"questions": [
		{"type": "predicate", "instructions": "Is this a billing issue?"},
		{"type": "choice", "name": "", "instructions": "Which team handles it?",
		 "choices": [
			{"value": "billing", "description": "Invoices and payments"},
			{"value": true, "description": "Anyone"},
			{"value": "support"}
		 ]},
		{"type": "score", "name": "urgency", "instructions": "How urgent is it?",
		 "levels": [{"label": "low", "description": "can wait"}, {"label": "high"}]}
	]
}`

func mustBuildEmulation(t *testing.T, body string) (inference.Request, DecisionEmulationTable) {
	t.Helper()
	req, table, derr := BuildDecisionEmulation([]byte(body), "gpt-5.4-mini")
	if derr != nil {
		t.Fatalf("unexpected refusal %s: %s", derr.Code, derr.Message)
	}
	return req, table
}

// schemaObject navigates the schema through properties.<key> steps.
func schemaObject(t *testing.T, schema map[string]any, path ...string) map[string]any {
	t.Helper()
	cur := schema
	for _, key := range path {
		props, ok := cur["properties"].(map[string]any)
		if !ok {
			t.Fatalf("no properties at %v in %v", key, cur)
		}
		next, ok := props[key].(map[string]any)
		if !ok {
			t.Fatalf("no property %q in %v", key, props)
		}
		cur = next
	}
	return cur
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// assertStrictObjects walks the schema and checks that every object schema
// forbids additional properties and requires all of its properties, and that
// every leaf is a number.
func assertStrictObjects(t *testing.T, where string, node map[string]any) {
	t.Helper()
	switch node["type"] {
	case "object":
		if node["additionalProperties"] != false {
			t.Errorf("%s: additionalProperties = %v, want false", where, node["additionalProperties"])
		}
		props, _ := node["properties"].(map[string]any)
		var required []string
		switch r := node["required"].(type) {
		case []string:
			required = r
		case []any:
			for _, v := range r {
				s, _ := v.(string)
				required = append(required, s)
			}
		}
		sort.Strings(required)
		if strings.Join(required, ",") != strings.Join(sortedKeys(props), ",") {
			t.Errorf("%s: required = %v, want all of %v", where, required, sortedKeys(props))
		}
		for k, v := range props {
			child, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("%s.%s is not a schema object", where, k)
			}
			assertStrictObjects(t, where+"."+k, child)
		}
	case "number":
	default:
		t.Errorf("%s: type = %v, want object or number", where, node["type"])
	}
}

func TestBuildDecisionEmulationSchema(t *testing.T) {
	req, _ := mustBuildEmulation(t, emulationBody)

	if req.Model != "gpt-5.4-mini" || req.ReasoningEffort != "low" || req.Stream {
		t.Fatalf("model/effort/stream = %q/%q/%v, want gpt-5.4-mini/low/false", req.Model, req.ReasoningEffort, req.Stream)
	}
	if req.OutputSchema == nil || !req.OutputSchema.Strict || req.OutputSchema.Name == "" {
		t.Fatalf("OutputSchema = %+v, want a named strict schema", req.OutputSchema)
	}
	// Round-trip so the test sees the schema as the upstream will.
	raw, err := json.Marshal(req.OutputSchema.Schema)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	assertStrictObjects(t, "schema", schema)

	answers := schemaObject(t, schema, "answers")
	if got := strings.Join(sortedKeys(answers["properties"].(map[string]any)), ","); got != "q0,q1,q2" {
		t.Fatalf("answers keys = %s, want q0,q1,q2", got)
	}
	for _, q := range []string{"q0", "q1", "q2"} {
		desc, _ := schemaObject(t, schema, "answers", q)["description"].(string)
		if !strings.Contains(desc, q) {
			t.Errorf("%s description %q does not name the question", q, desc)
		}
	}
	if got := strings.Join(sortedKeys(schemaObject(t, schema, "answers", "q0")["properties"].(map[string]any)), ","); got != "yes" {
		t.Errorf("q0 keys = %s, want yes", got)
	}
	choice := schemaObject(t, schema, "answers", "q1", "probabilities")
	if got := strings.Join(sortedKeys(choice["properties"].(map[string]any)), ","); got != "o0,o1,o2" {
		t.Errorf("q1.probabilities keys = %s, want o0,o1,o2", got)
	}
	score := schemaObject(t, schema, "answers", "q2", "probabilities")
	if got := strings.Join(sortedKeys(score["properties"].(map[string]any)), ","); got != "l0,l1" {
		t.Errorf("q2.probabilities keys = %s, want l0,l1", got)
	}
}

func messageText(m inference.Message) string { return m.Text() }

func TestBuildDecisionEmulationPrompt(t *testing.T) {
	req, _ := mustBuildEmulation(t, emulationBody)
	if len(req.Messages) != 2 || req.Messages[0].Role != inference.RoleSystem || req.Messages[1].Role != inference.RoleUser {
		t.Fatalf("messages = %+v, want one system and one user message", req.Messages)
	}
	if messageText(req.Messages[0]) != decisionEmulationInstructions {
		t.Errorf("system text = %q, want the fixed template", messageText(req.Messages[0]))
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("request does not validate: %v", err)
	}
	user := messageText(req.Messages[1])
	lineWith := func(needles ...string) {
		t.Helper()
		for _, line := range strings.Split(user, "\n") {
			all := true
			for _, n := range needles {
				all = all && strings.Contains(line, n)
			}
			if all {
				return
			}
		}
		t.Errorf("no line carries all of %q in:\n%s", needles, user)
	}
	lineWith("The ticket says: my invoice is wrong.")
	lineWith("Is this a billing issue?")
	lineWith("Which team handles it?")
	lineWith("How urgent is it?")
	lineWith("o0", `"billing"`, "Invoices and payments")
	lineWith("o1", "true", "Anyone")
	lineWith("o2", `"support"`)
	lineWith("l0", "low", "can wait")
	lineWith("l1", "high")
	for _, key := range []string{"q0", "q1", "q2"} {
		lineWith(key)
	}
	if strings.Index(user, "The ticket says") > strings.Index(user, "Is this a billing issue?") {
		t.Errorf("the state must come before the questions:\n%s", user)
	}

	// A JSON input is pretty-printed; an http image becomes an image part, in order.
	req, _ = mustBuildEmulation(t, `{"input":[{"role":"user","content":[
		{"type":"input_text","text":"before"},
		{"type":"input_json","data":{"a":1,"b":[true]}},
		{"type":"input_image","image_url":"https://example.com/cat.png"},
		{"type":"input_image","image_url":"data:image/png;base64,AAAA"}
	]}],"questions":[{"type":"predicate","instructions":"cat?"}]}`)
	parts := req.Messages[1].Content
	if len(parts) != 5 {
		t.Fatalf("user parts = %+v, want text, json text, two images, questions", parts)
	}
	if parts[0].Type != inference.ContentText || parts[0].Text != "before" {
		t.Errorf("part 0 = %+v, want the text", parts[0])
	}
	if parts[1].Type != inference.ContentText || parts[1].Text != "{\n  \"a\": 1,\n  \"b\": [\n    true\n  ]\n}" {
		t.Errorf("part 1 = %q, want the pretty-printed JSON", parts[1].Text)
	}
	if parts[2].Type != inference.ContentImage || parts[2].ImageURL != "https://example.com/cat.png" {
		t.Errorf("part 2 = %+v, want the http image", parts[2])
	}
	if parts[3].Type != inference.ContentImage || parts[3].ImageURL != "data:image/png;base64,AAAA" {
		t.Errorf("part 3 = %+v, want the data image", parts[3])
	}
	if parts[4].Type != inference.ContentText || !strings.Contains(parts[4].Text, "cat?") {
		t.Errorf("part 4 = %+v, want the question list", parts[4])
	}
}

func TestBuildDecisionEmulationRefuses(t *testing.T) {
	cases := []struct {
		name, body, code string
	}{
		{"unknown type", `{"input":"x","questions":[{"type":"rank"}]}`, CodeDecisionsQuestionInvalid},
		{"system input", `{"input":[{"role":"system","content":"x"}],"questions":[{"type":"predicate"}]}`, CodeDecisionsInputUnsupported},
		{"ftp image", `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"ftp://x/y.png"}]}],"questions":[{"type":"predicate"}]}`, CodeDecisionsInputUnsupported},
		{"empty input", `{"input":[],"questions":[{"type":"predicate"}]}`, CodeDecisionsInputUnsupported},
		{"no questions", `{"input":"x","questions":[]}`, CodeDecisionsQuestionInvalid},
		{"not json", `{`, CodeDecisionsInputUnsupported},
		{"choice without choices", `{"input":"x","questions":[{"type":"choice","choices":[]}]}`, CodeDecisionsQuestionInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, derr := BuildDecisionEmulation([]byte(tc.body), "m")
			if derr == nil || derr.Code != tc.code {
				t.Fatalf("refusal = %v, want %s", derr, tc.code)
			}
		})
	}
}

func TestParseDecisionEmulation(t *testing.T) {
	_, table := mustBuildEmulation(t, emulationBody)
	text := `{"answers":{"q0":{"yes":0.8},"q1":{"probabilities":{"o0":1,"o1":3,"o2":0}},"q2":{"probabilities":{"l0":0.2,"l1":0.6}}}}`
	usage := inference.Usage{InputTokens: 120, OutputTokens: 30, TotalTokens: 150, CachedTokens: 64}
	body, err := ParseDecisionEmulation(text, table, "client-model", usage)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, decodeAny(t, body), `{
		"answers": [
			{"type":"predicate","name":null,"probability":0.8},
			{"type":"choice","name":"","choice":true,"confidence":0.75,"probabilities":[
				{"value":"billing","probability":0.25},
				{"value":true,"probability":0.75},
				{"value":"support","probability":0}
			]},
			{"type":"score","name":"urgency","score":1,"confidence":0.75,"probabilities":[
				{"value":0,"label":"low","probability":0.25},
				{"value":1,"label":"high","probability":0.75}
			]}
		],
		"model":"client-model",
		"usage":{
			"input_tokens":120,
			"input_tokens_details":{"cached_tokens":64,"cache_write_tokens":0},
			"output_tokens":30,
			"output_tokens_details":{"reasoning_tokens":0},
			"total_tokens":150
		}
	}`)
}

func TestParseDecisionEmulationFenceAndErrors(t *testing.T) {
	_, table := mustBuildEmulation(t, emulationBody)
	good := `{"answers":{"q0":{"yes":0.8},"q1":{"probabilities":{"o0":1,"o1":3,"o2":0}},"q2":{"probabilities":{"l0":0.2,"l1":0.6}}}}`

	t.Run("fenced", func(t *testing.T) {
		if _, err := ParseDecisionEmulation("```json\n"+good+"\n```", table, "m", inference.Usage{}); err != nil {
			t.Fatalf("fenced body: %v", err)
		}
		if _, err := ParseDecisionEmulation("```\n"+good+"\n```\n", table, "m", inference.Usage{}); err != nil {
			t.Fatalf("bare fence: %v", err)
		}
	})
	t.Run("prose before", func(t *testing.T) {
		if _, err := ParseDecisionEmulation("Here you go: "+good, table, "m", inference.Usage{}); err == nil {
			t.Fatal("prose before the JSON parsed, want an error")
		}
	})
	t.Run("clamping", func(t *testing.T) {
		body, err := ParseDecisionEmulation(`{"answers":{"q0":{"yes":2},"q1":{"probabilities":{"o0":-1,"o1":2,"o2":0}},"q2":{"probabilities":{"l0":-1,"l1":2}}}}`, table, "m", inference.Usage{})
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Answers []map[string]any `json:"answers"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		if out.Answers[0]["probability"] != 1.0 {
			t.Errorf("predicate = %v, want 1 (clamped)", out.Answers[0]["probability"])
		}
		if out.Answers[1]["choice"] != true || out.Answers[1]["confidence"] != 1.0 {
			t.Errorf("choice = %v, want true with confidence 1", out.Answers[1])
		}
		if out.Answers[2]["score"] != 1.0 || out.Answers[2]["confidence"] != 1.0 {
			t.Errorf("score = %v, want 1 with confidence 1", out.Answers[2])
		}
		body, err = ParseDecisionEmulation(`{"answers":{"q0":{"yes":-1},"q1":{"probabilities":{"o0":1,"o1":0,"o2":0}},"q2":{"probabilities":{"l0":1,"l1":0}}}}`, table, "m", inference.Usage{})
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		if out.Answers[0]["probability"] != 0.0 {
			t.Errorf("predicate = %v, want 0 (clamped)", out.Answers[0]["probability"])
		}
	})
	t.Run("all-zero choice", func(t *testing.T) {
		_, err := ParseDecisionEmulation(`{"answers":{"q0":{"yes":0.5},"q1":{"probabilities":{"o0":0,"o1":0,"o2":0}},"q2":{"probabilities":{"l0":1,"l1":0}}}}`, table, "m", inference.Usage{})
		if err == nil || !strings.Contains(err.Error(), "q1") {
			t.Fatalf("err = %v, want an error naming q1", err)
		}
	})
	t.Run("missing question", func(t *testing.T) {
		_, err := ParseDecisionEmulation(`{"answers":{"q0":{"yes":0.5},"q1":{"probabilities":{"o0":1,"o1":0,"o2":0}}}}`, table, "m", inference.Usage{})
		if err == nil || !strings.Contains(err.Error(), "q2") {
			t.Fatalf("err = %v, want an error naming q2", err)
		}
	})
	t.Run("missing option", func(t *testing.T) {
		_, err := ParseDecisionEmulation(`{"answers":{"q0":{"yes":0.5},"q1":{"probabilities":{"o0":1,"o1":0}},"q2":{"probabilities":{"l0":1,"l1":0}}}}`, table, "m", inference.Usage{})
		if err == nil || !strings.Contains(err.Error(), "o2") {
			t.Fatalf("err = %v, want an error naming o2", err)
		}
	})
	t.Run("tie picks the first option", func(t *testing.T) {
		body, err := ParseDecisionEmulation(`{"answers":{"q0":{"yes":0.5},"q1":{"probabilities":{"o0":0,"o1":0.5,"o2":0.5}},"q2":{"probabilities":{"l0":0.5,"l1":0.5}}}}`, table, "m", inference.Usage{})
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Answers []map[string]any `json:"answers"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		if out.Answers[1]["choice"] != true {
			t.Errorf("choice = %v, want true (o1, the first of the tied options)", out.Answers[1]["choice"])
		}
		if out.Answers[2]["score"] != 0.0 {
			t.Errorf("score = %v, want 0 (the first of the tied levels)", out.Answers[2]["score"])
		}
	})
	t.Run("rounding", func(t *testing.T) {
		body, err := ParseDecisionEmulation(`{"answers":{"q0":{"yes":0.1},"q1":{"probabilities":{"o0":1,"o1":1,"o2":1}},"q2":{"probabilities":{"l0":0.1,"l1":0.2}}}}`, table, "m", inference.Usage{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `"probability":0.333333333333`) || strings.Contains(string(body), "0.3333333333333") {
			t.Errorf("choice probabilities not rounded to 12 decimals: %s", body)
		}
	})
}
