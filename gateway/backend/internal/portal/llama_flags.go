// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

// llamaServerValueFlags maps every llama-server option that takes a value to
// the number of argv elements its value occupies, at the pinned llama.cpp
// build (2bc46baa9207d098d03741ac394e7faaf1a955b6, common/arg.cpp: the
// options of the server example and the common ones, with their value
// hints). The cluster head scan uses it to step over option values, so an
// argument such as "--alias -dev" is read as one option with its value, not
// as a device flag. An option missing here is read as taking no value, which
// at worst reads its value as another option -- the scan's behaviour before
// the table existed. Regenerate it from common/arg.cpp when the pinned build
// moves.
var llamaServerValueFlags = map[string]int{
	"--adaptive-decay": 1, "--adaptive-target": 1, "--alias": 1, "--api-key": 1, "--api-key-file": 1,
	"--api-prefix": 1, "--batch-size": 1, "--cache-ram": 1, "--cache-reuse": 1, "--cache-type-k": 1,
	"--cache-type-k-draft": 1, "--cache-type-v": 1, "--cache-type-v-draft": 1, "--chat-template": 1,
	"--chat-template-file": 1, "--chat-template-kwargs": 1, "--checkpoint-min-step": 1,
	"--connect-code": 1, "--control-vector": 1, "--control-vector-layer-range": 2,
	"--control-vector-scaled": 1, "--cors-headers": 1, "--cors-methods": 1, "--cors-origins": 1,
	"--cpu-mask": 1, "--cpu-mask-batch": 1, "--cpu-mask-batch-draft": 1, "--cpu-mask-draft": 1,
	"--cpu-range": 1, "--cpu-range-batch": 1, "--cpu-range-draft": 1, "--cpu-strict": 1,
	"--cpu-strict-batch": 1, "--cpu-strict-batch-draft": 1, "--cpu-strict-draft": 1,
	"--ctx-checkpoints": 1, "--ctx-size": 1, "--defrag-thold": 1, "--device": 1, "--device-draft": 1,
	"--docker-repo": 1, "--draft": 1, "--draft-max": 1, "--draft-min": 1, "--draft-n": 1,
	"--draft-n-min": 1, "--draft-p-min": 1, "--draft-p-split": 1, "--dry-allowed-length": 1,
	"--dry-base": 1, "--dry-multiplier": 1, "--dry-penalty-last-n": 1, "--dry-sequence-breaker": 1,
	"--dynatemp-exp": 1, "--dynatemp-range": 1, "--embd-normalize": 1, "--fit": 1, "--fit-ctx": 1,
	"--fit-target": 1, "--flash-attn": 1, "--frequency-penalty": 1, "--gpu-layers": 1,
	"--gpu-layers-draft": 1, "--grammar": 1, "--grammar-file": 1, "--hf-file": 1, "--hf-repo": 1,
	"--hf-repo-draft": 1, "--hf-token": 1, "--host": 1, "--image-max-tokens": 1,
	"--image-min-tokens": 1, "--json-schema": 1, "--json-schema-file": 1, "--keep": 1,
	"--kv-unified-per-slot": 1, "--lazy-mode": 1, "--load-mode": 1, "--log-colors": 1,
	"--log-file": 1, "--log-prompts-dir": 1, "--log-verbosity": 1, "--logit-bias": 1,
	"--lookup-cache-dynamic": 1, "--lookup-cache-static": 1, "--lora": 1, "--lora-scaled": 1,
	"--main-gpu": 1, "--mcp-servers-config": 1, "--mcp-servers-json": 1, "--media-path": 1,
	"--min-p": 1, "--mirostat": 1, "--mirostat-ent": 1, "--mirostat-lr": 1, "--mmproj": 1,
	"--mmproj-device": 1, "--mmproj-url": 1, "--model": 1, "--model-draft": 1, "--model-url": 1,
	"--models-dir": 1, "--models-max": 1, "--models-memory-margin": 1, "--models-preset": 1,
	"--moe-cache-mib": 1, "--mtmd-batch-max-tokens": 1, "--n-cpu-ffn": 1, "--n-cpu-moe": 1,
	"--n-cpu-moe-draft": 1, "--n-gpu-layers": 1, "--n-gpu-layers-draft": 1, "--n-predict": 1,
	"--numa": 1, "--override-kv": 1, "--override-tensor": 1, "--override-tensor-draft": 1,
	"--parallel": 1, "--path": 1, "--poll": 1, "--poll-batch": 1, "--poll-batch-draft": 1,
	"--poll-draft": 1, "--pooling": 1, "--port": 1, "--predict": 1, "--presence-penalty": 1,
	"--prio": 1, "--prio-batch": 1, "--prio-batch-draft": 1, "--prio-draft": 1, "--reasoning": 1,
	"--reasoning-budget": 1, "--reasoning-budget-message": 1, "--reasoning-effort": 1,
	"--reasoning-format": 1, "--repeat-last-n": 1, "--repeat-penalty": 1, "--reverse-prompt": 1,
	"--rope-freq-base": 1, "--rope-freq-scale": 1, "--rope-scale": 1, "--rope-scaling": 1, "--rpc": 1,
	"--sampler-seq": 1, "--samplers": 1, "--sampling-seq": 1, "--seed": 1, "--sleep-idle-seconds": 1,
	"--slot-prompt-similarity": 1, "--slot-save-path": 1, "--spec-draft-cpu-mask": 1,
	"--spec-draft-cpu-mask-batch": 1, "--spec-draft-cpu-range": 1, "--spec-draft-cpu-strict": 1,
	"--spec-draft-cpu-strict-batch": 1, "--spec-draft-device": 1, "--spec-draft-hf": 1,
	"--spec-draft-model": 1, "--spec-draft-n-cpu-moe": 1, "--spec-draft-n-max": 1,
	"--spec-draft-n-min": 1, "--spec-draft-n-ubatch": 1, "--spec-draft-ncmoe": 1,
	"--spec-draft-ngl": 1, "--spec-draft-override-tensor": 1, "--spec-draft-p-min": 1,
	"--spec-draft-p-split": 1, "--spec-draft-poll": 1, "--spec-draft-poll-batch": 1,
	"--spec-draft-prio": 1, "--spec-draft-prio-batch": 1, "--spec-draft-sampling": 1,
	"--spec-draft-threads": 1, "--spec-draft-threads-batch": 1, "--spec-draft-type-k": 1,
	"--spec-draft-type-v": 1, "--spec-ngram-map-k-min-hits": 1, "--spec-ngram-map-k-size-m": 1,
	"--spec-ngram-map-k-size-n": 1, "--spec-ngram-map-k4v-min-hits": 1,
	"--spec-ngram-map-k4v-size-m": 1, "--spec-ngram-map-k4v-size-n": 1, "--spec-ngram-min-hits": 1,
	"--spec-ngram-mod-n-match": 1, "--spec-ngram-mod-n-max": 1, "--spec-ngram-mod-n-min": 1,
	"--spec-ngram-simple-min-hits": 1, "--spec-ngram-simple-size-m": 1,
	"--spec-ngram-simple-size-n": 1, "--spec-ngram-size-m": 1, "--spec-ngram-size-n": 1,
	"--spec-synth-len": 1, "--spec-synth-rates": 1, "--spec-type": 1, "--split-mode": 1,
	"--sse-ping-interval": 1, "--ssl-cert-file": 1, "--ssl-key-file": 1, "--swa-checkpoints": 1,
	"--tags": 1, "--temp": 1, "--temperature": 1, "--tensor-split": 1, "--threads": 1,
	"--threads-batch": 1, "--threads-batch-draft": 1, "--threads-draft": 1, "--threads-http": 1,
	"--timeout": 1, "--tools": 1, "--tools-runtime": 1, "--top-k": 1, "--top-n-sigma": 1,
	"--top-nsigma": 1, "--top-p": 1, "--typical": 1, "--typical-p": 1, "--ubatch-size": 1,
	"--ui-config": 1, "--ui-config-file": 1, "--verbosity": 1, "--video-ffmpeg-dir": 1,
	"--video-fps": 1, "--video-timestamp-interval": 1, "--webui-config": 1, "--webui-config-file": 1,
	"--xtc-probability": 1, "--xtc-threshold": 1, "--yarn-attn-factor": 1, "--yarn-beta-fast": 1,
	"--yarn-beta-slow": 1, "--yarn-ext-factor": 1, "--yarn-orig-ctx": 1, "-C": 1, "-Cb": 1, "-Cbd": 1,
	"-Cd": 1, "-Cr": 1, "-Crb": 1, "-Crd": 1, "-a": 1, "-b": 1, "-c": 1, "-cms": 1, "-cram": 1,
	"-ctk": 1, "-ctkd": 1, "-ctv": 1, "-ctvd": 1, "-ctxcp": 1, "-dev": 1, "-devd": 1, "-dr": 1,
	"-dt": 1, "-fa": 1, "-fit": 1, "-fitc": 1, "-fitt": 1, "-hf": 1, "-hfd": 1, "-hff": 1, "-hfr": 1,
	"-hfrd": 1, "-hft": 1, "-j": 1, "-jf": 1, "-l": 1, "-lcd": 1, "-lcs": 1, "-lm": 1, "-lv": 1,
	"-lzm": 1, "-m": 1, "-md": 1, "-mg": 1, "-mm": 1, "-mmdev": 1, "-mmu": 1, "-mu": 1, "-n": 1,
	"-ncffn": 1, "-ncmoe": 1, "-ncmoed": 1, "-ngl": 1, "-ngld": 1, "-np": 1, "-ot": 1, "-otd": 1,
	"-r": 1, "-rea": 1, "-s": 1, "-sm": 1, "-sps": 1, "-t": 1, "-tb": 1, "-tbd": 1, "-td": 1,
	"-to": 1, "-ts": 1, "-ub": 1, "dedup-cache-models": 1, "load-on-startup": 1, "stop-timeout": 1,
}
