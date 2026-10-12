// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState, type SubmitEvent } from 'react';
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
import type { ImageModelOptions } from '../api';
import type { Translation } from './shared/types';
import type { ChatContent } from './shared/chatContent';
import { PageTitle } from './shared/PageTitle';
import { SearchableSelect, type SearchableOption } from './shared/SearchableSelect';
import { ChatMessage } from './ChatMessage';
import {
  IMAGE_LOSSY_FORMATS,
  IMAGE_N_MAX,
  IMAGE_OUTPUT_FORMATS,
  IMAGE_SAMPLERS,
  IMAGE_SCHEDULERS,
  IMAGE_SIZE_OPTIONS,
  optionsOr,
  useChatStore,
} from './chat/ChatStore';
import { ChatSidebar } from './chat/ChatSidebar';

const SIDEBAR_COLLAPSED_KEY = 'op.chat.sidebarCollapsed';

// A bound or default the model actually reported. The options endpoint omits
// (omitempty) whatever it does not know, so `undefined` is the ONLY "not
// reported" signal: 0 and -1 are real values (clip_skip and seed report -1, a
// width/height/steps default can be 0) and must never be read as "unset". It
// also keeps an absent bound out of Math.min/Math.max (NaN) and out of a <=/>=
// comparison (silently false).
function isReported(value: number | undefined): value is number {
  return typeof value === 'number' && Number.isFinite(value);
}

// The most images one request may ask for: the picked model's reported max
// batch count when it states a usable (>= 1) one, else the static sd-server
// ceiling.
function imageCountMax(options: ImageModelOptions | null): number {
  const reported = options?.max_batch_count;
  return isReported(reported) && reported >= 1 ? Math.floor(reported) : IMAGE_N_MAX;
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
  const [showSettings, setShowSettings] = useState(false);
  // The advanced (sd-server) image knobs are collapsed by default: they are
  // power-user controls below the common size/count/format settings.
  const [showAdvanced, setShowAdvanced] = useState(false);
  const [sidebarCollapsed, setSidebarCollapsed] = useState<boolean>(readSidebarCollapsed);

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

  // Which capability the disabled attach button is about. The two are
  // different: an image GENERATOR emits images and accepts none, while a
  // non-vision model simply cannot read one. Saying "does not support images"
  // on a generator told the user the opposite of the truth.
  let attachTooltip: string = t.chatAttachImage;
  if (c.modelImageCapable) attachTooltip = t.chatImageGeneratorNoInput;
  else if (!c.modelVisionCapable) attachTooltip = t.chatImageModelUnsupported;

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
            <Button
              variant="outlined"
              size="small"
              onClick={() => setShowSettings((open) => !open)}
            >
              {t.chatSettings}
            </Button>
            {c.streaming && (
              <Button variant="outlined" size="small" onClick={c.stop}>
                {t.chatStop}
              </Button>
            )}
          </Stack>

          {showSettings && (
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
                token cap are all meaningless to the images endpoint, so an image
                thread hides them and shows the image knobs below instead. */}
              {!isImageThread && (
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
                    </Box>
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
                  onEdit={message.role === 'user' ? handlers.onEdit : undefined}
                  onRegenerate={message.role === 'assistant' ? handlers.onRegenerate : undefined}
                  canRun={c.modelAvailable}
                />
              );
            })}
          </Box>

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
              label={isImageThread ? t.chatImagePromptLabel : t.messageLabel}
              helperText={isImageThread ? t.chatImageOnlyHint : undefined}
              multiline
              minRows={4}
              fullWidth
              // Keep the floating label + its outline notch in sync. MUI derives the
              // notch from the OutlinedInput's own filled/focused state, which does
              // not track a controlled multiline value here — so the label shrinks
              // but the notch stays closed and the label sits ON the border. Pin the
              // label shrunk and the notch open so they always match.
              slotProps={{ inputLabel: { shrink: true }, input: { notched: true } }}
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
            {c.images.length > 0 && (
              <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: 1, mb: 1 }}>
                {c.images.map((url, index) => (
                  // Key/remove by index (matching the sent-message renderer):
                  // attaching the identical file twice must not collapse to one
                  // key or remove both copies.
                  <Box sx={{ position: 'relative', width: 72, height: 72 }} key={index}>
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
                      sx={{
                        position: 'absolute',
                        top: -6,
                        right: -6,
                        width: 20,
                        height: 20,
                        bgcolor: 'var(--brand-accent)',
                        color: 'var(--accent-text)',
                      }}
                    >
                      <CloseIcon fontSize="small" />
                    </IconButton>
                  </Box>
                ))}
              </Box>
            )}
            <Stack direction="row" sx={{ flexWrap: 'wrap', alignItems: 'center', gap: 1, mb: 1.5 }}>
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
                    disabled={c.streaming || !c.modelVisionCapable || c.modelImageCapable}
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
                      accept="image/*"
                      multiple
                      hidden
                      onChange={(event) => {
                        void c.handleFiles(event.target.files);
                        event.target.value = '';
                      }}
                    />
                  </IconButton>
                </span>
              </Tooltip>
              {/* The remaining capacity of an image thread. Rendered only when
                the gateway actually served the cap (imageCapacityLeft null =
                unknown): a number the portal made up would be worse than
                none. This is the one figure in the image flow that is exact
                and known BEFORE the user commits to a multi-minute wait. */}
              {isImageThread && c.imageCapacityLeft !== null && (
                <Typography
                  data-testid="chat-capacity"
                  variant="body2"
                  color={c.imageCapacityLeft <= 0 ? 'error' : undefined}
                  sx={c.imageCapacityLeft > 0 ? { color: 'var(--muted)' } : undefined}
                >
                  {c.imageCapacityLeft <= 0
                    ? t.chatCapacityExhausted
                    : t.chatCapacityRemaining(c.imageCapacityLeft)}
                </Typography>
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
        </Paper>
      </Box>
    </>
  );
}
