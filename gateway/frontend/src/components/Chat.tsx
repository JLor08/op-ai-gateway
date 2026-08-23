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
import type { Translation } from './shared/types';
import { PageTitle } from './shared/PageTitle';
import { SearchableSelect } from './shared/SearchableSelect';
import { ChatMessage } from './ChatMessage';
import { useChatStore } from './chat/ChatStore';
import { ChatSidebar } from './chat/ChatSidebar';

const SIDEBAR_COLLAPSED_KEY = 'op.chat.sidebarCollapsed';

function readSidebarCollapsed(): boolean {
  try {
    return window.localStorage?.getItem(SIDEBAR_COLLAPSED_KEY) === '1';
  } catch {
    return false;
  }
}

// The chat transcript, settings, and stream driver live in ChatStoreProvider
// (mounted above the view switch in App) so a stream keeps running when the
// user navigates away and back. This view is a thin presentation layer over
// useChatStore(); the local "settings open" toggle and the sidebar-collapsed
// flag stay here.
export function Chat({ t }: Readonly<{ t: Translation }>) {
  const c = useChatStore();
  const [showSettings, setShowSettings] = useState(false);
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

  let modelHelperText: string | undefined;
  if (c.overrideModel !== '') {
    modelHelperText = `${t.chatModelFromToken}: ${c.overrideModel}`;
  } else if (c.effectiveServerOverride !== '') {
    modelHelperText = t.serverOverrideFilteredHint;
  }

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
              return (
                <ChatMessage
                  key={message.id}
                  t={t}
                  role={message.role}
                  content={message.content}
                  reasoning={message.reasoning}
                  reasoningMs={message.reasoningMs}
                  ttftMs={message.ttftMs}
                  tps={message.tps}
                  streaming={c.streaming && isLast && message.role === 'assistant'}
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
              label={t.messageLabel}
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
              <Tooltip
                title={!c.modelVisionCapable ? t.chatImageModelUnsupported : t.chatAttachImage}
                describeChild
              >
                <span>
                  <IconButton
                    component="label"
                    size="small"
                    disabled={c.streaming || !c.modelVisionCapable}
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
