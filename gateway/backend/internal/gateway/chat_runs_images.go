// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"regexp"
	"slices"
	"strings"
)

// This file is the image kind's half of the run executor. It shares the
// reservation, the deadline, the header set and the single terminal commit
// with the text half (chat_runs.go) and NOTHING else -- deliberately.
//
// consumeRunStream cannot be reused here even in part: it opens a
// bufio.Scanner over the response body, drives the periodic checkpoint
// goroutine and computes TTFT/chars-per-second/tokens-per-second, and every
// one of those assumes a stream of deltas. /v1/images/generations answers ONE
// buffered JSON body -- no deltas, so nothing to checkpoint (a checkpoint
// would write a `pending` assistant turn with empty content that the terminal
// commit then has to replace) and no rate that could be measured rather than
// invented.

// imagesResponse is the upstream body this relay consumes:
// {"created":1,"output_format":"png","data":[{"b64_json":"..."}]}.
//
// This shape was originally taken from the repository's own fixtures
// (images_handler_test.go), which is a weak warrant, so it has since been
// measured against a real stable-diffusion.cpp server (FLUX.1-dev-FP16 GGUF,
// served as model `sd-cpp-local`; that build exposes no version endpoint).
// The server volunteers `output_format` at the top level WITHOUT being asked
// for it -- buildImagesBody sends only model/prompt/response_format -- and
// omits `revised_prompt` entirely, which imagePartsFrom already handles by
// emitting no text part.
//
// OutputFormat is what makes the data: URL's media type a REPORTED value
// rather than an assumption, and the measurement is what turns that from a
// principle into a necessity: asked for `jpeg` and for `webp` the same server
// answers `jpeg` and `webp`, and in all three cases the decoded bytes' magic
// number agrees with the field. So the value is genuinely variable, and
// hardcoding image/png would not merely be a fabricated measurement of the
// same class as a progress bar over an endpoint that reports no progress --
// it would be wrong for two formats this very server emits.
//
// Data is plural by design: imagesDataCounter's own doc comment
// (images_handler.go) says the billed quantity comes from the RESPONSE
// because "a partial failure makes those differ". One content part per item;
// never data[0].
type imagesResponse struct {
	OutputFormat string `json:"output_format"`
	Data         []struct {
		B64JSON       string `json:"b64_json"`
		RevisedPrompt string `json:"revised_prompt"`
	} `json:"data"`
}

// imageTextPart and imageURLPart are the two content parts an image turn
// emits. Declared types rather than map literals so the emitted key ORDER is
// the one the shape is documented and rendered in (`type` first), and so the
// shape is stated once here instead of at each append site.
type imageTextPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type imageURLTarget struct {
	URL string `json:"url"`
}

type imageURLPart struct {
	Type     string         `json:"type"`
	ImageURL imageURLTarget `json:"image_url"`
}

// The five terminal codes this path adds. All are CODES, not prose, for the
// reason runTimedOutMessage already is one: the frontend maps a run's error to
// a localized label, and each describes a condition a user has to be able to
// tell apart from an ordinary failure.
const (
	// imageRunDispatchFailedMessage: the run never got its request to the
	// gateway's own /v1/images/generations (or /v1/images/edits, for an edit)
	// at all -- marshalling our own body
	// failed, the request could not be constructed, or the loopback round
	// trip failed outright. All three previously returned err.Error(), and
	// the third one's Go error embeds the request URL, so a gateway that
	// cannot reach ITSELF showed the user `Post
	// "http://127.0.0.1:8080/v1/images/generations": dial tcp ...: connection
	// refused` -- an internal address and a transport detail, in a portal
	// bubble, about a condition no user can act on. One code for the three
	// because they are one fact to a user ("the request never went out") and
	// the operator's signal is the log line at the call site, which keeps the
	// full Go error.
	//
	// NOT used for a context that ended: runContextOutcome reports a deadline
	// and a cancel distinctly and is checked FIRST at the Do call site.
	imageRunDispatchFailedMessage = "gateway.chat_run_image_dispatch_failed"
	// imageRunNoImageMessage: a 2xx that produced no usable image. This is an
	// ERROR, not an empty success -- the endpoint's own counter already logs a
	// counted zero on a 2xx at Error level, and committing a turn with no
	// image would render as a blank bubble the user cannot distinguish from a
	// bug.
	imageRunNoImageMessage = "gateway.chat_run_no_image"
	// imageRunFormatUnknownMessage: a 2xx whose output_format is missing or is
	// not a media subtype this can safely name. The media type is the one
	// thing about the image that MUST come from the response; defaulting it
	// would state an unmeasured fact, and it would stay stated -- in the
	// transcript, in the download's file name, and in every later request
	// buildAPIHistory carries the part into.
	imageRunFormatUnknownMessage = "gateway.chat_run_image_format_unknown"
	// imageRunResponseUnreadableMessage: a 2xx whose body could not be turned
	// into the response this relay understands -- either the read itself
	// failed (a truncated body, a connection the upstream dropped mid-write)
	// or json.Unmarshal rejected it (an HTML error page, a proxy's own error
	// body, a truncated buffer). Both branches previously returned err.Error()
	// verbatim, so a run could end with Go's own decode message ("invalid
	// character '<' looking for beginning of value") in a portal bubble; a
	// user cannot act on that, and it names an implementation detail rather
	// than a condition. The context-ended case (deadline/cancel) is NOT this
	// code -- runContextOutcome already reports that distinctly and is checked
	// first at each call site.
	imageRunResponseUnreadableMessage = "gateway.chat_run_image_response_unreadable"
	// imageRunSourceUnreadableMessage: the turn is an edit -- its last user
	// message carries an image -- but an image it carries cannot be turned into
	// bytes this run may send: an external link (a run never fetches a url), a
	// gateway reference that was not resolved to bytes, or a data: url that does not
	// decode as one of the stored image types; or, of a mask, one that is not a PNG
	// or a second one. The run fails and sends NOTHING. Sending the rest -- the
	// prompt alone, as a generation, or the edit without the unreadable image or
	// without its mask (which would repaint what the user meant to keep) -- would
	// answer a request the user did not make with an image they did not ask for,
	// which is the silent drop every other code here exists to refuse. (An image the
	// chat simply does not hold never gets this far: the run is refused at its
	// start, see portal.Service.resolveImageRunSource.)
	imageRunSourceUnreadableMessage = "gateway.chat_run_image_source_unreadable"
	// imageRunSourceUnsupportedMessage: the turn is an edit whose SOURCE image is
	// readable but of a type the edit route does not read. sd-server's reads png and
	// jpeg only, and nothing between this run and it checks a source's type on a
	// whole-image edit (the gateway's edits endpoint converts a mask, and only then
	// looks at the source), so a webp or gif source would be forwarded and come back
	// as the upstream's own failure -- images.upstream_error, whose "please try
	// again" can never come true. Refused here instead, before anything is sent, with
	// a code of its own that says what to do (the portal labels it): the source is
	// typically the webp result of a thread whose output format is webp (a refine),
	// or an older upload that the composer's check of a NEW attachment never saw (a
	// replay). Distinct from imageRunSourceUnreadableMessage -- there the user is told
	// to attach the image again, which would change nothing here -- and from the
	// endpoint's images.edit_unsupported_format, whose wording is about masks. The
	// public /v1/images/edits is not touched: an external client may send what its
	// own sd build reads.
	imageRunSourceUnsupportedMessage = "gateway.chat_run_image_source_unsupported"
)

// imageSubtypePattern bounds what may be interpolated into the data: URL's
// media type. output_format is upstream-controlled text, and it lands in a URL
// the browser loads and the download names a file from, so only a bare media
// SUBTYPE is accepted (png, jpeg, webp -- the values sd-server and OpenAI's
// own images API report). Anything else, including a full "image/png", is
// refused rather than repaired: the alternative is letting an upstream choose
// the whole media type (a forged "text/html;base64,..." would be saved and
// opened as markup), and a refusal here is loud where a repair would be a
// guess.
//
// The length is bounded too (32, ample for every real subtype), not just the
// alphabet: without it a multi-megabyte output_format would pass the
// character check and get interpolated into every data: URL the part
// produces, with the chat store's own 4 MiB content cap as the only
// backstop -- and a run whose commit is refused for exceeding THAT cap is
// exactly the silent-success failure mode finishRunWithParts now surfaces
// loudly (see commitFailureCode in chat_runs.go).
//
// Every subtype this accepts becomes "image/<subtype>" in a data: URL that the
// run commit then stores as a chat blob, and the commit refuses a media type
// outside the portal's chatBlobMediaTypes allow-list (service_chats_blobs.go).
// The two sets MUST stay in sync: a format the pipeline can emit that the
// allow-list lacks is a generated image lost at commit with a generic error.
var imageSubtypePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9+.-]{0,31}$`)

// executeImageRun is executeRun's image branch: one buffered request to the
// gateway's own /v1/images/generations -- or, when the turn carries a source image,
// /v1/images/edits -- and one terminal commit carrying the generated image(s) as
// the assistant turn's structured content.
//
// It calls finishRunWithParts EXACTLY ONCE, on every path, because the work
// is split into a function that only COMPUTES the outcome (generateImageTurn,
// below) and this one, which only commits it. Nothing in the computation can
// return without producing an outcome, so "one terminal commit per run" is a
// property of the shape rather than of remembering to call finishRun on each
// of its error branches.
//
// The deadline is NOT applied here: executeRun already derived the bounded
// context from the run's kind before branching (see runDeadlineFor), and a
// second bound would be a second, divergent promise about the same wait.
func (s *Server) executeImageRun(ctx context.Context, owner auth.Token, run *ChatRun, prep PrepareRunResult) {
	parts, status, errMsg := s.generateImageTurn(ctx, owner, run, prep)
	s.finishRunWithParts(ctx, owner, run, status, errMsg, parts)
}

// generateImageTurn performs the single loopback request an image run makes
// and returns the turn's outcome: the content parts to commit, the run status,
// and the terminal error message. It never commits, never finishes the run and
// never publishes a delta -- there are none to publish.
//
// WHICH request is decided by the turn alone (ADR-043 R1): when the last user
// message carries an image, the turn is an EDIT of it -- multipart to
// /v1/images/edits, with the message's mask part (if it has one) as the `mask`
// field -- and otherwise a generation, the JSON body to /v1/images/generations
// that this executor has always sent. The thread's kind is image either way; the
// user attached an image or did not, and there is no mode for the executor to read.
// The response is the same document from both endpoints, so everything after the
// request is shared.
func (s *Server) generateImageTurn(ctx context.Context, owner auth.Token, run *ChatRun, prep PrepareRunResult) (json.RawMessage, string, string) {
	inputs, err := imageEditInputsOf(prep.History)
	if err != nil {
		// The operator's copy says which image and why; the user's is the code.
		log.Printf("chat run %s: the edit's source image or mask is unusable: %v", run.ID, err)
		if errors.Is(err, errImageEditSourceType) {
			return nil, "error", imageRunSourceUnsupportedMessage
		}
		return nil, "error", imageRunSourceUnreadableMessage
	}
	path := "/v1/images/generations"
	// contentType stays empty for the JSON body: setRunLoopbackHeaders already
	// states it.
	var (
		body        []byte
		contentType string
	)
	if len(inputs.sources) > 0 {
		path = "/v1/images/edits"
		body, contentType, err = buildImageEditBody(prep, inputs)
	} else {
		body, err = buildImagesBody(prep)
	}
	if err != nil {
		log.Printf("chat run %s: build images body failed: %v", run.ID, err)
		return nil, "error", imageRunDispatchFailedMessage
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.selfBaseURL+path, bytes.NewReader(body))
	if err != nil {
		log.Printf("chat run %s: build images request failed: %v", run.ID, err)
		return nil, "error", imageRunDispatchFailedMessage
	}
	s.setRunLoopbackHeaders(req, owner, run, prep)
	if contentType != "" {
		// After the shared helper, which sets application/json: a multipart body's
		// type carries its boundary, which only the writer that built it knows. It
		// is the one header the edit hop changes (see setRunLoopbackHeaders).
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if status, msg, ended := runContextOutcome(ctx); ended {
			return nil, status, msg
		}
		// The operator's copy keeps the whole Go error, internal URL and
		// all; the user's copy is the code. See
		// imageRunDispatchFailedMessage.
		log.Printf("chat run %s: images loopback request failed: %v", run.ID, err)
		return nil, "error", imageRunDispatchFailedMessage
	}
	defer resp.Body.Close()

	// Read the WHOLE body: this endpoint answers one buffered JSON payload and
	// the images cannot be decoded from a prefix of it. Deliberately not
	// bounded by s.captureMaxBytes the way relayImagesUpstreamError bounds an
	// ERROR body -- that cap (1 MiB) is smaller than an ordinary successful
	// image response, so applying it here would truncate exactly the bodies
	// this path exists for. The backstop on an absurd response is the chat
	// store's own 4 MiB content cap, which refuses the commit.
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		// The other half of the same conflation consumeRunStream handles at
		// its scanner error: once the status line has arrived, a deadline or
		// a cancel lands here as a read failure, and reporting either as a
		// generic error -- or a deadline as a cancel -- would tell the user
		// they pressed Stop when they pressed nothing.
		if status, msg, ended := runContextOutcome(ctx); ended {
			return nil, status, msg
		}
		return nil, "error", imageRunResponseUnreadableMessage
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "error", upstreamErrorCode(payload, resp.Status)
	}

	var decoded imagesResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, "error", imageRunResponseUnreadableMessage
	}
	parts, err := imagePartsFrom(decoded)
	if err != nil {
		return nil, "error", err.Error()
	}
	return parts, "completed", ""
}

// runContextOutcome maps an ended run context to its terminal (status,
// message), reporting false when the context is still live and the caller's
// own error is the real one. A DEADLINE IS NOT A CANCEL: a cancel carries the
// empty message the UI reads as "the user pressed Stop", and a run its own
// deadline ended must not claim that.
func runContextOutcome(ctx context.Context) (status, errMsg string, ended bool) {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "error", runTimedOutMessage, true
	case ctx.Err() != nil:
		return "canceled", "", true
	}
	return "", "", false
}

// upstreamErrorCode pulls error.code out of the gateway's own error envelope,
// falling back to the status line when the body is not that shape.
//
// It exists because a non-2xx that is turned into "upstream status 400 Bad
// Request" DISCARDS every code the images endpoint was built to return --
// images.prompt_required, images.stream_unsupported,
// images.response_format_unsupported, images.upstream_error -- and the run is
// then the one place in the system that cannot say what went wrong. The
// envelope is apierror.Body rather than a local anonymous struct so this
// stays tied to the shape writeJSON actually emits.
//
// executeRun's non-200 branch (chat_runs.go) had the identical loss -- it read
// no body at all -- and reuses this function verbatim rather than a second
// copy of the same envelope-vs-fallback logic.
func upstreamErrorCode(body []byte, status string) string {
	var envelope apierror.Body
	if json.Unmarshal(body, &envelope) == nil {
		if code := strings.TrimSpace(envelope.Error.Code); code != "" {
			return code
		}
	}
	return "upstream status " + status
}

// buildImagesBody is the images endpoint's own request shape: the three fields
// the relay always needs, plus whatever image settings the thread carries.
//
// model and prompt are the run's own. response_format is stated EXPLICITLY
// although validateImagesRequest also accepts its absence: it is the contract
// this executor depends on (the relay can only consume b64_json), and an
// inherited default states nothing. The remaining fields come from the thread's
// ChatImageSettings and only the SET ones are added -- an unset knob is omitted
// so sd-server stays on its own default rather than being handed an inert zero.
// The gateway's /v1/images/generations handler passes every field except
// response_format and stream through to the upstream unexamined (see
// validateImagesRequest), so these reach sd-server as sent; it honors size, n,
// output_format and output_compression and ignores the rest.
//
// The advanced sd-server knobs (negative_prompt, sampler, scheduler, steps, cfg,
// seed, clip_skip, and the edit knob strength) are NOT top-level fields:
// sd-server reads them only from a
// <sd_cpp_extra_args> block in the prompt, so buildSDExtraArgs renders the set
// ones into that block and this appends it to the prompt text. The user prompt is
// first stripped of any literal tag (stripSDExtraArgsTag) so a thread cannot
// smuggle its own block.
//
// A history with no user text yields an empty prompt, which is NOT rejected
// here: validateImagesRequest owns "a prompt is required" and answers
// images.prompt_required, and a second copy of that rule in the executor
// would be free to drift from the endpoint's. The refusal reaches the run
// through upstreamErrorCode above.
func buildImagesBody(prep PrepareRunResult) ([]byte, error) {
	return json.Marshal(imageRequestFields(prep, false))
}

// imageEditOutputFormats are the output formats sd-server's edit route can
// produce. Its generations route also writes webp; the edit route does not.
var imageEditOutputFormats = map[string]struct{}{"png": {}, "jpeg": {}}

// imageRequestFields are the request fields buildImagesBody's doc comment
// describes, as a map: json.Marshal of it is the generations body, and an edit
// (buildImageEditBody) sends the very same fields as form values, so the two
// requests cannot disagree about what the thread's settings mean. The values are
// strings and integers only.
//
// The one difference is the output format of an EDIT (edit true): a format the edit
// route cannot produce (webp) is left out, with the compression that belongs to
// it, so the upstream answers in its own default instead of refusing the whole edit
// -- which the user would see as a generic upstream error. The thread's setting
// itself is not touched; its generations keep honoring it.
func imageRequestFields(prep PrepareRunResult, edit bool) map[string]any {
	prompt := stripSDExtraArgsTag(lastUserPrompt(prep.History))
	body := map[string]any{
		"model":           prep.Settings.Model,
		"response_format": "b64_json",
	}
	if img := prep.Settings.Image; img != nil {
		if img.Size != "" {
			body["size"] = img.Size
		}
		if img.N > 0 {
			body["n"] = img.N
		}
		_, formatOK := imageEditOutputFormats[strings.ToLower(img.OutputFormat)]
		dropFormat := edit && img.OutputFormat != "" && !formatOK
		if img.OutputFormat != "" && !dropFormat {
			body["output_format"] = img.OutputFormat
		}
		if img.OutputCompression != nil && !dropFormat {
			body["output_compression"] = *img.OutputCompression
		}
		if extra := buildSDExtraArgs(img); extra != "" {
			prompt += extra
		}
	}
	body["prompt"] = prompt
	return body
}

// imageEditSource is one image the turn is an edit of: its decoded bytes and the
// media type the store holds it under.
type imageEditSource struct {
	mediaType string
	data      []byte
}

// imageEditInputs is what the last user message gives an edit: the source images,
// in order, and the mask of an inpainting edit (nil for a whole-image one). A turn
// with no source is not an edit, and then both are empty.
type imageEditInputs struct {
	sources []imageEditSource
	mask    *imageEditSource
}

// imageEditInputsOf reads the images of the LAST user message in the prepared
// history, in order: the sources, and the mask when a part is tagged as one
// (portal.ImagePurposeMask). A message with no source image is not an edit -- the
// turn is then a generation -- and a mask it carries is ignored, not sent on its
// own and not read: a mask is only ever the mask OF an edit. (The composer cannot
// produce such a message; a replay or an API client can.) Only the last user
// message counts: an earlier one's images were an earlier turn's, and an assistant
// turn's images are its results; none of them is sent anywhere.
//
// Every image that is used must be readable as a data: url of a stored image type
// (portal.DecodeImageDataURL, the same definition the run commit stores uploads
// by), and any that is not fails the whole call -- see
// imageRunSourceUnreadableMessage. A SOURCE must in addition be a png or a jpeg
// (imageEditSourceTypes): the edit route reads no other, and what it would do with a
// webp or gif is fail after the upstream round trip -- see
// imageRunSourceUnsupportedMessage. By the time the executor runs, a stored
// reference -- a source's or a mask's, they are resolved alike -- has already been
// resolved to such a url (portal.Service.PrepareChatRun, resolveImageRunSource), so
// what is left that is not one is an external link or an unresolved reference, and
// neither is ever fetched or sent. The same goes for what cannot be sent as the
// mask of an edit: one that is not a PNG (the form field is a PNG whose alpha marks
// the region to edit, and the gateway's edits endpoint converts exactly that), or a
// second one (an edit has one mask, as in OpenAI's API; picking one of two would be
// a guess about which region the user meant).
//
// The image parts are found by portal.MessageImages, the one definition of an image
// part, so this cannot count a part the resolution did not see.
func imageEditInputsOf(history []portal.ChatAPIMessage) (imageEditInputs, error) {
	message, ok := lastUserMessage(history)
	if !ok {
		return imageEditInputs{}, nil
	}
	images := portal.MessageImages(message.Content)
	masks := 0
	for _, img := range images {
		if img.Mask {
			masks++
		}
	}
	if masks == len(images) {
		return imageEditInputs{}, nil // no source: no edit, whatever else the message holds
	}
	if masks > 1 {
		return imageEditInputs{}, fmt.Errorf("%d masks, an edit takes one", masks)
	}
	var in imageEditInputs
	for i, img := range images {
		if err := in.add(i, len(images), img); err != nil {
			return imageEditInputs{}, err
		}
	}
	return in, nil
}

// add reads the image part img, the i-th (from 0) of n image parts of the message,
// and files it as a source or as the mask of the edit, refusing one that is not
// readable or not of a type that role is sent in (see imageEditInputsOf).
func (in *imageEditInputs) add(i, n int, img portal.MessageImage) error {
	mediaType, data, err := portal.DecodeImageDataURL(img.URL)
	if err != nil {
		// The url is logged by its scheme only: a data: url is the image itself.
		return fmt.Errorf("image %d of %d (%s): %w", i+1, n, urlKind(img.URL), err)
	}
	src := imageEditSource{mediaType: mediaType, data: data}
	if !img.Mask {
		if !imageEditSourceTypes[mediaType] {
			return fmt.Errorf("image %d of %d: the source is a %s: %w", i+1, n, mediaType, errImageEditSourceType)
		}
		in.sources = append(in.sources, src)
		return nil
	}
	if mediaType != mediaTypePNG {
		return fmt.Errorf("image %d of %d: the mask is a %s, not a PNG", i+1, n, mediaType)
	}
	in.mask = &src
	return nil
}

// imageEditSourceTypes are the media types the edit route of sd-server reads a source
// image in. A source of any other stored type (webp, gif) is refused before the call
// (errImageEditSourceType, imageRunSourceUnsupportedMessage); the composer's own check
// of a new attachment (IMAGE_EDIT_SOURCE_TYPES in the portal frontend) is the same set.
var imageEditSourceTypes = map[string]bool{
	mediaTypePNG: true,
	"image/jpeg": true,
}

// mediaTypePNG is the media type of a PNG image: the one a mask must be stored as
// (imageEditInputsOf), the one of the converted mask part the edits endpoint
// rebuilds a form with (writeConvertedMaskPart), and a key of the edit source
// tables below.
const mediaTypePNG = "image/png"

// errImageEditSourceType marks an edit source that is a readable image of a type the
// edit route does not read, so that the caller can tell it from an unreadable one.
var errImageEditSourceType = errors.New("edit source type not supported")

// urlKind names an image url for a log line without repeating it: a data: url
// carries the whole image and any other may carry a credential.
func urlKind(url string) string {
	switch {
	case strings.HasPrefix(strings.ToLower(url), "data:"):
		return "data url"
	case strings.HasPrefix(url, chatBlobRoutePrefix):
		return "unresolved gateway reference"
	}
	return "external url"
}

// imageEditFilenames is the file name each stored image type is sent under: the
// upstream reads a part's file name, and the extension is what a server that
// sniffs by name falls back on. The keys are exactly the allow-list
// DecodeImageDataURL enforces, so no other media type -- and nothing the url
// itself chose -- ever reaches a part header.
var imageEditFilenames = map[string]string{
	mediaTypePNG: "image.png",
	"image/jpeg": "image.jpg",
	"image/webp": "image.webp",
	"image/gif":  "image.gif",
}

// buildImageEditBody is the edit's request: the same fields buildImagesBody
// sends, as multipart form values, and the source image(s) -- plus the mask, when
// there is one -- as file parts. It returns the body and the Content-Type to send
// it under (which carries the boundary).
//
// One source is the `image` part, several are the repeated `image[]` -- the two
// spellings the endpoint reads (parseImageEditForm), in the order they came, so
// the first stays the first (sd-server reads its init image from it); the mask is
// not a source and does not count for the choice. The mask follows them as the
// `mask` part, a PNG named mask.png (the endpoint converts its alpha into the
// luminance mask sd-server reads, and refuses one that is not the first source's
// size). The fields are written in sorted order so the body is deterministic.
//
// The strength knob and the other advanced ones are not form fields: like for a
// generation they ride the prompt's <sd_cpp_extra_args> block (imageRequestFields),
// which is where sd-server's edits route reads them.
//
// The thread's size, count, output_format and output_compression travel as set,
// exactly as on a generation: the executor does not second-guess what an upstream
// accepts (the same stance the sampler and scheduler knobs take) -- with the one
// exception imageRequestFields documents: the edit route's output formats are
// narrower than a generation's (png and jpeg), and a format it cannot produce is
// left out rather than sent to be refused.
func buildImageEditBody(prep PrepareRunResult, in imageEditInputs) ([]byte, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fields := imageRequestFields(prep, true)
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		if err := mw.WriteField(name, fmt.Sprint(fields[name])); err != nil {
			return nil, "", err
		}
	}
	field := "image"
	if len(in.sources) > 1 {
		field = "image[]"
	}
	for _, src := range in.sources {
		filename, ok := imageEditFilenames[src.mediaType]
		if !ok {
			return nil, "", fmt.Errorf("no file name for media type %q", src.mediaType)
		}
		if err := writeImageFilePart(mw, field, filename, src); err != nil {
			return nil, "", err
		}
	}
	if in.mask != nil {
		if err := writeImageFilePart(mw, imageEditMaskField, imageEditMaskFilename, *in.mask); err != nil {
			return nil, "", err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

// imageEditMaskField is the form field the edit's mask travels in (OpenAI's name
// for it, which the edits endpoint reads).
const imageEditMaskField = "mask"

// writeImageFilePart writes one image as a file part of the edit's form.
func writeImageFilePart(mw *multipart.Writer, field, filename string, img imageEditSource) error {
	header := make(textproto.MIMEHeader)
	// The canonical spelling every client sends -- quoted values, name first
	// -- and not mime.FormatMediaType's (sorted parameters, unquoted tokens):
	// sd-server's form parser (cpp-httplib) has matched this header with a
	// pattern that wants exactly `form-data; name="..."; filename="..."`.
	// Every value is a constant, so there is nothing to escape.
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, field, filename))
	header.Set("Content-Type", img.mediaType)
	part, err := mw.CreatePart(header)
	if err != nil {
		return err
	}
	_, err = part.Write(img.data)
	return err
}

// sdExtraArgsOpen / sdExtraArgsClose delimit the block sd-server parses the
// native knobs out of. It extracts the JSON between them with a NON-GREEDY regex,
// so a stray closing tag in user text would truncate the block -- which is why
// stripSDExtraArgsTag removes any the user typed.
const (
	sdExtraArgsOpen  = "<sd_cpp_extra_args>"
	sdExtraArgsClose = "</sd_cpp_extra_args>"
)

// sdExtraArgs is the <sd_cpp_extra_args> JSON block, in sd-server's NATIVE schema
// (not the sdapi aliases): negative_prompt / clip_skip / seed are top-level; the
// sampler (sample_method), scheduler, step count and CFG (guidance.txt_cfg) live
// under sample_params. Every field is omitempty and each nested object is built
// only when it carries a value, so an unset knob never reaches sd-server and
// leaves its own per-model default.
//
// Strength is the one EDIT knob (top-level, like seed). It has to travel here
// because sd-server's OpenAI edits route reads no `strength` form field: it
// takes the value from this block and otherwise uses 0.75, a weak edit. A pointer
// so an explicit 0 is a value; sd-server ignores it for a generation.
type sdExtraArgs struct {
	NegativePrompt string          `json:"negative_prompt,omitempty"`
	ClipSkip       *int            `json:"clip_skip,omitempty"`
	Seed           *int            `json:"seed,omitempty"`
	Strength       *float64        `json:"strength,omitempty"`
	SampleParams   *sdSampleParams `json:"sample_params,omitempty"`
}

type sdSampleParams struct {
	SampleSteps  *int        `json:"sample_steps,omitempty"`
	SampleMethod string      `json:"sample_method,omitempty"`
	Scheduler    string      `json:"scheduler,omitempty"`
	Guidance     *sdGuidance `json:"guidance,omitempty"`
}

type sdGuidance struct {
	TxtCFG *float64 `json:"txt_cfg,omitempty"`
}

// buildSDExtraArgs renders the <sd_cpp_extra_args> block for the thread's
// advanced knobs, or "" when none is set. The payload is produced by json.Marshal
// of a typed struct, never string concatenation, so it is valid by construction
// and the one free-text knob (negative_prompt) is auto-escaped (after any literal
// tag is stripped from it, same reason as the prompt). An empty object ("{}")
// yields "", so a thread that set no advanced knob leaves the prompt pristine.
func buildSDExtraArgs(img *portal.ChatImageSettings) string {
	args := sdExtraArgs{
		NegativePrompt: stripSDExtraArgsTag(img.NegativePrompt),
		ClipSkip:       img.ClipSkip,
		Seed:           img.Seed,
		Strength:       img.Strength,
	}
	if img.Steps != nil || img.CFG != nil || img.Sampler != "" || img.Scheduler != "" {
		sp := &sdSampleParams{
			SampleSteps:  img.Steps,
			SampleMethod: img.Sampler,
			Scheduler:    img.Scheduler,
		}
		if img.CFG != nil {
			sp.Guidance = &sdGuidance{TxtCFG: img.CFG}
		}
		args.SampleParams = sp
	}
	raw, err := json.Marshal(args)
	if err != nil || string(raw) == "{}" {
		return ""
	}
	return sdExtraArgsOpen + string(raw) + sdExtraArgsClose
}

// stripSDExtraArgsTag removes every literal sd_cpp_extra_args open/close tag from
// user text, so a user cannot smuggle sampler overrides -- or break the parse by
// closing the block early -- through the prompt or the negative prompt. The block
// buildSDExtraArgs appends is then unambiguously the only one sd-server sees.
//
// It loops to a FIXPOINT, which the single-pass form does not: strings.ReplaceAll
// makes one non-overlapping pass and never re-scans its output, so a tag SPLIT by
// an inner one -- e.g. "<sd_cpp_extra_<sd_cpp_extra_args>args>" -- RE-FORMS into a
// whole tag after the pass that would have removed it, and sd-server's non-greedy
// extractor would then match that surviving user block before ours. Every
// iteration that changes anything deletes a whole tag and so shrinks the string,
// so this terminates; at the fixpoint neither tag remains.
func stripSDExtraArgsTag(s string) string {
	for {
		next := strings.ReplaceAll(s, sdExtraArgsOpen, "")
		next = strings.ReplaceAll(next, sdExtraArgsClose, "")
		if next == s {
			return s
		}
		s = next
	}
}

// lastUserPrompt is the text of the LAST user message in the prepared history
// -- the wish this run is about. The text is pulled with portal.MessageText,
// the extractor deriveChatTitle already uses, so the two shapes a content can
// have (a plain string, or an array of parts) are understood in exactly one
// place.
func lastUserPrompt(history []portal.ChatAPIMessage) string {
	if message, ok := lastUserMessage(history); ok {
		return portal.MessageText(message.Content)
	}
	return ""
}

// lastUserMessage is the LAST user message of the prepared history: the turn this
// run is about, whose text is the prompt and whose images (if any) are the
// edit's sources.
func lastUserMessage(history []portal.ChatAPIMessage) (portal.ChatAPIMessage, bool) {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "user" {
			return history[i], true
		}
	}
	return portal.ChatAPIMessage{}, false
}

// imagePartsFrom turns the upstream response into the assistant turn's
// content: one image_url part per produced image, each preceded by its own
// revised_prompt when the upstream reported one.
//
// A revised_prompt becomes an ORDINARY {"type":"text"} part rather than a new
// field on the image part: the existing text renderer then shows it and
// buildAPIHistory carries it forward, with no new shape for anything to learn.
//
// An item with no base64 payload is skipped -- it carries no image, and a
// data: URL with nothing behind it is the same blank bubble a zero-length
// data[] would be. A response that leaves no image part at all is an error,
// never an empty success.
// The two failures are checked in this order on purpose: "produced nothing"
// is the more fundamental fact, and a response with no image at all should
// not be reported as one whose media type could not be named.
//
// The media type named here (image/<output_format>) must be one the portal's
// chatBlobMediaTypes allow-list accepts, or the commit refuses the image: see
// imageSubtypePattern.
func imagePartsFrom(resp imagesResponse) (json.RawMessage, error) {
	produced := 0
	for _, item := range resp.Data {
		if item.B64JSON != "" {
			produced++
		}
	}
	if produced == 0 {
		return nil, errors.New(imageRunNoImageMessage)
	}
	mediaType, err := imageMediaType(resp.OutputFormat)
	if err != nil {
		return nil, err
	}
	parts := make([]any, 0, produced*2)
	for _, item := range resp.Data {
		if item.B64JSON == "" {
			continue
		}
		if strings.TrimSpace(item.RevisedPrompt) != "" {
			parts = append(parts, imageTextPart{Type: "text", Text: item.RevisedPrompt})
		}
		parts = append(parts, imageURLPart{
			Type:     "image_url",
			ImageURL: imageURLTarget{URL: "data:" + mediaType + ";base64," + item.B64JSON},
		})
	}
	return json.Marshal(parts)
}

// imageMediaType turns the response's own output_format ("png") into the data:
// URL's media type ("image/png"), and refuses rather than invents one when the
// response reported nothing usable. Lower-cased because a media type is
// case-insensitive and the portal's download names its file from this subtype.
func imageMediaType(outputFormat string) (string, error) {
	format := strings.TrimSpace(outputFormat)
	if !imageSubtypePattern.MatchString(format) {
		return "", errors.New(imageRunFormatUnknownMessage)
	}
	return "image/" + strings.ToLower(format), nil
}
