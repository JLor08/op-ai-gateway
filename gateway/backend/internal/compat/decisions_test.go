// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package compat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

// decodeAny decodes JSON into a generic value for value-level comparison.
func decodeAny(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("invalid JSON %s: %v", raw, err)
	}
	return v
}

func mustTranslate(t *testing.T, in string) (map[string]any, []byte, DecisionsAnswerTable) {
	t.Helper()
	body, table, derr := TranslateDecisionsRequest([]byte(in), "up-model")
	if derr != nil {
		t.Fatalf("unexpected error %s: %s", derr.Code, derr.Message)
	}
	m, ok := decodeAny(t, body).(map[string]any)
	if !ok {
		t.Fatalf("body is not an object: %s", body)
	}
	return m, body, table
}

func assertJSONEqual(t *testing.T, got any, wantJSON string) {
	t.Helper()
	var want any
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatalf("bad want JSON %s: %v", wantJSON, err)
	}
	if !reflect.DeepEqual(got, want) {
		g, _ := json.Marshal(got)
		t.Fatalf("got %s\nwant %s", g, wantJSON)
	}
}

const onePredicate = `"questions":[{"type":"predicate","instructions":"q?"}]`

func TestTranslateDecisionsRequestState(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"string", `"hello"`, `"hello"`},
		{"single user message string", `[{"role":"user","content":"hi"}]`, `"hi"`},
		{"typed message item", `[{"type":"message","role":"user","content":"hi"}]`, `"hi"`},
		{"two user messages joined", `[{"role":"user","content":"a"},{"role":"user","content":"b"}]`, `"a\n\nb"`},
		{"text parts joined", `[{"role":"user","content":[{"type":"input_text","text":"a"},{"type":"input_text","text":"b"}]}]`, `"a\n\nb"`},
		{"lone input_json", `[{"role":"user","content":[{"type":"input_json","data":{"k":1}}]}]`, `{"k":1}`},
		{"json plus text", `[{"role":"user","content":[{"type":"input_json","data":{"k":1}},{"type":"input_text","text":"note"}]}]`, `[{"k":1},"note"]`},
		{"text then json", `[{"role":"user","content":[{"type":"input_text","text":"note"},{"type":"input_json","data":[{"a":1}]}]}]`, `["note",[{"a":1}]]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _, _ := mustTranslate(t, `{"model":"m","input":`+tc.input+`,`+onePredicate+`}`)
			assertJSONEqual(t, m["state"], tc.want)
			if m["model"] != "up-model" {
				t.Fatalf("model = %v", m["model"])
			}
		})
	}
}

func TestTranslateDecisionsRequestImagesAndDrops(t *testing.T) {
	in := `{"model":"m","prompt_cache_key":"k","prompt_cache_options":{"mode":"implicit"},"safety_identifier":"s","stream":false,"junk":1,
	"input":[{"role":"user","content":[
		{"type":"input_image","image_url":"data:image/png;base64,AAA","detail":"high","prompt_cache_breakpoint":{"mode":"explicit"}},
		{"type":"input_text","text":"x"},
		{"type":"input_image","image_url":"data:image/png;base64,BBB"}]}],` + onePredicate + `}`
	m, body, _ := mustTranslate(t, in)
	assertJSONEqual(t, m["images"], `["data:image/png;base64,AAA","data:image/png;base64,BBB"]`)
	assertJSONEqual(t, m["state"], `"x"`)
	for _, k := range []string{"prompt_cache_key", "prompt_cache_options", "safety_identifier", "stream", "junk", "detail"} {
		if strings.Contains(string(body), k) {
			t.Fatalf("%s leaked into body: %s", k, body)
		}
	}
	m2, _, _ := mustTranslate(t, `{"model":"m","input":"x",`+onePredicate+`}`)
	if _, ok := m2["images"]; ok {
		t.Fatalf("images present without images")
	}
}

func TestTranslateDecisionsRequestInputRefusals(t *testing.T) {
	cases := map[string]string{
		"https image":   `[{"role":"user","content":[{"type":"input_image","image_url":"https://x/y.png"}]}]`,
		"file_id image": `[{"role":"user","content":[{"type":"input_image","file_id":"file-1"}]}]`,
		"system role":   `[{"role":"system","content":"s"}]`,
		"developer":     `[{"role":"developer","content":"s"}]`,
		"assistant":     `[{"role":"assistant","content":"s"}]`,
		"function_call": `[{"type":"function_call","call_id":"c","name":"n","arguments":"{}"}]`,
		"other item":    `[{"type":"custom_tool_call_output","call_id":"c","output":"o"}]`,
		"unknown part":  `[{"role":"user","content":[{"type":"input_audio"}]}]`,
		"input number":  `5`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, derr := TranslateDecisionsRequest([]byte(`{"model":"m","input":`+input+`,`+onePredicate+`}`), "u")
			if derr == nil || derr.Code != "decisions.input_unsupported" {
				t.Fatalf("got %+v", derr)
			}
		})
	}
}

func TestTranslateDecisionsRequestQuestionOrder(t *testing.T) {
	var qs []string
	var names []string
	for i := 0; i < 12; i++ {
		n := string(rune('z' - i))
		names = append(names, n)
		qs = append(qs, fmt.Sprintf(`{"type":"predicate","name":%q,"instructions":"i"}`, n))
	}
	_, body, _ := mustTranslate(t, `{"model":"m","input":"x","questions":[`+strings.Join(qs, ",")+`]}`)
	last := -1
	for _, n := range names {
		idx := bytes.Index(body, []byte(fmt.Sprintf(`"%s":{"type":"noul"`, n)))
		if idx < 0 || idx < last {
			t.Fatalf("question %s out of order (idx %d, last %d) in %s", n, idx, last, body)
		}
		last = idx
	}
}

func TestTranslateDecisionsRequestNaming(t *testing.T) {
	cases := []struct {
		name string
		qs   string
		keys []string
	}{
		{"missing and null", `[{"type":"predicate","instructions":"i"},{"type":"predicate","name":null,"instructions":"i"},{"type":"predicate","name":"","instructions":"i"}]`, []string{"q0", "q1", "q2"}},
		{"duplicates", `[{"type":"predicate","name":"a","instructions":"i"},{"type":"predicate","name":"a","instructions":"i"},{"type":"predicate","name":"a","instructions":"i"}]`, []string{"a", "a-2", "a-3"}},
		{"synthetic collision", `[{"type":"predicate","instructions":"i"},{"type":"predicate","name":"q0","instructions":"i"}]`, []string{"q0", "q0-2"}},
		{"explicit before synthetic", `[{"type":"predicate","name":"q1","instructions":"i"},{"type":"predicate","instructions":"i"}]`, []string{"q1", "q1-2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _, table := mustTranslate(t, `{"model":"m","input":"x","questions":`+tc.qs+`}`)
			qm := m["questions"].(map[string]any)
			if len(qm) != len(tc.keys) {
				t.Fatalf("keys = %v", qm)
			}
			for i, k := range tc.keys {
				if _, ok := qm[k]; !ok {
					t.Fatalf("missing key %s in %v", k, qm)
				}
				if table.questions[i].key != k {
					t.Fatalf("table key[%d] = %s", i, table.questions[i].key)
				}
			}
		})
	}
}

func TestTranslateDecisionsRequestQuestionTypes(t *testing.T) {
	cases := []struct {
		name string
		q    string
		want string
	}{
		{"predicate", `{"type":"predicate","name":"p","instructions":"is it?"}`, `{"type":"noul","instructions":"is it?"}`},
		{
			"choice string+bool", `{"type":"choice","name":"p","instructions":"i","choices":[{"value":"a","description":"desc"},{"value":"b"},{"value":true,"description":""},{"value":false,"description":"no"}]}`,
			`{"type":"choice","instructions":"i","criteria":{"a":"desc","b":null,"true":null,"false":"no"}}`,
		},
		{
			"score", `{"type":"score","name":"p","instructions":"i","levels":[{"label":"low"},{"label":"mid","description":"so so"},{"label":"hi","description":""}]}`,
			`{"type":"score","instructions":"i","criteria":["low","mid: so so","hi"]}`,
		},
		{"instruction parts", `{"type":"predicate","name":"p","instructions":[{"type":"input_text","text":"a"},{"type":"input_text","text":"b"}]}`, `{"type":"noul","instructions":"a\n\nb"}`},
		{"missing instructions", `{"type":"predicate","name":"p"}`, `{"type":"noul","instructions":""}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _, _ := mustTranslate(t, `{"model":"m","input":"x","questions":[`+tc.q+`]}`)
			assertJSONEqual(t, m["questions"].(map[string]any)["p"], tc.want)
		})
	}
}

func TestTranslateDecisionsRequestQuestionRefusals(t *testing.T) {
	cases := []struct{ name, q, code string }{
		{"bool/string collision", `{"type":"choice","instructions":"i","choices":[{"value":"true"},{"value":true}]}`, "decisions.question_invalid"},
		{"duplicate string", `{"type":"choice","instructions":"i","choices":[{"value":"a"},{"value":"a"}]}`, "decisions.question_invalid"},
		{"unknown type", `{"type":"box","instructions":"i"}`, "decisions.question_invalid"},
		{"choice without choices", `{"type":"choice","instructions":"i"}`, "decisions.question_invalid"},
		{"choice empty", `{"type":"choice","instructions":"i","choices":[]}`, "decisions.question_invalid"},
		{"score without levels", `{"type":"score","instructions":"i"}`, "decisions.question_invalid"},
		{"choice not object", `{"type":"choice","instructions":"i","choices":["a"]}`, "decisions.question_invalid"},
		{"level not object", `{"type":"score","instructions":"i","levels":["a"]}`, "decisions.question_invalid"},
		{"choice numeric value", `{"type":"choice","instructions":"i","choices":[{"value":3}]}`, "decisions.question_invalid"},
		{"question not object", `"str"`, "decisions.question_invalid"},
		{"non-string name", `{"type":"predicate","name":5,"instructions":"i"}`, "decisions.question_invalid"},
		{"image in instructions", `{"type":"predicate","instructions":[{"type":"input_image","image_url":"data:image/png;base64,A"}]}`, "decisions.input_unsupported"},
		{"json in instructions", `{"type":"predicate","instructions":[{"type":"input_json","data":{}}]}`, "decisions.input_unsupported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, derr := TranslateDecisionsRequest([]byte(`{"model":"m","input":"x","questions":[`+tc.q+`]}`), "u")
			if derr == nil || derr.Code != tc.code {
				t.Fatalf("got %+v want %s", derr, tc.code)
			}
		})
	}
}

// translateFixture is a request with one question of each type, used by the
// response tests.
const translateFixture = `{"model":"client","input":"x","questions":[
	{"type":"predicate","name":"p","instructions":"i"},
	{"type":"choice","instructions":"i","choices":[{"value":"a","description":"A"},{"value":true},{"value":false}]},
	{"type":"score","name":"s","instructions":"i","levels":[{"label":"low","description":"very low"},{"label":"high"}]}
]}`

func translateResp(t *testing.T, fixture, upstream string) (map[string]any, DecisionsUsage, error) {
	t.Helper()
	_, _, table := mustTranslate(t, fixture)
	out, usage, err := TranslateSystemOneResponse([]byte(upstream), table, "client-model")
	if err != nil {
		return nil, usage, err
	}
	return decodeAny(t, out).(map[string]any), usage, nil
}

const fullUpstream = `{"id":"decision-1","object":"structured_decision","created":5,"model":"up","diagnostics":{"p":{}},"joint":null,"meta":{},
 "answers":{
  "q1":{"type":"choice","choice":"true","confidence":0.5,"probabilities":{"a":0.2,"true":0.5,"false":0.3}},
  "p":{"type":"noul","noul":0.9,"probabilities":{"yes":0.9,"no":0.1},"confidence":0.9},
  "s":{"type":"score","score":0.7,"legend":{"0":"low: very low","1":"high"},"probabilities":{"0":0.3,"1":0.7},"confidence":0.7}},
 "usage":{"input_tokens":10,"output_tokens":3}}`

func TestTranslateSystemOneResponseFull(t *testing.T) {
	m, usage, err := translateResp(t, translateFixture, fullUpstream)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"client-model","answers":[
	 {"type":"predicate","name":"p","probability":0.9},
	 {"type":"choice","name":null,"choice":true,"confidence":0.5,"probabilities":[{"value":"a","probability":0.2},{"value":true,"probability":0.5},{"value":false,"probability":0.3}]},
	 {"type":"score","name":"s","score":0.7,"confidence":0.7,"probabilities":[{"value":0,"label":"low","probability":0.3},{"value":1,"label":"high","probability":0.7}]}],
	 "usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"output_tokens":3,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":13}}`
	assertJSONEqual(t, m, want)
	if usage != (DecisionsUsage{InputTokens: 10, OutputTokens: 3}) {
		t.Fatalf("usage = %+v", usage)
	}
	for _, k := range []string{"id", "object", "created", "diagnostics", "joint", "meta"} {
		if _, ok := m[k]; ok {
			t.Fatalf("%s should be dropped", k)
		}
	}
}

func TestTranslateSystemOneResponseNameNullBytes(t *testing.T) {
	_, _, table := mustTranslate(t, translateFixture)
	out, _, err := TranslateSystemOneResponse([]byte(fullUpstream), table, "c")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"name":null`)) {
		t.Fatalf("name null not written: %s", out)
	}
}

func TestTranslateSystemOneResponseProbabilityShapes(t *testing.T) {
	cases := []struct {
		name     string
		upstream string
		idx      int
		want     string
	}{
		{
			"choice array of value", `{"answers":{"p":{"type":"noul","noul":0.1},"q1":{"type":"choice","choice":"a","confidence":0.4,"probabilities":[{"value":"a","probability":0.4},{"value":"true","probability":0.6}]},"s":{"type":"score","score":0,"confidence":1,"probabilities":{}}}}`, 1,
			`{"type":"choice","name":null,"choice":"a","confidence":0.4,"probabilities":[{"value":"a","probability":0.4},{"value":true,"probability":0.6},{"value":false,"probability":0}]}`,
		},
		{
			"choice array of name", `{"answers":{"p":{"type":"noul","noul":0.1},"q1":{"type":"choice","choice":"a","confidence":0.4,"probabilities":[{"name":"a","probability":0.4}]},"s":{"type":"score","score":0,"confidence":1,"probabilities":{}}}}`, 1,
			`{"type":"choice","name":null,"choice":"a","confidence":0.4,"probabilities":[{"value":"a","probability":0.4},{"value":true,"probability":0},{"value":false,"probability":0}]}`,
		},
		{
			"choice missing key", `{"answers":{"p":{"type":"noul","noul":0.1},"q1":{"type":"choice","choice":"false","confidence":0.4,"probabilities":{"false":1}},"s":{"type":"score","score":0,"confidence":1,"probabilities":{}}}}`, 1,
			`{"type":"choice","name":null,"choice":false,"confidence":0.4,"probabilities":[{"value":"a","probability":0},{"value":true,"probability":0},{"value":false,"probability":1}]}`,
		},
		{
			"score positional array", `{"answers":{"p":{"type":"noul","noul":0.1},"q1":{"type":"choice","choice":"a","confidence":1,"probabilities":{}},"s":{"type":"score","score":0.8,"confidence":0.8,"probabilities":[0.2,0.8]}}}`, 2,
			`{"type":"score","name":"s","score":0.8,"confidence":0.8,"probabilities":[{"value":0,"label":"low","probability":0.2},{"value":1,"label":"high","probability":0.8}]}`,
		},
		{
			"score array of value objects", `{"answers":{"p":{"type":"noul","noul":0.1},"q1":{"type":"choice","choice":"a","confidence":1,"probabilities":{}},"s":{"type":"score","score":0.8,"confidence":0.8,"probabilities":[{"value":1,"probability":0.8},{"value":0,"probability":0.2}]}}}`, 2,
			`{"type":"score","name":"s","score":0.8,"confidence":0.8,"probabilities":[{"value":0,"label":"low","probability":0.2},{"value":1,"label":"high","probability":0.8}]}`,
		},
		{
			"score map missing key", `{"answers":{"p":{"type":"noul","noul":0.1},"q1":{"type":"choice","choice":"a","confidence":1,"probabilities":{}},"s":{"type":"score","score":1,"probabilities":{"1":1}}}}`, 2,
			`{"type":"score","name":"s","score":1,"confidence":0,"probabilities":[{"value":0,"label":"low","probability":0},{"value":1,"label":"high","probability":1}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _, err := translateResp(t, translateFixture, tc.upstream)
			if err != nil {
				t.Fatal(err)
			}
			assertJSONEqual(t, m["answers"].([]any)[tc.idx], tc.want)
		})
	}
}

func TestTranslateSystemOneResponseConfidenceAndUsageDefaults(t *testing.T) {
	up := `{"answers":{"p":{"type":"noul","noul":0.5},"q1":{"type":"choice","choice":"a","probabilities":{"a":1}},"s":{"type":"score","score":0,"probabilities":{"0":1}}}}`
	m, usage, err := translateResp(t, translateFixture, up)
	if err != nil {
		t.Fatal(err)
	}
	answers := m["answers"].([]any)
	if answers[1].(map[string]any)["confidence"] != 0.0 || answers[2].(map[string]any)["confidence"] != 0.0 {
		t.Fatalf("confidence default: %v", answers)
	}
	assertJSONEqual(t, m["usage"], `{"input_tokens":0,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0}`)
	if usage != (DecisionsUsage{}) {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestTranslateSystemOneResponseErrors(t *testing.T) {
	cases := map[string]string{
		"missing answer":       `{"answers":{"p":{"type":"noul","noul":0.5}}}`,
		"type mismatch":        `{"answers":{"p":{"type":"choice","choice":"a"},"q1":{"type":"choice","choice":"a","probabilities":{}},"s":{"type":"score","score":0}}}`,
		"missing type":         `{"answers":{"p":{"noul":0.5},"q1":{"type":"choice","choice":"a"},"s":{"type":"score","score":0}}}`,
		"not json":             `nope`,
		"not object":           `[1]`,
		"no answers":           `{"usage":{}}`,
		"answers array":        `{"answers":[]}`,
		"noul missing":         `{"answers":{"p":{"type":"noul"},"q1":{"type":"choice","choice":"a"},"s":{"type":"score","score":0}}}`,
		"unknown choice":       `{"answers":{"p":{"type":"noul","noul":1},"q1":{"type":"choice","choice":"zzz"},"s":{"type":"score","score":0}}}`,
		"choice missing":       `{"answers":{"p":{"type":"noul","noul":1},"q1":{"type":"choice"},"s":{"type":"score","score":0}}}`,
		"score missing":        `{"answers":{"p":{"type":"noul","noul":1},"q1":{"type":"choice","choice":"a"},"s":{"type":"score"}}}`,
		"answer not object":    `{"answers":{"p":5,"q1":{"type":"choice","choice":"a"},"s":{"type":"score","score":0}}}`,
		"probabilities string": `{"answers":{"p":{"type":"noul","noul":1},"q1":{"type":"choice","choice":"a","probabilities":"x"},"s":{"type":"score","score":0}}}`,
		"prob element bad":     `{"answers":{"p":{"type":"noul","noul":1},"q1":{"type":"choice","choice":"a","probabilities":["x"]},"s":{"type":"score","score":0}}}`,
		"prob object no p":     `{"answers":{"p":{"type":"noul","noul":1},"q1":{"type":"choice","choice":"a","probabilities":[{"value":"a"}]},"s":{"type":"score","score":0}}}`,
		"usage bad":            `{"answers":{"p":{"type":"noul","noul":1},"q1":{"type":"choice","choice":"a"},"s":{"type":"score","score":0}},"usage":5}`,
	}
	for name, up := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, table := mustTranslate(t, translateFixture)
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic: %v", r)
				}
			}()
			if _, _, err := TranslateSystemOneResponse([]byte(up), table, "c"); err == nil {
				t.Fatalf("expected error")
			}
		})
	}
}

func TestTranslateSystemOneResponseZeroTable(t *testing.T) {
	if _, _, err := TranslateSystemOneResponse([]byte(`{"answers":{}}`), DecisionsAnswerTable{}, "c"); err != nil {
		t.Fatalf("empty table: %v", err)
	}
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestNormalizeDecisionsConfidence(t *testing.T) {
	t.Run("openai choice", func(t *testing.T) {
		in := `{"model":"m","extra":{"x":[1,2]},"answers":[{"type":"choice","name":"n","choice":"b","confidence":0.1,"probabilities":[{"value":"a","probability":0.2},{"value":"b","probability":0.6}]}],"usage":{"input_tokens":1}}`
		out, ok := NormalizeDecisionsConfidence([]byte(in), DialectOpenAI)
		if !ok {
			t.Fatal("not ok")
		}
		m := decodeAny(t, out).(map[string]any)
		a := m["answers"].([]any)[0].(map[string]any)
		if !approx(a["confidence"].(float64), 0.75) {
			t.Fatalf("confidence = %v", a["confidence"])
		}
		want := decodeAny(t, []byte(in)).(map[string]any)
		want["answers"].([]any)[0].(map[string]any)["confidence"] = a["confidence"]
		if !reflect.DeepEqual(m, want) {
			t.Fatalf("other fields changed: %s", out)
		}
	})
	t.Run("system one score map", func(t *testing.T) {
		in := `{"answers":{"s":{"type":"score","score":0.8,"legend":{"0":"a","1":"b"},"probabilities":{"0":0.1,"1":0.3},"confidence":0.3},"x":{"type":"choice","choice":"a","probabilities":{"a":1,"b":1}}}}`
		out, ok := NormalizeDecisionsConfidence([]byte(in), DialectSystemOne)
		if !ok {
			t.Fatal("not ok")
		}
		ans := decodeAny(t, out).(map[string]any)["answers"].(map[string]any)
		s := ans["s"].(map[string]any)
		if !approx(s["confidence"].(float64), 0.75) || s["score"] != 0.8 {
			t.Fatalf("s = %v", s)
		}
		if !approx(ans["x"].(map[string]any)["confidence"].(float64), 0.5) {
			t.Fatalf("x = %v", ans["x"])
		}
	})
	t.Run("system one choice without confidence gains one", func(t *testing.T) {
		out, _ := NormalizeDecisionsConfidence([]byte(`{"answers":{"x":{"type":"choice","choice":"a","probabilities":[0.25,0.75]}}}`), DialectSystemOne)
		x := decodeAny(t, out).(map[string]any)["answers"].(map[string]any)["x"].(map[string]any)
		if !approx(x["confidence"].(float64), 0.75) {
			t.Fatalf("x = %v", x)
		}
	})
	untouched := map[string]struct {
		in string
		d  DecisionsDialect
	}{
		"predicate openai":     {`{"answers":[{"type":"predicate","name":null,"probability":0.4}]}`, DialectOpenAI},
		"noul system one":      {`{"answers":{"p":{"type":"noul","noul":0.4,"confidence":0.4}}}`, DialectSystemOne},
		"zero sum":             {`{"answers":[{"type":"choice","choice":"a","confidence":0.3,"probabilities":[{"value":"a","probability":0}]}]}`, DialectOpenAI},
		"no probabilities":     {`{"answers":{"x":{"type":"choice","choice":"a","confidence":0.3}}}`, DialectSystemOne},
		"unusable probs":       {`{"answers":{"x":{"type":"choice","choice":"a","confidence":0.3,"probabilities":"zz"}}}`, DialectSystemOne},
		"no answers":           {`{"model":"m"}`, DialectOpenAI},
		"answers wrong shape":  {`{"answers":{"a":1}}`, DialectOpenAI},
		"non-object answer":    {`{"answers":[5,null]}`, DialectOpenAI},
		"unknown answer types": {`{"answers":[{"type":"refusal","name":"n"}]}`, DialectOpenAI},
	}
	for name, tc := range untouched {
		t.Run("untouched "+name, func(t *testing.T) {
			out, _ := NormalizeDecisionsConfidence([]byte(tc.in), tc.d)
			if !reflect.DeepEqual(decodeAny(t, out), decodeAny(t, []byte(tc.in))) {
				t.Fatalf("changed: %s", out)
			}
		})
	}
	t.Run("non-json", func(t *testing.T) {
		in := []byte("not json at all")
		out, ok := NormalizeDecisionsConfidence(in, DialectOpenAI)
		if ok || !bytes.Equal(out, in) {
			t.Fatalf("out=%q ok=%v", out, ok)
		}
	})
	t.Run("json non-object", func(t *testing.T) {
		in := []byte(`[1,2]`)
		out, ok := NormalizeDecisionsConfidence(in, DialectSystemOne)
		if ok || !bytes.Equal(out, in) {
			t.Fatalf("out=%q ok=%v", out, ok)
		}
	})
}

func TestParseDecisionsUsage(t *testing.T) {
	oa := `{"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":4,"cache_write_tokens":2},"output_tokens":3,"output_tokens_details":{"reasoning_tokens":1},"total_tokens":13}}`
	if u := ParseDecisionsUsage([]byte(oa), DialectOpenAI); u != (DecisionsUsage{10, 3, 4, 2}) {
		t.Fatalf("openai usage = %+v", u)
	}
	s1 := `{"usage":{"input_tokens":7,"output_tokens":2}}`
	if u := ParseDecisionsUsage([]byte(s1), DialectSystemOne); u != (DecisionsUsage{InputTokens: 7, OutputTokens: 2}) {
		t.Fatalf("system one usage = %+v", u)
	}
	for _, bad := range []string{``, `nope`, `{"usage":5}`, `{}`, `[]`} {
		if u := ParseDecisionsUsage([]byte(bad), DialectOpenAI); u != (DecisionsUsage{}) {
			t.Fatalf("bad %q usage = %+v", bad, u)
		}
	}
}
