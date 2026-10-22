// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useCallback, useEffect, useRef, useState, type SubmitEvent } from 'react';
import {
  Box,
  Button,
  Checkbox,
  FormControlLabel,
  IconButton,
  Paper,
  Slider,
  Stack,
  TextField,
  Tooltip,
  Typography,
} from '@mui/material';
import CloseIcon from '@mui/icons-material/Close';
import AddPhotoAlternateIcon from '@mui/icons-material/AddPhotoAlternate';
import LayersIcon from '@mui/icons-material/Layers';
import type { ImageModelOptions } from '../api';
import type { Translation } from './shared/types';
import type { ChatContent } from './shared/chatContent';
import { MASK_THUMBNAIL_BACKGROUND } from './shared/maskThumbnail';
import { PageTitle } from './shared/PageTitle';
import { keyedUrls } from './shared/urlKeys';
import { SearchableSelect, type SearchableOption } from './shared/SearchableSelect';
import { AudioRetentionControl } from './AudioRetentionControl';
import { ChatMessage } from './ChatMessage';
import { SpeechVoiceField } from './SpeechVoiceField';
import { TranscriptionComposer } from './TranscriptionComposer';
import {
  IMAGE_EDIT_SOURCE_ACCEPT,
  IMAGE_LOSSY_FORMATS,
  IMAGE_N_MAX,
  IMAGE_OUTPUT_FORMATS,
  IMAGE_SAMPLERS,
  IMAGE_SCHEDULERS,
  IMAGE_SIZE_OPTIONS,
  audioPassthroughMessage,
  optionsOr,
  parseAudioPassthrough,
  parseTranscriptionPassthrough,
  transcriptionPassthroughMessage,
  useChatStore,
} from './chat/ChatStore';
import { ChatSidebar } from './chat/ChatSidebar';

const SIDEBAR_COLLAPSED_KEY = 'op.chat.sidebarCollapsed';

// The speaking-rate hint of the speech settings: the neutral rate every OpenAI-style
// speech endpoint documents. A placeholder only -- a blank speed is never written, so
// the model keeps its own default (and its own range, which the portal does not clamp).
const SPEECH_SPEED_PLACEHOLDER = '1.0';

// The small round close button on the corner of an attached image's (or mask's)
// thumbnail in the composer's attach strip.
const thumbnailRemoveSx = {
  position: 'absolute',
  top: -6,
  right: -6,
  width: 20,
  height: 20,
  bgcolor: 'var(--brand-accent)',
  color: 'var(--accent-text)',
} as const;

// A bound or default the model actually reported. The options endpoint omits
// (omitempty) whatever it does not know, so `undefined` is the ONLY "not
// reported" signal: 0 and -1 are real values (clip_skip and seed report -1, a
// width/height/steps default can be 0) and must never be read as "unset". It
// also keeps an absent bound out of Math.min/Math.max (NaN) and out of a <=/>=
// comparison (silently false).
function isReported(value: number | undefined): value is number {
  return typeof value === 'number' && Number.isFinite(value);
}

// The most images one request may ask for. IMAGE_N_MAX is the portal's HARD cap
// (not a model limit), so the picked model's reported max
// batch count may only NARROW it: a usable (>= 1) report is clamped to the cap
// (a model reporting 16 still gets 8), and an absent or unusable one leaves the
// cap itself.
function imageCountMax(options: ImageModelOptions | null): number {
  const reported = options?.max_batch_count;
  return isReported(reported) && reported >= 1
    ? Math.min(Math.floor(reported), IMAGE_N_MAX)
    : IMAGE_N_MAX;
}

// Clamp a typed image count to [1, max]; a non-number falls back to 1. The
// gateway passes n through and sd-server clamps too, but keeping the field
// honest avoids a request that silently generates a different count.
function clampImageN(raw: number, max: number): number {
  if (!Number.isFinite(raw)) return 1;
  return Math.min(max, Math.max(1, Math.round(raw)));
}

// Whether `value` lies inside [min, max], where either bound may be unstated
// (undefined = unbounded on that side).
function withinBounds(value: number, min: number | undefined, max: number | undefined): boolean {
  return (!isReported(min) || value >= min) && (!isReported(max) || value <= max);
}

const SIZE_PRESET = /^(\d+)x(\d+)$/;

// Whether a "WxH" size preset fits the model's size envelope. An absent envelope
// or an unstated bound constrains nothing; "" (the model default) and anything
// that is not a WxH string always stay.
function sizePresetFits(preset: string, limits: ImageModelOptions['size_limits']): boolean {
  const match = SIZE_PRESET.exec(preset);
  if (!limits || !match) return true;
  return (
    withinBounds(Number(match[1]), limits.min_width, limits.max_width) &&
    withinBounds(Number(match[2]), limits.min_height, limits.max_height)
  );
}

// The dropdown entries of one image knob: the given values with the "" default
// entry always first, and the thread's PERSISTED value appended as a trailing
// entry when the (narrowed) list omits it, so a saved pick the current model does
// not offer stays visible and selectable instead of rendering blank -- the same
// trick as the locked server-override option below.
function imageChoices(
  values: readonly string[],
  current: string,
  labelOf: (value: string) => string,
): SearchableOption[] {
  const entries = values.includes('') ? [...values] : ['', ...values];
  if (current !== '' && !entries.includes(current)) entries.push(current);
  return entries.map((value) => ({ value, label: labelOf(value) }));
}

// "<prefix>: <value>" for a default the model reported, else undefined. Reported
// means present: a number's 0 / -1 count, but an empty string says nothing.
function defaultHint(prefix: string, value: string | number | undefined): string | undefined {
  const known =
    typeof value === 'number' ? Number.isFinite(value) : value !== undefined && value !== '';
  return known ? `${prefix}: ${value}` : undefined;
}

function readSidebarCollapsed(): boolean {
  try {
    return window.localStorage?.getItem(SIDEBAR_COLLAPSED_KEY) === '1';
  } catch {
    return false;
  }
}

// A generated image's alt text is the prompt that produced it: the text of
// the user turn immediately preceding the assistant turn that rendered it.
function textOf(content: ChatContent): string | undefined {
  if (typeof content === 'string') return content;
  const textPart = content.find((part) => part.type === 'text');
  return textPart?.text;
}

// The chat transcript, settings, and stream driver live in ChatStoreProvider
// (mounted above the view switch in App) so a stream keeps running when the
// user navigates away and back. This view is a thin presentation layer over
// useChatStore(); the local "settings open" toggle and the sidebar-collapsed
// flag stay here.
export function Chat({ t }: Readonly<{ t: Translation }>) {
  const c = useChatStore();
  // The live speech bubble's Mute, for the active chat: a stable reference (the store's
  // muteAudio is) so it never defeats a row's memo.
  const { activeChatId, muteAudio } = c;
  const onMuteAudio = useCallback(
    (muted: boolean) => {
      if (activeChatId) muteAudio(activeChatId, muted);
    },
    [activeChatId, muteAudio],
  );
  const [showSettings, setShowSettings] = useState(false);
  // The advanced (sd-server) image knobs are collapsed by default: they are
  // power-user controls below the common size/count/format settings.
  const [showAdvanced, setShowAdvanced] = useState(false);
  // The speech thread's own collapsible "Advanced" (the JSON passthrough). Separate
  // from the image one: the two never show together, and a speech thread should not
  // open expanded because an image thread's section was left open.
  const [showSpeechAdvanced, setShowSpeechAdvanced] = useState(false);
  // And the transcription thread's: its own, for the same reason (a transcription thread should
  // not open expanded because a speech thread's section was left open).
  const [showTranscriptionAdvanced, setShowTranscriptionAdvanced] = useState(false);
  const [sidebarCollapsed, setSidebarCollapsed] = useState<boolean>(readSidebarCollapsed);

  // A refine (the per-image action on a generated result) seeds the composer with that
  // image as the source of the next edit; the user's next move is to say what to
  // change, and the composer may be far below the image they clicked, so the prompt
  // field takes the focus (which also scrolls it into view). The store only bumps a
  // counter; the view acts on a CHANGE of it, so a remount of this view (the store
  // lives above the view switch) does not replay an old refine.
  const promptRef = useRef<HTMLTextAreaElement | null>(null);
  const handledFocusRequest = useRef(c.composerFocusRequest);
  useEffect(() => {
    if (handledFocusRequest.current === c.composerFocusRequest) return;
    handledFocusRequest.current = c.composerFocusRequest;
    promptRef.current?.focus();
  }, [c.composerFocusRequest]);

  function toggleSidebar() {
    setSidebarCollapsed((prev) => {
      const next = !prev;
      try {
        window.localStorage?.setItem(SIDEBAR_COLLAPSED_KEY, next ? '1' : '0');
      } catch {
        /* best-effort */
      }
      return next;
    });
  }

  function submit(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    c.send();
  }

  // Once a server override is IN EFFECT (the chat's own picked server, or the
  // run-as token's server when it locks the chat -- see effectiveServerOverride),
  // narrow the model dropdown to what that server actually offers (api.serverModels,
  // fetched by the store); otherwise show the normal reachable-models list.
  // serverOverrideModels carries no loaded/vision metadata (it is a plain
  // per-server model listing), so those fields are simply omitted for the
  // filtered options.
  const modelPickerOptions =
    c.effectiveServerOverride !== ''
      ? c.serverOverrideModels.map((option) => ({ value: option.id, label: option.display_name }))
      : c.modelOptions.map((option) => ({
          value: option.id,
          label: option.display_name,
          loaded: option.loaded,
          loadedTitle:
            option.loaded_on && option.loaded_on.length > 0
              ? `${t.chatModelLoadedOn}: ${option.loaded_on.join(', ')}`
              : t.chatModelLoaded,
        }));

  // The THREAD's kind, not the picked model's capability: a text thread stays
  // a text thread even if an image model is selected afterwards (the backend
  // pins the kind at the first send and forces it from then on), and an image
  // thread keeps its composer even while its model is momentarily unreachable.
  const isImageThread = c.chatKind === 'image';
  // Likewise for a text-to-speech thread: the composer keeps its text input but the
  // settings become the speech knobs, and the reply is audio.
  const isSpeechThread = c.chatKind === 'speech';
  // And for a speech-to-text thread: the composer's text prompt gives way to an audio input (an
  // attached or recorded clip is the turn), the settings become the transcription knobs, and the
  // reply is text.
  const isTranscriptionThread = c.chatKind === 'transcription';
  const isTextThread = !isImageThread && !isSpeechThread && !isTranscriptionThread;

  // The advanced passthrough is the one speech or transcription setting the store REFUSES a send
  // (and a regenerate) for when it is not a JSON object or names a field the run owns
  // (chatAudioPassthroughInvalid, chatSpeechPassthroughReserved and their transcription twins --
  // the transcription one also refuses a field name a form cannot carry), and the
  // field is inside two things the user can close: the settings panel and the Advanced
  // section. A refusal that names a field the user cannot see is useless, so while the
  // text is invalid (the same parse the store refuses on) both stay open: the toast and
  // the field's own error then always point at something on screen. Typing alone cannot
  // hide it, but a remount can -- the store (and so the text) outlives this view, whose
  // own open/closed state starts collapsed again -- hence the derivation here rather
  // than only the toggles' guard below. Once the text is fixed or blank, neither is held open.
  const passthroughParse = parseAudioPassthrough(c.audioSettings.passthrough);
  const passthroughInvalid = isSpeechThread && passthroughParse.status === 'invalid';
  const transcriptionPassthroughParse = parseTranscriptionPassthrough(
    c.transcriptionSettings.passthrough,
  );
  const transcriptionPassthroughInvalid =
    isTranscriptionThread && transcriptionPassthroughParse.status === 'invalid';
  const settingsOpen = showSettings || passthroughInvalid || transcriptionPassthroughInvalid;
  const speechAdvancedOpen = showSpeechAdvanced || passthroughInvalid;
  const transcriptionAdvancedOpen = showTranscriptionAdvanced || transcriptionPassthroughInvalid;
  // The toggles' updater: a click while the text is invalid keeps the thing OPEN
  // (rather than flipping state the user cannot see), so fixing the text -- whose last
  // keystroke makes it valid -- does not collapse the field out from under the cursor.
  // The two kinds' flags can never both be set (each needs its own kind of thread), so one
  // updater serves the settings panel's toggle and both Advanced toggles.
  const toggleOpen = (open: boolean) =>
    passthroughInvalid || transcriptionPassthroughInvalid || !open;

  // The attach control follows ONE predicate, the store's attachMode (the
  // thread's kind and the picked model -- ADR-043 R1): 'edit' offers the image as
  // the source of an edit, 'vision' as an input the model reads, 'none' refuses.
  //
  // Which capability a REFUSED attach button is about: the two are different -- an
  // image GENERATOR (that cannot be edited) emits images and accepts none, while a
  // non-vision model simply cannot read one. Saying "does not support images" on
  // a generator told the user the opposite of the truth.
  let attachTooltip: string = t.chatAttachImage;
  if (c.attachMode === 'edit') attachTooltip = t.chatAttachImageEdit;
  else if (c.attachMode === 'none') {
    // A speech thread takes no image whatever the picked model can read (the thread's
    // kind decides, as for an image thread), so a vision-capable model must not leave
    // the disabled button reading as a plain "attach an image".
    if (isSpeechThread) attachTooltip = t.chatSpeechNoImageInput;
    else if (c.modelImageCapable) attachTooltip = t.chatImageGeneratorNoInput;
    else if (!c.modelVisionCapable) attachTooltip = t.chatImageModelUnsupported;
  }

  // The prompt field's helper text, on an image thread only. An edit-capable
  // model's thread also edits: the hint says what an attachment does there (there
  // is no mode to switch, the image is the operand). A generator that cannot be
  // edited keeps the old wording.
  let promptHelperText: string | undefined;
  let promptLabel = t.messageLabel;
  if (isImageThread) {
    promptHelperText = c.attachMode === 'edit' ? t.chatImageEditHint : t.chatImageOnlyHint;
    promptLabel = t.chatImagePromptLabel;
  } else if (isSpeechThread) {
    promptHelperText = t.chatSpeechOnlyHint;
    promptLabel = t.chatSpeechPromptLabel;
  }

  let modelHelperText: string | undefined;
  if (c.overrideModel !== '') {
    modelHelperText = `${t.chatModelFromToken}: ${c.overrideModel}`;
  } else if (c.effectiveServerOverride !== '') {
    modelHelperText = t.serverOverrideFilteredHint;
  }

  // The advanced image controls follow the PICKED model: each dropdown offers
  // what that model reports (store: modelImageOptions, null until fetched or when
  // the model reports nothing) and falls back to the static sd-cpp superset
  // otherwise. The model's reported defaults are shown as hints only, never
  // written into the knobs: a blank knob must stay blank so the run omits it and
  // sd-server keeps its own default.
  const imageOptions = c.modelImageOptions;
  const imageDefaults = imageOptions?.defaults;
  const imageCountCeiling = imageCountMax(imageOptions);
  const defaultLabel = t.chatImageAdvancedDefault;
  const hintOr = (value: number | undefined) => defaultHint(defaultLabel, value) ?? defaultLabel;
  const sizeDefaultHint =
    isReported(imageDefaults?.width) && isReported(imageDefaults?.height)
      ? defaultHint(defaultLabel, `${imageDefaults.width}x${imageDefaults.height}`)
      : undefined;

  return (
    <>
      <PageTitle title={t.chat} subtitle={t.chatIntro} titleId="chat-heading" />
      <Box sx={{ display: 'flex', gap: 2, alignItems: 'stretch', minHeight: 0 }}>
        <ChatSidebar t={t} collapsed={sidebarCollapsed} onToggleCollapse={toggleSidebar} />
        <Paper
          component="section"
          variant="outlined"
          sx={{ p: 3, flex: 1, minWidth: 0 }}
          aria-labelledby="chat-heading"
        >
          {/* Run-as token + model pickers are ALWAYS stacked (single column) so the
            layout does not reflow when the sidebar panel is opened/closed. */}
          <Box sx={{ display: 'grid', gridTemplateColumns: 'minmax(0, 460px)', gap: 2.25 }}>
            <SearchableSelect
              id="chat-run-as-token"
              label={t.chatRunAsTokenLabel}
              value={c.selectedTokenId}
              onChange={(value) => c.setSelectedTokenId(value)}
              disabled={c.streaming}
              options={[
                { value: '', label: t.chatRunAsNone },
                ...c.usableTokens.map((tk) => ({ value: tk.id, label: tk.name })),
              ]}
            />
            <SearchableSelect
              id="chat-model"
              label={t.chatModel}
              value={c.model}
              onChange={(value) => c.setModel(value)}
              disabled={c.streaming || c.overrideLocksModel}
              helperText={modelHelperText}
              // A loaded option renders a green dot BOTH in the dropdown and (once
              // selected) before the name in the field. A picked-but-unavailable model
              // shows a red "!" in the same leading spot instead (not for an empty,
              // deliberately cleared selection) — see SearchableSelect.
              unavailable={(c.model !== '' || c.overrideModel !== '') && !c.modelAvailable}
              unavailableTitle={t.chatModelUnavailable}
              options={modelPickerOptions}
            />
          </Box>

          <Stack direction="row" sx={{ flexWrap: 'wrap', alignItems: 'center', gap: 1, mb: 1.5 }}>
            <Button variant="outlined" size="small" onClick={() => setShowSettings(toggleOpen)}>
              {t.chatSettings}
            </Button>
            {c.streaming && (
              <Button variant="outlined" size="small" onClick={c.stop}>
                {t.chatStop}
              </Button>
            )}
          </Stack>

          {settingsOpen && (
            <Box
              sx={{
                display: 'grid',
                gap: 1.75,
                mb: 2,
                p: 2,
                border: '1px solid var(--line)',
                bgcolor: 'var(--page)',
              }}
            >
              {/* Text-chat controls: a system prompt, sampling temperature and a
                token cap are all meaningless to the images, speech and transcription
                endpoints, so an image, speech or transcription thread hides them and
                shows its own knobs below instead. */}
              {isTextThread && (
                <>
                  <TextField
                    id="chat-system-prompt"
                    label={t.chatSystemPrompt}
                    multiline
                    minRows={3}
                    fullWidth
                    value={c.systemPrompt}
                    onChange={(event) => c.setSystemPrompt(event.target.value)}
                    disabled={c.streaming}
                  />
                  <Box
                    sx={{
                      display: 'grid',
                      gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))',
                      gap: 2,
                    }}
                  >
                    <Box>
                      <Typography component="label" id="chat-temperature-label">
                        {t.chatTemperature}: {c.temperature.toFixed(1)}
                      </Typography>
                      <Slider
                        aria-labelledby="chat-temperature-label"
                        min={0}
                        max={2}
                        step={0.1}
                        value={c.temperature}
                        onChange={(_event, value) => c.setTemperature(value as number)}
                        disabled={c.streaming}
                      />
                    </Box>
                    <TextField
                      id="chat-max-tokens"
                      label={t.chatMaxTokens}
                      type="number"
                      value={String(c.maxTokens)}
                      onChange={(event) => c.setMaxTokens(Number(event.target.value))}
                      disabled={c.streaming}
                    />
                  </Box>
                </>
              )}
              {/* Image-generation knobs. The gateway passes these through to
                sd-server, which honours size / n / output_format and (for a
                lossy format) output_compression; an untouched control sends
                nothing, leaving the upstream default. */}
              {isImageThread && (
                <Box
                  sx={{
                    display: 'grid',
                    gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))',
                    gap: 2,
                  }}
                >
                  <SearchableSelect
                    id="chat-image-size"
                    label={t.chatImageSize}
                    value={c.imageSettings.size}
                    onChange={(value) => c.setImageSettings((s) => ({ ...s, size: value }))}
                    disabled={c.streaming}
                    helperText={sizeDefaultHint}
                    options={imageChoices(
                      IMAGE_SIZE_OPTIONS.filter((size) =>
                        sizePresetFits(size, imageOptions?.size_limits),
                      ),
                      c.imageSettings.size,
                      (size) => (size === '' ? t.chatImageSizeDefault : size),
                    )}
                  />
                  <TextField
                    id="chat-image-count"
                    label={t.chatImageCount}
                    type="number"
                    value={String(c.imageSettings.n)}
                    onChange={(event) =>
                      c.setImageSettings((s) => ({
                        ...s,
                        n: clampImageN(Number(event.target.value), imageCountCeiling),
                      }))
                    }
                    disabled={c.streaming}
                    // A saved count above THIS model's ceiling is kept as typed but
                    // flagged, since the run would otherwise ask for more than the
                    // model offers.
                    error={c.imageSettings.n > imageCountCeiling}
                    helperText={`1–${imageCountCeiling}`}
                  />
                  <SearchableSelect
                    id="chat-image-output-format"
                    label={t.chatImageOutputFormat}
                    value={c.imageSettings.output_format}
                    onChange={(value) =>
                      c.setImageSettings((s) => ({ ...s, output_format: value }))
                    }
                    disabled={c.streaming}
                    helperText={defaultHint(
                      defaultLabel,
                      imageDefaults?.output_format?.toUpperCase(),
                    )}
                    options={imageChoices(
                      optionsOr(imageOptions?.output_formats, IMAGE_OUTPUT_FORMATS),
                      c.imageSettings.output_format,
                      (format) => (format === '' ? t.chatImageFormatDefault : format.toUpperCase()),
                    )}
                  />
                  {IMAGE_LOSSY_FORMATS.includes(c.imageSettings.output_format) && (
                    <Box>
                      <Typography component="label" id="chat-image-compression-label">
                        {t.chatImageCompression}: {c.imageSettings.output_compression}
                      </Typography>
                      <Slider
                        aria-labelledby="chat-image-compression-label"
                        min={0}
                        max={100}
                        step={1}
                        value={c.imageSettings.output_compression}
                        onChange={(_event, value) =>
                          c.setImageSettings((s) => ({
                            ...s,
                            output_compression: value as number,
                          }))
                        }
                        disabled={c.streaming}
                      />
                    </Box>
                  )}
                </Box>
              )}
              {/* Advanced (sd-server) knobs: the native sampler/steps/cfg/etc.,
                collapsed by default. The gateway folds the SET ones into the
                prompt's <sd_cpp_extra_args> block; a blank numeric field leaves
                sd-server's own per-model default. Sampler/scheduler (and the size,
                format and count above) narrow to the picked model's reported
                options, else the static sd-cpp enum superset (a build that lacks
                one ignores it); the model's reported defaults show as hints. */}
              {isImageThread && (
                <>
                  <Button
                    variant="text"
                    size="small"
                    onClick={() => setShowAdvanced((open) => !open)}
                    sx={{ justifySelf: 'start' }}
                  >
                    {t.chatImageAdvanced}
                  </Button>
                  {showAdvanced && (
                    <Box
                      sx={{
                        display: 'grid',
                        gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))',
                        gap: 2,
                      }}
                    >
                      <TextField
                        id="chat-image-negative-prompt"
                        label={t.chatImageNegativePrompt}
                        multiline
                        minRows={2}
                        fullWidth
                        value={c.imageSettings.negative_prompt}
                        onChange={(event) =>
                          c.setImageSettings((s) => ({ ...s, negative_prompt: event.target.value }))
                        }
                        disabled={c.streaming}
                        placeholder={defaultHint(defaultLabel, imageDefaults?.negative_prompt)}
                        sx={{ gridColumn: '1 / -1' }}
                      />
                      <SearchableSelect
                        id="chat-image-sampler"
                        label={t.chatImageSampler}
                        value={c.imageSettings.sampler}
                        onChange={(value) => c.setImageSettings((s) => ({ ...s, sampler: value }))}
                        disabled={c.streaming}
                        helperText={defaultHint(defaultLabel, imageDefaults?.sampler)}
                        options={imageChoices(
                          optionsOr(imageOptions?.samplers, IMAGE_SAMPLERS),
                          c.imageSettings.sampler,
                          (sampler) => (sampler === '' ? defaultLabel : sampler),
                        )}
                      />
                      <SearchableSelect
                        id="chat-image-scheduler"
                        label={t.chatImageScheduler}
                        value={c.imageSettings.scheduler}
                        onChange={(value) =>
                          c.setImageSettings((s) => ({ ...s, scheduler: value }))
                        }
                        disabled={c.streaming}
                        helperText={defaultHint(defaultLabel, imageDefaults?.scheduler)}
                        options={imageChoices(
                          optionsOr(imageOptions?.schedulers, IMAGE_SCHEDULERS),
                          c.imageSettings.scheduler,
                          (scheduler) => (scheduler === '' ? defaultLabel : scheduler),
                        )}
                      />
                      <TextField
                        id="chat-image-steps"
                        label={t.chatImageSteps}
                        type="number"
                        value={c.imageSettings.steps}
                        placeholder={hintOr(imageDefaults?.steps)}
                        onChange={(event) =>
                          c.setImageSettings((s) => ({ ...s, steps: event.target.value }))
                        }
                        disabled={c.streaming}
                      />
                      <TextField
                        id="chat-image-cfg"
                        label={t.chatImageCFG}
                        type="number"
                        value={c.imageSettings.cfg}
                        placeholder={hintOr(imageDefaults?.cfg)}
                        onChange={(event) =>
                          c.setImageSettings((s) => ({ ...s, cfg: event.target.value }))
                        }
                        disabled={c.streaming}
                      />
                      <TextField
                        id="chat-image-seed"
                        label={t.chatImageSeed}
                        type="number"
                        value={c.imageSettings.seed}
                        placeholder={hintOr(imageDefaults?.seed)}
                        onChange={(event) =>
                          c.setImageSettings((s) => ({ ...s, seed: event.target.value }))
                        }
                        disabled={c.streaming}
                      />
                      <TextField
                        id="chat-image-clip-skip"
                        label={t.chatImageClipSkip}
                        type="number"
                        value={c.imageSettings.clip_skip}
                        placeholder={hintOr(imageDefaults?.clip_skip)}
                        onChange={(event) =>
                          c.setImageSettings((s) => ({ ...s, clip_skip: event.target.value }))
                        }
                        disabled={c.streaming}
                      />
                      {/* The edit knob: how far an image edit departs from its
                        source. Blank = sd-server's own default (0.75, stated in
                        the tooltip, which also notes that reference-edit models
                        often want more); sd-server ignores it for a generation. */}
                      <Tooltip title={t.chatImageStrengthHint} describeChild>
                        <TextField
                          id="chat-image-strength"
                          label={t.chatImageStrength}
                          type="number"
                          value={c.imageSettings.strength}
                          placeholder={defaultLabel}
                          onChange={(event) =>
                            c.setImageSettings((s) => ({ ...s, strength: event.target.value }))
                          }
                          disabled={c.streaming}
                          slotProps={{ htmlInput: { min: 0, max: 1, step: 0.05 } }}
                        />
                      </Tooltip>
                    </Box>
                  )}
                </>
              )}
              {/* Speech-synthesis knobs (/v1/audio/speech). The voice is a combobox over
                the voices the gateway has cached for the model (c.modelVoices) that
                stays FREE TEXT -- the cache is only what was seen, so any name may be
                typed -- and the model's own default is only a placeholder, never
                written. A blank speed leaves the model's rate (placeholder 1.0; not
                clamped, the model's own range applies). The output is always WAV, so
                there is no format control, just the static hint. The "Advanced"
                section holds a JSON object of extra request fields (OpenAI
                `instructions`, model-native knobs). */}
              {isSpeechThread && (
                <>
                  <Box
                    sx={{
                      display: 'grid',
                      gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))',
                      gap: 2,
                    }}
                  >
                    <SpeechVoiceField
                      id="chat-speech-voice"
                      value={c.audioSettings.voice}
                      voices={c.modelVoices}
                      onChange={(voice) => c.setAudioSettings((s) => ({ ...s, voice }))}
                      disabled={c.streaming}
                      t={t}
                    />
                    <TextField
                      id="chat-speech-speed"
                      label={t.chatSpeechSpeed}
                      type="number"
                      value={c.audioSettings.speed}
                      placeholder={SPEECH_SPEED_PLACEHOLDER}
                      onChange={(event) =>
                        c.setAudioSettings((s) => ({ ...s, speed: event.target.value }))
                      }
                      disabled={c.streaming}
                      slotProps={{
                        inputLabel: { shrink: true },
                        input: { notched: true },
                        htmlInput: { min: 0, step: 0.1 },
                      }}
                    />
                  </Box>
                  {/* The streaming toggle exists only for a model the gateway says it can
                    stream (c.speechStreaming: it streams and its output rate is known).
                    On by default; only its opt-out is persisted. The run decides again
                    server-side, so this is a choice, never an authority. */}
                  {c.speechStreaming && (
                    <FormControlLabel
                      sx={{ justifySelf: 'start', mr: 0 }}
                      control={
                        <Checkbox
                          size="small"
                          checked={c.audioSettings.stream}
                          onChange={(event) =>
                            c.setAudioSettings((s) => ({ ...s, stream: event.target.checked }))
                          }
                          disabled={c.streaming}
                          slotProps={{ input: { 'aria-describedby': 'chat-speech-stream-hint' } }}
                        />
                      }
                      label={t.chatSpeechStream}
                    />
                  )}
                  {c.speechStreaming && (
                    <Typography
                      id="chat-speech-stream-hint"
                      variant="caption"
                      color="text.secondary"
                    >
                      {t.chatSpeechStreamHint}
                    </Typography>
                  )}
                  {/* The retention of the answers' audio (#281): keep all, keep the last N, or
                    (streaming only) do not store. Keyed by the chat, since its keep-last-N
                    choice is local to one thread's text. */}
                  <AudioRetentionControl
                    key={c.activeChatId ?? ''}
                    keepAudio={c.audioSettings.keepAudio}
                    keepLast={c.audioSettings.keepLast}
                    speechStreaming={c.speechStreaming}
                    disabled={c.streaming}
                    onChange={(patch) => c.setAudioSettings((s) => ({ ...s, ...patch }))}
                    pending={c.pendingAudioRetention}
                    onApply={c.confirmAudioRetention}
                    onDiscard={c.cancelAudioRetention}
                    t={t}
                  />
                  <Typography variant="caption" color="text.secondary">
                    {t.chatSpeechFormatHint}
                  </Typography>
                  <Button
                    variant="text"
                    size="small"
                    aria-expanded={speechAdvancedOpen}
                    onClick={() => setShowSpeechAdvanced(toggleOpen)}
                    sx={{ justifySelf: 'start' }}
                  >
                    {t.chatSpeechAdvanced}
                  </Button>
                  {speechAdvancedOpen && (
                    <TextField
                      id="chat-speech-passthrough"
                      label={t.chatSpeechPassthrough}
                      multiline
                      minRows={4}
                      fullWidth
                      value={c.audioSettings.passthrough}
                      onChange={(event) =>
                        c.setAudioSettings((s) => ({ ...s, passthrough: event.target.value }))
                      }
                      disabled={c.streaming}
                      error={passthroughInvalid}
                      helperText={
                        passthroughInvalid
                          ? audioPassthroughMessage(passthroughParse, t)
                          : t.chatSpeechPassthroughHint
                      }
                      slotProps={{ htmlInput: { spellCheck: false } }}
                      sx={{ '& textarea': { fontFamily: 'var(--font-mono, monospace)' } }}
                    />
                  )}
                </>
              )}
              {/* Speech-to-text knobs (/v1/audio/transcriptions). The language is FREE TEXT
                (an ISO-639-1 code; blank = the model detects it), the prompt is free text, and
                a blank temperature leaves the model's own (a typed 0 is a value, not blank;
                not clamped, the model's own range applies). The reply is always text, so there
                is no format control, just the static hint. The "Advanced" section holds a JSON
                object of extra request fields, sent as form fields. */}
              {isTranscriptionThread && (
                <>
                  <Box
                    sx={{
                      display: 'grid',
                      gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))',
                      gap: 2,
                    }}
                  >
                    <TextField
                      id="chat-transcription-language"
                      label={t.chatTranscriptionLanguage}
                      value={c.transcriptionSettings.language}
                      placeholder={t.chatTranscriptionDefault}
                      helperText={t.chatTranscriptionLanguageHint}
                      onChange={(event) =>
                        c.setTranscriptionSettings((s) => ({ ...s, language: event.target.value }))
                      }
                      disabled={c.streaming}
                      // Pinned shrunk with the notch open, like the speech fields, so the
                      // placeholder (the model-default hint) is visible, not hidden behind the
                      // resting label.
                      slotProps={{ inputLabel: { shrink: true }, input: { notched: true } }}
                    />
                    <TextField
                      id="chat-transcription-temperature"
                      label={t.chatTranscriptionTemperature}
                      type="number"
                      value={c.transcriptionSettings.temperature}
                      placeholder={t.chatTranscriptionDefault}
                      onChange={(event) =>
                        c.setTranscriptionSettings((s) => ({
                          ...s,
                          temperature: event.target.value,
                        }))
                      }
                      disabled={c.streaming}
                      slotProps={{
                        inputLabel: { shrink: true },
                        input: { notched: true },
                        htmlInput: { min: 0, step: 0.1 },
                      }}
                    />
                    <TextField
                      id="chat-transcription-prompt"
                      label={t.chatTranscriptionPrompt}
                      multiline
                      minRows={2}
                      value={c.transcriptionSettings.prompt}
                      placeholder={t.chatTranscriptionDefault}
                      helperText={t.chatTranscriptionPromptHint}
                      onChange={(event) =>
                        c.setTranscriptionSettings((s) => ({ ...s, prompt: event.target.value }))
                      }
                      disabled={c.streaming}
                      slotProps={{ inputLabel: { shrink: true }, input: { notched: true } }}
                      sx={{ gridColumn: '1 / -1' }}
                    />
                  </Box>
                  <Typography variant="caption" color="text.secondary">
                    {t.chatTranscriptionFormatHint}
                  </Typography>
                  <Button
                    variant="text"
                    size="small"
                    aria-expanded={transcriptionAdvancedOpen}
                    onClick={() => setShowTranscriptionAdvanced(toggleOpen)}
                    sx={{ justifySelf: 'start' }}
                  >
                    {t.chatTranscriptionAdvanced}
                  </Button>
                  {transcriptionAdvancedOpen && (
                    <TextField
                      id="chat-transcription-passthrough"
                      label={t.chatTranscriptionPassthrough}
                      multiline
                      minRows={4}
                      fullWidth
                      value={c.transcriptionSettings.passthrough}
                      onChange={(event) =>
                        c.setTranscriptionSettings((s) => ({
                          ...s,
                          passthrough: event.target.value,
                        }))
                      }
                      disabled={c.streaming}
                      error={transcriptionPassthroughInvalid}
                      helperText={
                        transcriptionPassthroughInvalid
                          ? transcriptionPassthroughMessage(transcriptionPassthroughParse, t)
                          : t.chatTranscriptionPassthroughHint
                      }
                      slotProps={{ htmlInput: { spellCheck: false } }}
                      sx={{ '& textarea': { fontFamily: 'var(--font-mono, monospace)' } }}
                    />
                  )}
                </>
              )}
              {/* The server-override picker is hidden entirely when the caller
                manages zero servers AND the run-as token does not lock it —
                otherwise there would be nothing to override onto. A locked
                token still shows the control (read-only) even with zero
                manageable servers, so the operator can see what is in effect. */}
              {(c.manageableServers.length > 0 || c.serverOverrideLocksChat) && (
                <Box sx={{ display: 'grid', gap: 1 }}>
                  <SearchableSelect
                    id="chat-server-override"
                    label={t.serverOverrideLabel}
                    value={c.effectiveServerOverride}
                    onChange={(value) => c.setServerOverride(value)}
                    disabled={c.streaming || c.serverOverrideLocksChat}
                    helperText={
                      c.serverOverrideLocksChat ? t.serverOverrideLockedHint : t.serverOverrideNote
                    }
                    options={[
                      { value: '', label: t.serverOverrideNone },
                      ...c.manageableServers.map((server) => ({
                        value: server.id,
                        label:
                          server.status === 'maintenance'
                            ? `${server.name} (${t.statusMaintenance})`
                            : server.name,
                      })),
                      // A locked override whose server is not among the caller's
                      // manageable servers (known limit — the run-as token may
                      // belong to a different owner) still needs an option so the
                      // picker shows its id instead of rendering blank.
                      ...(c.serverOverrideLocksChat &&
                      !c.manageableServers.some((server) => server.id === c.effectiveServerOverride)
                        ? [{ value: c.effectiveServerOverride, label: c.effectiveServerOverride }]
                        : []),
                    ]}
                  />
                  {c.effectiveServerOverride !== '' && (
                    <>
                      <FormControlLabel
                        control={
                          <Checkbox
                            checked={c.effectiveServerOverrideForce}
                            onChange={(event) =>
                              c.setServerOverrideForceUnreachable(event.target.checked)
                            }
                            disabled={c.streaming || c.serverOverrideLocksChat}
                          />
                        }
                        label={t.serverOverrideForceLabel}
                      />
                      <Typography variant="caption" color="text.secondary">
                        {t.serverOverrideForceHelp}
                      </Typography>
                    </>
                  )}
                </Box>
              )}
            </Box>
          )}

          <Box
            sx={{
              minHeight: 260,
              my: 3,
              border: '1px solid var(--line)',
              bgcolor: 'var(--page)',
              p: 2.25,
            }}
            role="log"
            aria-label={t.chatStreamLabel}
            aria-live="polite"
            aria-relevant="additions text"
          >
            {c.messages.length === 0 && (
              <Typography sx={{ color: 'var(--muted)', fontWeight: 700 }}>
                {t.emptyChatMessage}
              </Typography>
            )}
            {c.messages.map((message, index) => {
              const handlers = c.handlersFor(message.id);
              const isLast = index === c.messages.length - 1;
              const previous = index > 0 ? c.messages[index - 1] : undefined;
              const promptText =
                message.role === 'assistant' && previous?.role === 'user'
                  ? textOf(previous.content)
                  : undefined;
              const rowStreaming = c.streaming && isLast && message.role === 'assistant';
              return (
                <ChatMessage
                  key={message.id}
                  t={t}
                  role={message.role}
                  content={message.content}
                  turnId={message.id}
                  promptText={promptText}
                  reasoning={message.reasoning}
                  reasoningMs={message.reasoningMs}
                  ttftMs={message.ttftMs}
                  tps={message.tps}
                  tokensPerSecond={message.tokensPerSecond}
                  streaming={rowStreaming}
                  // kind/elapsedMs are only ever consulted while `streaming`
                  // is also true (ChatMessage's pending-render guard), so
                  // gate them the SAME way: every other row keeps a stable
                  // `undefined` across renders. runElapsedMs in particular
                  // is recomputed on every provider render and is a
                  // different float essentially every time -- passed
                  // unconditionally, it would change on EVERY row's props on
                  // every token delta and defeat ChatMessage's memo for the
                  // whole transcript during any live run, not just the row
                  // actually in flight.
                  //
                  // c.runKind, NEVER c.chatKind: this prop describes the RUN
                  // in flight, and chatKind is the thread-level guess that
                  // drives the composer. Substituting it renders the image
                  // wait state over an entire text run in a tab whose pin is
                  // stale -- pinned by Chat.test.tsx's "renders no image wait
                  // state while the run itself reports a text kind".
                  kind={rowStreaming ? c.runKind : undefined}
                  elapsedMs={rowStreaming ? c.runElapsedMs : undefined}
                  // Gated into the streaming row like kind/elapsedMs, for the same memo
                  // reason: only that row's pending turn reads it.
                  audioLive={rowStreaming ? c.runAudioLive : undefined}
                  audioMuted={rowStreaming ? c.runAudioMuted : undefined}
                  // Mute is local playback only (reversible); Abort is the composer's own
                  // Stop -- the run's ordinary cancel, whose canceled `done` ends the bubble
                  // as a not-stored answer. Both are stable references, and only the
                  // streaming row gets them.
                  onMuteAudio={rowStreaming ? onMuteAudio : undefined}
                  onAbortAudio={rowStreaming ? c.stop : undefined}
                  onEdit={message.role === 'user' ? handlers.onEdit : undefined}
                  onRegenerate={message.role === 'assistant' ? handlers.onRegenerate : undefined}
                  // The refine of a generated image: only where it can run. attachMode
                  // 'edit' is the composer's own gate (an image thread, a model whose
                  // image_edit is served true), so the action is never offered for an
                  // edit the endpoint would refuse; it moves only with a model switch,
                  // never per token, so it does not defeat ChatMessage's memo.
                  onRefine={
                    message.role === 'assistant' && c.attachMode === 'edit'
                      ? handlers.onRefine
                      : undefined
                  }
                  // The delete of a spoken answer's audio, per message like onRegenerate; the
                  // row itself offers it only where audio is still stored.
                  onDeleteAudio={message.role === 'assistant' ? handlers.onDeleteAudio : undefined}
                  canRun={c.modelAvailable}
                />
              );
            })}
          </Box>

          {/* A transcription thread's turn is an audio clip, not text: its composer is a
            component of its own (the attach and record controls, the clip, Send). Every other
            thread keeps the text composer below, unchanged. */}
          {isTranscriptionThread ? (
            <TranscriptionComposer t={t} />
          ) : (
            <Box
              component="form"
              sx={{
                display: 'grid',
                gridTemplateColumns: 'minmax(0, 1fr) auto',
                alignItems: 'end',
                gap: 1,
              }}
              onSubmit={submit}
            >
              <TextField
                id="chat-message"
                label={promptLabel}
                helperText={promptHelperText}
                multiline
                minRows={4}
                fullWidth
                // Keep the floating label + its outline notch in sync. MUI derives the
                // notch from the OutlinedInput's own filled/focused state, which does
                // not track a controlled multiline value here — so the label shrinks
                // but the notch stays closed and the label sits ON the border. Pin the
                // label shrunk and the notch open so they always match.
                slotProps={{ inputLabel: { shrink: true }, input: { notched: true } }}
                inputRef={promptRef}
                value={c.input}
                onChange={(event) => c.setInput(event.target.value)}
                onKeyDown={(event) => {
                  // Enter sends; Shift+Enter inserts a newline. Ignore Enter while an
                  // IME composition is active (CJK etc.) so it does not send mid-word.
                  // Also ignore it while streaming or while the effective model is
                  // unavailable — mirrors the Send button's disabled condition (the
                  // store's send() guards too, but the key handler must not even try).
                  if (event.key === 'Enter' && !event.shiftKey && !event.nativeEvent.isComposing) {
                    event.preventDefault();
                    if (!c.streaming && c.modelAvailable) c.send();
                  }
                }}
                disabled={c.streaming}
              />
              {(c.images.length > 0 || c.mask !== null) && (
                <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: 1, mb: 1 }}>
                  {keyedUrls(c.images).map(({ key, url, index }) => (
                    // Keyed by the image itself, a repeat of it by a copy suffix
                    // (keyedUrls), and removed by position: attaching the identical
                    // file twice must not collapse to one key or remove both copies.
                    <Box sx={{ position: 'relative', width: 72, height: 72 }} key={key}>
                      <Box
                        component="img"
                        src={url}
                        alt={t.chatAttachedImage}
                        sx={{
                          display: 'block',
                          width: 72,
                          height: 72,
                          objectFit: 'cover',
                          border: '1px solid var(--line)',
                          borderRadius: '8px',
                        }}
                      />
                      <IconButton
                        size="small"
                        aria-label={t.chatCancel}
                        onClick={() => c.removeImage(index)}
                        sx={thumbnailRemoveSx}
                      >
                        <CloseIcon fontSize="small" />
                      </IconButton>
                    </Box>
                  ))}
                  {c.mask !== null && (
                    <Box sx={{ position: 'relative', width: 72, height: 72 }}>
                      <Box
                        component="img"
                        src={c.mask}
                        alt={t.chatAttachedMask}
                        sx={{
                          display: 'block',
                          width: 72,
                          height: 72,
                          objectFit: 'cover',
                          border: '1px solid var(--line)',
                          borderRadius: '8px',
                          background: MASK_THUMBNAIL_BACKGROUND,
                        }}
                      />
                      <IconButton
                        size="small"
                        aria-label={t.chatRemoveMask}
                        onClick={c.removeMask}
                        sx={thumbnailRemoveSx}
                      >
                        <CloseIcon fontSize="small" />
                      </IconButton>
                    </Box>
                  )}
                </Box>
              )}
              <Stack
                direction="row"
                sx={{ flexWrap: 'wrap', alignItems: 'center', gap: 1, mb: 1.5 }}
              >
                {/* describeChild -> Tooltip uses aria-describedby (not aria-label), so it
                does not add a second element matching getByLabelText. The file input
                is named by the visually-hidden label text (like the old text button),
                so getByLabelText(chatAttachImage) still resolves to the input alone.
                The wrapping <span> lets the Tooltip work while the button is disabled
                (streaming) without an MUI warning. */}
                <Tooltip title={attachTooltip} describeChild>
                  <span>
                    <IconButton
                      component="label"
                      size="small"
                      disabled={c.streaming || c.attachMode === 'none'}
                    >
                      <AddPhotoAlternateIcon fontSize="small" />
                      <Box
                        component="span"
                        sx={{
                          position: 'absolute',
                          width: 1,
                          height: 1,
                          p: 0,
                          m: '-1px',
                          overflow: 'hidden',
                          clip: 'rect(0 0 0 0)',
                          whiteSpace: 'nowrap',
                          border: 0,
                        }}
                      >
                        {t.chatAttachImage}
                      </Box>
                      <input
                        type="file"
                        // An edit's source is png or jpeg (all the edit route of
                        // sd-server reads); a text model reads any image.
                        accept={c.attachMode === 'edit' ? IMAGE_EDIT_SOURCE_ACCEPT : 'image/*'}
                        // An edit has one source image; a text model reads several.
                        multiple={c.attachMode !== 'edit'}
                        hidden
                        onChange={(event) => {
                          void c.handleFiles(event.target.files);
                          event.target.value = '';
                        }}
                      />
                    </IconButton>
                  </span>
                </Tooltip>
                {/* The mask slot of an edit (inpainting): a PNG the size of the source
                image whose transparent area marks the region to change. Only an edit
                has one, and it needs the source first -- so it is disabled, and
                says why, until one is attached. Same structure as the attach
                button above: the file input is named by the hidden label text. */}
                {c.attachMode === 'edit' && (
                  <Tooltip
                    title={c.images.length === 0 ? t.chatMaskNeedsSource : t.chatAttachMaskTooltip}
                    describeChild
                  >
                    <span>
                      <IconButton
                        component="label"
                        size="small"
                        disabled={c.streaming || c.images.length === 0}
                      >
                        <LayersIcon fontSize="small" />
                        <Box
                          component="span"
                          sx={{
                            position: 'absolute',
                            width: 1,
                            height: 1,
                            p: 0,
                            m: '-1px',
                            overflow: 'hidden',
                            clip: 'rect(0 0 0 0)',
                            whiteSpace: 'nowrap',
                            border: 0,
                          }}
                        >
                          {t.chatAttachMask}
                        </Box>
                        <input
                          type="file"
                          accept="image/png"
                          hidden
                          onChange={(event) => {
                            void c.handleMaskFile(event.target.files);
                            event.target.value = '';
                          }}
                        />
                      </IconButton>
                    </span>
                  </Tooltip>
                )}
              </Stack>
              <Tooltip
                title={!c.streaming && !c.modelAvailable ? t.chatModelUnavailable : ''}
                describeChild
              >
                <span>
                  <Button
                    type="submit"
                    variant="contained"
                    disabled={c.streaming || !c.modelAvailable}
                  >
                    {t.send}
                  </Button>
                </span>
              </Tooltip>
            </Box>
          )}
        </Paper>
      </Box>
    </>
  );
}
