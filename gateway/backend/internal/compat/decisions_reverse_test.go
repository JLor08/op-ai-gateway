// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package compat

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func reverseOK(t *testing.T, in string) (string, SystemOneAnswerTable) {
	t.Helper()
	body, table, derr := TranslateSystemOneRequest([]byte(in), "gpt-6-luna")
	if derr != nil {
		t.Fatalf("unexpected error %s: %s", derr.Code, derr.Message)
	}
	return string(body), table
}

func TestSystemOneRequestInputAndQuestions(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string // exact JSON, compared as values
	}{
		{
			"string state", `{"model":"m","state":"hello","questions":{"a":{"type":"noul"}}}`,
			`{"model":"gpt-6-luna","input":"hello","questions":[{"type":"predicate","name":"a","instructions":""}]}`,
		},
		{
			"object state", `{"model":"m","state":{"k":1},"questions":{"a":{"type":"noul"}}}`,
			`{"model":"gpt-6-luna","input":[{"type":"message","role":"user","content":[{"type":"input_json","data":{"k":1}}]}],"questions":[{"type":"predicate","name":"a","instructions":""}]}`,
		},
		{
			"array state", `{"model":"m","state":[{"a":1},{"b":"x"}],"questions":{"a":{"type":"noul"}}}`,
			`{"model":"gpt-6-luna","input":[{"type":"message","role":"user","content":[{"type":"input_json","data":[{"a":1},{"b":"x"}]}]}],"questions":[{"type":"predicate","name":"a","instructions":""}]}`,
		},
		{
			"object state with images", `{"model":"m","state":{"k":1},"images":["data:image/png;base64,AAA","https://x.test/i.png"],"questions":{"a":{"type":"noul"}}}`,
			`{"model":"gpt-6-luna","input":[{"type":"message","role":"user","content":[{"type":"input_json","data":{"k":1}},{"type":"input_image","image_url":"data:image/png;base64,AAA"},{"type":"input_image","image_url":"https://x.test/i.png"}]}],"questions":[{"type":"predicate","name":"a","instructions":""}]}`,
		},
		{
			"string state with images", `{"model":"m","state":"hi","images":["https://x.test/i.png"],"questions":{"a":{"type":"noul"}}}`,
			`{"model":"gpt-6-luna","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"},{"type":"input_image","image_url":"https://x.test/i.png"}]}],"questions":[{"type":"predicate","name":"a","instructions":""}]}`,
		},
		{
			"noul with instructions", `{"model":"m","state":"s","questions":{"a":{"type":"noul","instructions":"Is it nice?"}}}`,
			`{"model":"gpt-6-luna","input":"s","questions":[{"type":"predicate","name":"a","instructions":"Is it nice?"}]}`,
		},
		{
			"noul null criteria", `{"model":"m","state":"s","questions":{"a":{"type":"noul","instructions":"Q","criteria":null}}}`,
			`{"model":"gpt-6-luna","input":"s","questions":[{"type":"predicate","name":"a","instructions":"Q"}]}`,
		},
		{
			"noul both folded", `{"model":"m","state":"s","questions":{"a":{"type":"noul","instructions":"Q","criteria":{"true":"A","false":"B"}}}}`,
			`{"model":"gpt-6-luna","input":"s","questions":[{"type":"predicate","name":"a","instructions":"Q\n\nAnswer yes if: A\n\nAnswer no if: B"}]}`,
		},
		{
			"noul only yes", `{"model":"m","state":"s","questions":{"a":{"type":"noul","instructions":"Q","criteria":{"true":"A","false":null}}}}`,
			`{"model":"gpt-6-luna","input":"s","questions":[{"type":"predicate","name":"a","instructions":"Q\n\nAnswer yes if: A"}]}`,
		},
		{
			"noul only no, no instructions", `{"model":"m","state":"s","questions":{"a":{"type":"noul","criteria":{"false":"B"}}}}`,
			`{"model":"gpt-6-luna","input":"s","questions":[{"type":"predicate","name":"a","instructions":"\n\nAnswer no if: B"}]}`,
		},
		{
			"choice with null description", `{"model":"m","state":"s","questions":{"c":{"type":"choice","instructions":"Q","criteria":{"z":"Zed","a":null,"m":"Em"}}}}`,
			`{"model":"gpt-6-luna","input":"s","questions":[{"type":"choice","name":"c","instructions":"Q","choices":[{"value":"z","description":"Zed"},{"value":"a"},{"value":"m","description":"Em"}]}]}`,
		},
		{
			"score levels", `{"model":"m","state":"s","questions":{"s":{"type":"score","instructions":"Q","criteria":["low","mid",3]}}}`,
			`{"model":"gpt-6-luna","input":"s","questions":[{"type":"score","name":"s","instructions":"Q","levels":[{"label":"low"},{"label":"mid"},{"label":"3"}]}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := reverseOK(t, tt.in)
			assertJSONEqual(t, decodeAny(t, []byte(body)), tt.want)
		})
	}
}

func TestSystemOneRequestKeepsObjectOrder(t *testing.T) {
	keys := []string{"zulu", "alpha", "mike", "bravo", "yankee", "charlie", "x-ray", "delta", "whiskey", "echo", "victor", "foxtrot", "10", "2"}
	var qs []string
	for _, k := range keys {
		qs = append(qs, fmt.Sprintf(`%q:{"type":"noul"}`, k))
	}
	body, table := reverseOK(t, `{"model":"m","state":"s","questions":{`+strings.Join(qs, ",")+`}}`)
	last := -1
	for _, k := range keys {
		i := strings.Index(body, fmt.Sprintf(`"name":%q`, k))
		if i < 0 || i < last {
			t.Fatalf("question %q out of order or missing in %s", k, body)
		}
		last = i
	}
	for i, k := range keys {
		if table.questions[i].key != k {
			t.Fatalf("table[%d] = %q, want %q", i, table.questions[i].key, k)
		}
	}
}

func TestSystemOneRequestChoiceOrder(t *testing.T) {
	body, _ := reverseOK(t, `{"model":"m","state":"s","questions":{"c":{"type":"choice","criteria":{"z":null,"y":null,"a":null,"b":null}}}}`)
	if !strings.Contains(body, `"choices":[{"value":"z"},{"value":"y"},{"value":"a"},{"value":"b"}]`) {
		t.Fatalf("choices not in key order: %s", body)
	}
}

func TestSystemOneRequestRefusals(t *testing.T) {
	q := `"questions":{"a":{"type":"noul"}}`
	tests := []struct {
		name string
		in   string
		code string
	}{
		{"top-level instructions", `{"model":"m","state":"s","instructions":"x",` + q + `}`, CodeSystemOneFieldUnsupported},
		{"box", `{"model":"m","state":"s","questions":{"a":{"type":"box"}}}`, CodeSystemOneQuestionTypeUnsupported},
		{"set", `{"model":"m","state":"s","questions":{"a":{"type":"set"}}}`, CodeSystemOneQuestionTypeUnsupported},
		{"span", `{"model":"m","state":"s","questions":{"a":{"type":"span"}}}`, CodeSystemOneQuestionTypeUnsupported},
		{"track", `{"model":"m","state":"s","questions":{"a":{"type":"track"}}}`, CodeSystemOneQuestionTypeUnsupported},
		{"unknown type", `{"model":"m","state":"s","questions":{"a":{"type":"wat"}}}`, CodeSystemOneQuestionTypeUnsupported},
		{"missing type", `{"model":"m","state":"s","questions":{"a":{}}}`, CodeSystemOneQuestionTypeUnsupported},
		{"assets", `{"model":"m","state":"s","assets":[],` + q + `}`, CodeSystemOneFieldUnsupported},
		{"audio", `{"model":"m","state":"s","audio":["a"],` + q + `}`, CodeSystemOneFieldUnsupported},
		{"videos", `{"model":"m","state":"s","videos":["a"],` + q + `}`, CodeSystemOneFieldUnsupported},
		{"roles", `{"model":"m","state":"s","roles":{},` + q + `}`, CodeSystemOneFieldUnsupported},
		{"options", `{"model":"m","state":"s","options":{},` + q + `}`, CodeSystemOneFieldUnsupported},
		{"preset", `{"model":"m","state":"s","preset":"p",` + q + `}`, CodeSystemOneFieldUnsupported},
		{"states", `{"model":"m","state":"s","states":[],` + q + `}`, CodeSystemOneFieldUnsupported},
		{"joint", `{"model":"m","state":"s","joint":true,` + q + `}`, CodeSystemOneFieldUnsupported},
		{"chat_template_kwargs", `{"model":"m","state":"s","chat_template_kwargs":{},` + q + `}`, CodeSystemOneFieldUnsupported},
		{"cache_salt", `{"model":"m","state":"s","cache_salt":"x",` + q + `}`, CodeSystemOneFieldUnsupported},
		{"priority", `{"model":"m","state":"s","priority":1,` + q + `}`, CodeSystemOneFieldUnsupported},
		{"request_id", `{"model":"m","state":"s","request_id":"r",` + q + `}`, CodeSystemOneFieldUnsupported},
		{"not json", `nope`, CodeSystemOneFieldUnsupported},
		{"null body", `null`, CodeSystemOneFieldUnsupported},
		{"missing state", `{"model":"m",` + q + `}`, CodeSystemOneFieldUnsupported},
		{"missing questions", `{"model":"m","state":"s"}`, CodeSystemOneFieldUnsupported},
		{"empty questions", `{"model":"m","state":"s","questions":{}}`, CodeSystemOneFieldUnsupported},
		{"questions array", `{"model":"m","state":"s","questions":[]}`, CodeSystemOneFieldUnsupported},
		{"bad images", `{"model":"m","state":"s","images":[1],` + q + `}`, CodeSystemOneFieldUnsupported},
		{"choice without criteria", `{"model":"m","state":"s","questions":{"a":{"type":"choice"}}}`, CodeSystemOneFieldUnsupported},
		{"score without criteria", `{"model":"m","state":"s","questions":{"a":{"type":"score"}}}`, CodeSystemOneFieldUnsupported},
		{"noul bad key", `{"model":"m","state":"s","questions":{"a":{"type":"noul","criteria":{"maybe":"x"}}}}`, CodeSystemOneFieldUnsupported},
		{"non-string instructions", `{"model":"m","state":"s","questions":{"a":{"type":"noul","instructions":[1]}}}`, CodeSystemOneFieldUnsupported},
		{"null image", `{"model":"m","state":"s","images":[null],` + q + `}`, CodeSystemOneFieldUnsupported},
		{"empty image", `{"model":"m","state":"s","images":[""],` + q + `}`, CodeSystemOneFieldUnsupported},
		{"duplicate noul key", `{"model":"m","state":"s","questions":{"a":{"type":"noul","criteria":{"true":"A","true":"B"}}}}`, CodeSystemOneFieldUnsupported},
		{"number state", `{"model":"m","state":5,` + q + `}`, CodeSystemOneFieldUnsupported},
		{"bool state", `{"model":"m","state":true,` + q + `}`, CodeSystemOneFieldUnsupported},
		{"array state with scalar", `{"model":"m","state":[1,"x"],` + q + `}`, CodeSystemOneFieldUnsupported},
		{"null question", `{"model":"m","state":"s","questions":{"a":null}}`, CodeSystemOneFieldUnsupported},
		{"duplicate question key", `{"model":"m","state":"s","questions":{"a":{"type":"noul"},"a":{"type":"noul"}}}`, CodeSystemOneFieldUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, derr := TranslateSystemOneRequest([]byte(tt.in), "gpt-6-luna")
			if derr == nil {
				t.Fatalf("expected %s, got no error", tt.code)
			}
			if derr.Code != tt.code {
				t.Fatalf("code = %s, want %s (%s)", derr.Code, tt.code, derr.Message)
			}
		})
	}
}

const reverseReq = `{"model":"m","state":"s","questions":{
	"nice":{"type":"noul"},
	"mood":{"type":"choice","criteria":{"calm":null,"angry":null,"glad":null}},
	"urgency":{"type":"score","criteria":["low","mid","high"]}}}`

const reverseResp = `{"answers":[
	{"type":"predicate","name":"nice","probability":0.8},
	{"type":"choice","name":"mood","choice":"angry","confidence":0.6,"probabilities":[{"value":"calm","probability":0.3},{"value":"angry","probability":0.6},{"value":"glad","probability":0.1}]},
	{"type":"score","name":"urgency","score":1.7,"confidence":0.7,"probabilities":[{"value":0,"label":"low","probability":0.1},{"value":1,"label":"mid","probability":0.1},{"value":2,"label":"high","probability":0.8}]}],
	"model":"gpt-6-luna","usage":{"input_tokens":120,"input_tokens_details":{"cached_tokens":64,"cache_write_tokens":16},"output_tokens":9,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":129}}`

const reverseWant = `{"answers":{
	"nice":{"type":"noul","noul":0.8,"probabilities":{"yes":0.8,"no":0.2},"confidence":0.8},
	"mood":{"type":"choice","choice":"angry","probabilities":{"calm":0.3,"angry":0.6,"glad":0.1},"confidence":0.6},
	"urgency":{"type":"score","score":1.7,"legend":{"0":"low","1":"mid","2":"high"},"probabilities":{"0":0.1,"1":0.1,"2":0.8},"confidence":0.7}},
	"model":"client-model","usage":{"input_tokens":120,"output_tokens":9}}`

func TestOpenAIDecisionsResponseNamedAnswers(t *testing.T) {
	_, table := reverseOK(t, reverseReq)
	out, usage, err := TranslateOpenAIDecisionsResponse([]byte(reverseResp), table, "client-model")
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, decodeAny(t, out), reverseWant)
	if want := (DecisionsUsage{InputTokens: 120, OutputTokens: 9, CachedTokens: 64, CacheWriteTokens: 16}); usage != want {
		t.Fatalf("usage = %+v, want %+v", usage, want)
	}
	// answers follow question order, not the upstream's
	s := string(out)
	if !(strings.Index(s, `"nice"`) < strings.Index(s, `"mood"`) && strings.Index(s, `"mood"`) < strings.Index(s, `"urgency"`)) {
		t.Fatalf("answers not in question order: %s", s)
	}
}

func TestOpenAIDecisionsResponseReorderedNamedAnswers(t *testing.T) {
	_, table := reverseOK(t, `{"model":"m","state":"s","questions":{"a":{"type":"noul"},"b":{"type":"noul"}}}`)
	body := `{"answers":[{"type":"predicate","name":"b","probability":0.25},{"type":"predicate","name":"a","probability":1}],"model":"x","usage":{}}`
	out, _, err := TranslateOpenAIDecisionsResponse([]byte(body), table, "c")
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, decodeAny(t, out), `{"answers":{"a":{"type":"noul","noul":1,"probabilities":{"yes":1,"no":0},"confidence":1},"b":{"type":"noul","noul":0.25,"probabilities":{"yes":0.25,"no":0.75},"confidence":0.75}},"model":"c","usage":{"input_tokens":0,"output_tokens":0}}`)
}

func TestOpenAIDecisionsResponsePositionalAnswers(t *testing.T) {
	_, table := reverseOK(t, `{"model":"m","state":"s","questions":{"a":{"type":"noul"},"b":{"type":"noul"}}}`)
	body := `{"answers":[{"type":"predicate","name":null,"probability":0.5},{"type":"predicate","probability":0.1}],"model":"x","usage":{}}`
	out, _, err := TranslateOpenAIDecisionsResponse([]byte(body), table, "c")
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, decodeAny(t, out), `{"answers":{"a":{"type":"noul","noul":0.5,"probabilities":{"yes":0.5,"no":0.5},"confidence":0.5},"b":{"type":"noul","noul":0.1,"probabilities":{"yes":0.1,"no":0.9},"confidence":0.9}},"model":"c","usage":{"input_tokens":0,"output_tokens":0}}`)
}

func TestOpenAIDecisionsResponseRefusal(t *testing.T) {
	_, table := reverseOK(t, reverseReq)
	body := `{"answers":[{"type":"refusal","name":"nice"},{"type":"choice","name":"mood","choice":"calm","confidence":1,"probabilities":[]},{"type":"refusal","name":"urgency"}],"model":"x","usage":{}}`
	_, _, err := TranslateOpenAIDecisionsResponse([]byte(body), table, "c")
	if !errors.Is(err, ErrSystemOneRefused) {
		t.Fatalf("err = %v, want ErrSystemOneRefused", err)
	}
	var re *SystemOneRefusedError
	if !errors.As(err, &re) || !reflect.DeepEqual(re.Keys, []string{"nice", "urgency"}) {
		t.Fatalf("refused keys = %+v", re)
	}
}

func TestOpenAIDecisionsResponseErrors(t *testing.T) {
	_, table := reverseOK(t, reverseReq)
	tests := []struct{ name, body string }{
		{"not json", `nope`},
		{"empty", ``},
		{"null", `null`},
		{"no answers", `{"model":"x"}`},
		{"answers object", `{"answers":{}}`},
		{"answers null", `{"answers":null}`},
		{"answer not object", `{"answers":[1,2,3]}`},
		{"missing answer", `{"answers":[{"type":"predicate","name":"nice","probability":0.5}]}`},
		{"empty answers", `{"answers":[]}`},
		{"wrong type", `{"answers":[{"type":"choice","name":"nice"},{},{}]}`},
		{"predicate no probability", `{"answers":[{"type":"predicate","name":"nice"},{},{}]}`},
		{"predicate out of range", `{"answers":[{"type":"predicate","name":"nice","probability":1.5},{},{}]}`},
		{"choice unknown", `{"answers":[{"type":"predicate","name":"nice","probability":0.5},{"type":"choice","name":"mood","choice":"meh","probabilities":[]},{}]}`},
		{"choice probabilities not array", `{"answers":[{"type":"predicate","name":"nice","probability":0.5},{"type":"choice","name":"mood","choice":"calm","probabilities":{}},{}]}`},
		{"choice unknown probability value", `{"answers":[{"type":"predicate","name":"nice","probability":0.5},{"type":"choice","name":"mood","choice":"calm","probabilities":[{"value":"zzz","probability":1}]},{}]}`},
		{"score no score", `{"answers":[{"type":"predicate","name":"nice","probability":0.5},{"type":"choice","name":"mood","choice":"calm","probabilities":[]},{"type":"score","name":"urgency","probabilities":[]}]}`},
		{"score index out of range", `{"answers":[{"type":"predicate","name":"nice","probability":0.5},{"type":"choice","name":"mood","choice":"calm","probabilities":[]},{"type":"score","name":"urgency","score":1,"probabilities":[{"value":9,"label":"x","probability":1}]}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := TranslateOpenAIDecisionsResponse([]byte(tt.body), table, "c")
			if err == nil {
				t.Fatal("expected an error")
			}
			if errors.Is(err, ErrSystemOneRefused) {
				t.Fatalf("malformed body must not read as a refusal: %v", err)
			}
		})
	}
}

func TestOpenAIDecisionsResponseConfidenceFallback(t *testing.T) {
	_, table := reverseOK(t, `{"model":"m","state":"s","questions":{"mood":{"type":"choice","criteria":{"calm":null,"glad":null}}}}`)
	body := `{"answers":[{"type":"choice","name":"mood","choice":"glad","probabilities":[{"value":"calm","probability":0.25},{"value":"glad","probability":0.75}]}],"model":"x","usage":{}}`
	out, _, err := TranslateOpenAIDecisionsResponse([]byte(body), table, "c")
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, decodeAny(t, out), `{"answers":{"mood":{"type":"choice","choice":"glad","probabilities":{"calm":0.25,"glad":0.75},"confidence":0.75}},"model":"c","usage":{"input_tokens":0,"output_tokens":0}}`)
}

func TestOpenAIDecisionsResponseBooleanChoiceValue(t *testing.T) {
	_, table := reverseOK(t, `{"model":"m","state":"s","questions":{"q":{"type":"choice","criteria":{"true":null,"false":null}}}}`)
	body := `{"answers":[{"type":"choice","name":"q","choice":true,"confidence":0.9,"probabilities":[{"value":true,"probability":0.9},{"value":false,"probability":0.1}]}],"model":"x","usage":{}}`
	out, _, err := TranslateOpenAIDecisionsResponse([]byte(body), table, "c")
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, decodeAny(t, out), `{"answers":{"q":{"type":"choice","choice":"true","probabilities":{"true":0.9,"false":0.1},"confidence":0.9}},"model":"c","usage":{"input_tokens":0,"output_tokens":0}}`)
}

func TestOpenAIDecisionsResponseRefusalOutranksMalformedSibling(t *testing.T) {
	_, table := reverseOK(t, reverseReq)
	body := `{"answers":[{"type":"refusal","name":"nice"},{"type":"choice","name":"mood","choice":"meh","probabilities":[]}],"model":"x","usage":{}}`
	_, _, err := TranslateOpenAIDecisionsResponse([]byte(body), table, "c")
	var re *SystemOneRefusedError
	if !errors.Is(err, ErrSystemOneRefused) || !errors.As(err, &re) || !reflect.DeepEqual(re.Keys, []string{"nice"}) {
		t.Fatalf("err = %v, want a refusal naming nice", err)
	}
}
