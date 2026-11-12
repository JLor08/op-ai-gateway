# Decision protocol wire schemas (primary sources)

Raw source copies: scratchpad/raw/ (oa_*.py = openai-python v3.49.0, vllm_*.py = vLLM v0.33.0).
Refs: openai-python tag v3.49.0 (commit 3d573c04ef9ceafcba682ae5a98b3971a062a777, latest release at 2026-11-12), vLLM tag v0.33.0 (tag object 3a4d82f569a9f57054ba81633e9b6c73b34b51fe).
Web: https://developers.openai.com/api/reference/resources/decisions ; https://developers.openai.com/api/docs/guides/decisions ; https://docs.typesafe.ai/api.md (the latter two were summarised by the fetch tool, not verbatim; the reference page lists fields/limits fully).

## 1. OpenAI Decisions API  POST /v1/decisions

### Request (openai-python src/openai/types/decision_create_params.py @v3.49.0; reference page)
Top level (all JSON, no `stream`, no `metadata`, no `instructions`, no `temperature`):
- `input` REQUIRED: `str` (0-10,485,760 chars) | array (0-131,072 items) of input items
- `model` REQUIRED: str (max 1,048,576). "Only `gpt-6-luna` is supported."
- `questions` REQUIRED: array, 1-400 items (guide: up from 200 in beta). Answers same order.
- `prompt_cache_key`: str|null (max 64)
- `prompt_cache_options`: object|null `{mode?: "implicit"|"explicit"|null (default implicit), ttl?: "30m"}`
- `safety_identifier`: str|null (max 128)

Input items (`type` optional on messages, default "message"):
- user message: `{role:"user", content: str | [input_text | input_image | input_json parts], type?:"message"}`
- developer / system message: `{role:"developer"|"system", content: str | [input_text parts]}`
- assistant message: `{role:"assistant", content: str | [{type:"output_text",text} | {type:"refusal",refusal}], phase?: "commentary"|"final_answer"|null}`
- `{type:"function_call", arguments:str, call_id:str, name:str, id?, namespace?, status?: "in_progress"|"completed"|"incomplete"}`
- `{type:"function_call_output", call_id:str, output: str | [input_text|input_image parts], id?, status?}`
- `{type:"custom_tool_call", call_id, input:str, name, id?, namespace?, status?}`
- `{type:"custom_tool_call_output", call_id, output: str | [input_text|input_image parts], id?, status?}`
- input_json only allowed in user-role content (guide).

Parts:
- `input_text`: `{type:"input_text", text:str (max 10,485,760), prompt_cache_breakpoint?: {mode:"explicit"}|null}`
- `input_image`: `{type:"input_image", image_url:str (required; data: URL or public http(s) URL, NO file_id), detail?: "low"|"high"|"auto"|"original"|null (default auto), prompt_cache_breakpoint?}`
- `input_json`: `{type:"input_json", data: object | array<object>}` (values any JSON; limits 32 container levels, 10,000 values, 256 KiB compact UTF-8)

Questions (name optional on all: `name?: str`):
- predicate: `{type:"predicate", instructions, name?}`
- choice: `{type:"choice", instructions, choices:[{value: str|bool (required), description?: str}], name?}`; 2-255 choices, unique; values typed (string "true" != boolean true)
- score: `{type:"score", instructions, levels:[{label:str (required), description?:str}], name?}`; 2-10 levels, ordered
- `instructions`: `str` (max 1,048,576) | array 1-16,384 of DecisionQuestionInputPart = `{type:"input_text",text}` | `{type:"input_image",image_url,detail?}` | `{type:"input_json",data}`  => YES, instructions can be an array of parts (shared_params/decision_question_input_part.py; guide: "Array parts go directly in instructions", question images count toward the image limits).

### Response (openai-python src/openai/types/decision.py @v3.49.0; reference page)
`Decision` = `{answers: List[Answer], model: str, usage: Usage}`. NO `id`, NO `object`, NO `created_at` in the SDK model or reference page (confirmed absent on both).
Answer (discriminator `type`), in question order:
- predicate: `{type:"predicate", name: str|null, probability: float}`  (SDK: `name: Optional[str] = None`, `probability: float`)
- choice: `{type:"choice", name: str|null, choice: str|bool, confidence: float, probabilities: [{value: str|bool, probability: float}]}` -- probabilities is an ARRAY
- score: `{type:"score", name: str|null, score: float, confidence: float, probabilities: [{value: int, label: str, probability: float}]}` -- ARRAY; `value` = zero-based level index; score = probability-weighted average of level indices (guide: "Level indices start at 0", e.g. 0.1/0.7/0.2 -> 1.1)
- refusal: `{type:"refusal", name: str|null}` -- per-question; other questions still answered. No reason field.
Usage: `{input_tokens:int, input_tokens_details:{cache_write_tokens:int, cached_tokens:int}, output_tokens:int, output_tokens_details:{reasoning_tokens:int}, total_tokens:int}` (all required in SDK)
Error shape: not documented on the reference page (SDK uses the standard OpenAI `{error:{message,type,param,code}}` handling; not verified for this endpoint).
SDK call: `client.decisions.create(...)` -> POST "/decisions" (src/openai/resources/decisions.py), cast_to=Decision, no streaming variant. vLLM docs say SDK >= 3.26.0.

## 2. vLLM v0.33.0

Files: vllm/entrypoints/generate/structured_decisions/{protocol,serving,api_router}.py (System One), vllm/entrypoints/openai/decisions/{protocol,adapters,question_types,serving,state,strategies,api_router}.py, docs/models/structured_decisions_models.md.

### POST /v1/systemone  (generate/structured_decisions/protocol.py)
Request `StructuredDecisionRequest` (extra="forbid" -> unknown top-level field = 400):
- `model: str|None=None`, `state: Any` REQUIRED (text or JSON), `questions: dict[str, QuestionSpec]` REQUIRED (id -> spec, ordered), `instructions: str|None`, `images: list[str]|None`, `audio: list[str]|None`, `videos: list[str]|None` (URLs or data URLs), `chat_template_kwargs: dict|None`, `joint: bool=False` (DiffusionGemma only else 400), `cache_salt: str|None`, `priority: int=0`, `request_id: str` (default uuid).
- QuestionSpec: `{type: str, instructions: Any = "", criteria: Any = None}`; other question fields -> 400.
- Types/criteria (openai/decisions/question_types.py):
  - `choice`: criteria = object option name -> description|null (non-empty). Max 26 options (A-Z) on label-reading models; D3: up to len(codes)=255.
  - `noul`: criteria = null or object with only keys `true`/`false` (strings|null). Yes/no.
  - `score`: criteria = ordered list of level names, >=2 (docs: max 10 levels). Items str()-ed.
  - unknown type -> 400 "unknown question type ... supported: ['choice','noul','score']"; no `box`.
  - Questions per request: 1..64. Duplicate option names -> 400. Empty id -> 400.
Response `StructuredDecisionResponse`: `{id: str ("decision-<reqid>"), object:"structured_decision", created:int, model:str, answers: dict[qid, dict], usage:{input_tokens:int, output_tokens:int}, diagnostics: dict[qid,{label_mass:float, argmax_is_label:bool}], joint: {canvases:[{questions,width,slots}]}|null}`
Answers (question_types.py):
- choice: `{"type":"choice","choice":<option name>,"probabilities":{name: p,...} (MAP keyed by option name),"confidence": p_top*label_mass}`
- noul: `{"type":"noul","noul": P(yes),"probabilities":{"yes":p,"no":p},"confidence": max(p)*label_mass}`
- score: `{"type":"score","score": sum(i*p_i) (EXPECTED ZERO-BASED INDEX, float, can be fractional),"legend":{"0":"<level0 name>","1":...} (map index-string -> level name from criteria),"probabilities":{"0":p,"1":p,...} (MAP keyed by index string),"confidence": p_argmax*label_mass}`
- `confidence` = chosen option prob over whole vocab (share among labels x label_mass). Not argmax of score.
Errors: StructuredDecisionError -> 400 via ErrorResponse `{error:{message:str,type:str,param:str|null,code:int}}` (serve/engine/protocol.py). No handler (unsupported model / no read strategy) -> `raise NotImplementedError("The model does not support the structured decision API")` -> 501 per docs ("Models without a supported read strategy return 501"; route declares 400/404/500/501 responses).

### POST /v1/decisions  (openai/decisions/protocol.py)
All models `extra="forbid"` -> any unlisted field (prompt_cache_key, prompt_cache_options, stream, metadata...) = 400.
Request `DecisionRequest`: `model: str`, `input: str (<=10,485,760) | list[DecisionInputMessage] (<=131,072)`, `questions: 1..200 list`, `safety_identifier: str|null (<=128)`, `joint: bool=False` (vLLM ext), `cache_salt: str|null` (vLLM ext, 1..1024).
- DecisionInputMessage: `{role:"user" ONLY, content: str | list[{type:"input_text",text}] (<=16384), type:"message"="message"}`. input_image -> 400 "Decisions supports text input only". developer/system/assistant roles, tool items, input_json, input_image, prompt_cache_* all rejected.
- Questions: predicate `{type,instructions:str(<=1,048,576),name?}`; choice adds `choices:[{value:str|bool, description:str=""}]` 2..255 in schema, distinct (type,value); score adds `levels:[{label,description=""}]` 2..10. `instructions` is a STRING ONLY (array of parts -> 400). `name` explicit null -> 400 ("name must be a string when provided").
- Runtime limits (adapters/strategies): max_questions 64, options <=26 (A-Z) on generate models -> 400 "This model supports at most N choices per question"; docs: "MVP supports 1-64 questions, 2-26 choices, 2-10 score levels".
Response `DecisionResponse` = `{model:str, answers:[...], usage:{input_tokens, input_tokens_details:{cached_tokens=0,cache_write_tokens=0}, output_tokens, output_tokens_details:{reasoning_tokens=0}, total_tokens}}`. No id/object.
- predicate `{type,name:str|null,probability: probs[1]}` (probs over [false,true]); name always present (null if unnamed)
- choice `{type,name,choice:<value>,probabilities:[{value,probability}] ARRAY in request order,confidence}`
- score `{type,name,score: sum(i*p) zero-based expected index,probabilities:[{value:int idx,label,probability}] ARRAY,confidence}`
- No refusal answers ("per-question refusal scoring outside this MVP").
Errors/501: same ErrorResponse; if `app.state.openai_serving_decisions` is None (model w/o read strategy, OR strategy.task != "generate", e.g. D3 classify) -> JSONResponse 501 `{error:{message:"The model does not support the Decisions API", type:"NotImplementedError"?, code:501}}` (built with create_error_response(NotImplementedError(...)); type string not verified by running). Validation errors (pydantic) -> 400/422 via vLLM's standard handler (not traced).

### D3 (D3ForSequenceClassification, D3Strategy, task="classify")
- Served ONLY via /v1/systemone (init_decisions_state sets openai_serving_decisions only if strategy.task=="generate"), so /v1/decisions on a d3 model -> 501.
- One classify read per question; codes `decision_config["codes"]` (255) not A-Z; options up to 255 for choice, score max 10 levels; noul supported (options sent as "No / false"/"Yes / true" unless descriptions). Probabilities = softmax of the question's option-code logits at checkpoint temperature; answer shape identical to above (same QuestionType.answer). `confidence` = p_top*label_mass; `label_mass` handling per strategy (not 1 forced for D3 verified: docs say Clef models use 1; D3 read path not re-derived). `instructions` -> 400 (fixed prompt); chat_template_kwargs ignored. Images/videos accepted; audio rejected (modalities {image,video}).

## 3. TypeSafe System One (https://docs.typesafe.ai/api.md; POST https://api.typesafe.ai/v1/systemone; full spec https://api.typesafe.ai/openapi.json)
Request: `model`, `state` (string|object|array), `questions` (object name -> question), optional `assets` (images, need TypeSafe-Danger-Zone-Images header). Types: `noul` (criteria optional {true,false} nullable), `choice` (criteria map name->desc|null, 1-255), `score` (criteria ordered array of 1-10 level descriptions), `box`.
Response: `{model, answers:{name: answer}, usage:{input_tokens, output_tokens}}`.
- noul: `{"type":"noul","noul":0.98}` (prob of yes; no probabilities/confidence in doc example)
- choice: `{"type":"choice","choice":"calm","confidence":0.7,"probabilities":{"angry":0.15,"calm":0.8,"excited":0.05}}` MAP
- score: `{"type":"score","score":1.7,"confidence":0.55,"legend":{"0":"Can wait","1":"...","2":"..."},"probabilities":{"0":0.1,"1":0.1,"2":0.8}}` -- score = probability-weighted average of zero-based indices (index-based, can be fractional); legend index-string -> criteria description; probabilities MAP keyed by index string.
Errors: 400 `{"detail": <str|object>}`; 422 `{"detail":[{loc,msg,type,input?,ctx?}]}`.
Note: TypeSafe has no id/object/created/diagnostics (vLLM adds those).

## Disagreements
1. probabilities: OpenAI = ARRAY of {value,probability} (choice) / {value,label,probability} (score); System One (TypeSafe + vLLM) = MAP (choice: option name; score: index string).
2. score: all three agree numerically = expected zero-based index (float). OpenAI score probabilities carry `value` (int) + `label`; System One score has `legend` {"i": name} instead and keys by str(index). Request side: OpenAI `levels:[{label,description}]` vs System One `criteria:[names]` (TypeSafe: "level descriptions").
3. Predicate vs noul: OpenAI `probability` = P(true) with `name`; System One `noul` = P(yes); vLLM noul additionally has probabilities{yes,no}+confidence; TypeSafe doc example shows only `noul`.
4. Answers: OpenAI = ordered ARRAY with optional `name` (null echoed); System One = MAP keyed by question id.
5. Choice values: OpenAI str|bool (typed); System One names are strings only.
6. Refusal: OpenAI has `refusal` answer type; System One / vLLM have none.
7. Envelope: OpenAI {answers,model,usage} (no id/object; usage has details sub-objects, total_tokens); vLLM systemone adds id/object/created/diagnostics/joint; usage {input_tokens,output_tokens} only.
8. Limits: OpenAI 400 questions/255 choices vs vLLM 64/26 (generate models); score levels 2-10 in OpenAI & vLLM (/decisions), TypeSafe 1-10; vLLM score min 2.
9. vLLM systemone docs say choice "at least one option"; OpenAI choices min 2.
10. Errors: OpenAI undocumented (likely {error:{...}}); vLLM {error:{message,type,param,code}}; TypeSafe {detail}.
11. Unsupported model: vLLM 501 (both endpoints); D3 supports /v1/systemone but not /v1/decisions.
