// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"strings"
)

// This file owns the RESOLVED COMMAND: what one generation of a managed model
// process was actually launched with, in the form it is safe to report.
//
// WHY IT EXISTS. An operator debugging a spec that will not start is looking
// at a template, not at what ran. ${PORT}, ${MODEL}, ${HOST_GPU_IDS} and
// ${AGENT_ENV:NAME} are all resolved at launch, the binary came from the
// agent's own allowlist, the working directory may not be what the operator
// assumes, and the environment is a hand-built minimal block rather than the
// agent's own. The gap between "what I typed" and "what ran" is where the bug
// lives, and nothing used to keep the resolved form: ExpandPlaceholders built
// it, exec.Command consumed it, and it was dropped.
//
// WHERE IT LIVES, and why that is not an arbitrary choice. A ResolvedCommand is
// a typed field on the generation's OPENING MARKER RECORD (logs.go's
// logEventStarted / logEventStartFailed), not a per-spec slot and not a channel
// of its own. Attribution then holds structurally rather than by a rule: the
// marker IS the generation boundary and already carries the pid, so the command
// cannot drift onto the wrong attempt, and across a crash loop each attempt's
// own command sits inline with its own output -- including the fact that ${PORT}
// differed between attempts, which a single "latest command" view cannot show.
//
// It is a typed FIELD, never synthesized text in the stream. That is the same
// rule the markers themselves follow and for the same reason: text an agent
// emits is indistinguishable from output the process printed, and therefore
// forgeable -- a model server printing a convincing marker line would be read
// as a portal statement. The portal owns all wording; the agent reports only
// structure.
//
// THE MASKING RULE, which is the whole security content of this file:
//
//	A value is shown only if the gateway already has it, or the agent itself
//	computed it. Everything else is masked.
//
// That resolves to exactly two masking cases, and no others:
//
//  1. EVERY ${AGENT_ENV:NAME}-derived span, wherever it landed -- in an
//     argument as much as in an env value -- is replaced by its own
//     "${AGENT_ENV:NAME}" placeholder. This is the one class of value the
//     gateway is never given: it lives only in the AI server's own
//     environment, by the whole design of ADR-027, and a resolved copy of it
//     must not travel upward just because a panel wants to be helpful.
//  2. On a FILE-MODE agent only, every spec-supplied env VALUE is masked in
//     full. That is not a new rule -- it is precisely the one the upward
//     report already draws (report.go's redactConfigEnv): a local
//     runtime.json is the operator's own document and may legitimately hold a
//     plaintext secret, and env values are the one thing the report withholds
//     from the gateway. A panel that showed them would quietly undo that,
//     which is why this file reuses the report's own line instead of
//     inventing a second one that can drift from it.
//
// And, stated as plainly, what is deliberately NOT masked, because masking it
// would cost the panel its entire reason for existing while protecting
// nothing:
//
//   - Agent-computed values: the resolved ${PORT}, ${MODEL} and
//     ${HOST_GPU_IDS}, the agent-provided base environment (baseEnvNames), and
//     the GPU visibility variable a set_visible_devices spec receives. These
//     are the values an operator cannot see anywhere else, and "the visibility
//     variable was actually set, to these cards" is among the most useful
//     things this feature can say.
//   - Literal text the operator or the portal wrote into the spec: the binary,
//     the arguments, the working directory, env KEYS in both modes, and env
//     VALUES in gateway mode. In gateway mode that text is in the gateway's
//     own database and the portal's spec editor renders it back to the same
//     admins, so masking it would hide nothing from anyone. In file mode the
//     upward report already carries binary/args/work_dir/env keys verbatim, so
//     showing them here exports nothing the gateway does not already hold --
//     the one field the report withholds is the env value, and case 2 above is
//     exactly that field.
//
// WHAT THIS DOES NOT PROTECT, so the gap stays visible to the reader best
// placed to close it:
//
//   - A LITERAL secret typed into a spec (an argument in either mode, or an
//     env value in gateway mode) is shown, because provenance says the
//     gateway already has it. It does: it is in the gateway's database in
//     plaintext, and in file mode an argument is in the report. This is not the
//     exposure; it is a second view of one that already exists. Operator
//     guidance is unchanged: ${AGENT_ENV:NAME}, never a literal.
//   - The CHILD's own output. A model server that prints its command line at
//     startup prints the real values, and that text reaches the same operator
//     through the very stream this rides on (and through LastError.StderrTail).
//     Nothing here can mask a value the child chose to print, and claiming
//     otherwise would be the dangerous kind of comment.
//
// COPY AFFORDANCE, decided deliberately in the negative: there is none, and
// the portal must not add one. Even a completely unmasked rendering of this
// struct is not a runnable command line -- the env block REPLACES the
// environment rather than adding to it, the port was ephemeral and is stale
// (or taken) by the time anyone reads the panel, and a set_visible_devices
// child renumbers its GPUs from zero so any device-numbering argument means
// something different outside that environment. A copy button would promise
// "paste this and reproduce it" and quietly deliver something that fails for
// reasons the view itself created. The fields are rendered as selectable
// monospace text instead, which carries no such promise.
//
// RETENTION IS ALREADY-MASKED, never plaintext. The masked struct is what the
// LogStore holds; the resolved plaintext exists only as the local variables
// exec.Command is handed. So there is nothing for a later bug in the store,
// the drain, the frame or the gateway to leak, and the "nothing reaches disk"
// property of the log path covers this addition by construction rather than by
// review.
//
// THE ACCEPTED COST of living on a record inside the bounded ring: a marker can
// be EVICTED. The ring trims from the front, so a generation that has printed
// more than the per-spec capacity loses its own opening marker -- and with it
// the only copy of its command. That is a real loss in the tail-a-busy-model
// case, accepted in exchange for exact per-generation attribution, and it must
// never be allowed to read as "there was no command": the portal states it
// wherever the visible history begins with output rather than with a marker,
// the same discipline as LogEntry.DroppedBytes and the empty-buffer notice.

// ResolvedCommand is one generation's launch command in reportable form: a
// TYPED FIELD on that generation's opening marker record, and the wire shape of
// a LogEntry's "command".
//
// It carries no pid and no timestamp of its own, deliberately: the marker
// record it hangs on already has both, and the whole reason to attach it there
// is that "this pid, from this moment, running this command" is ONE fact. Two
// copies of the pid would be two things to keep in agreement.
//
// Masked reports whether anything in Args or Env was replaced, so a reader can
// state that plainly rather than hoping the operator recognises a placeholder.
// Truncated reports that the command exceeded maxResolvedCommandBytes and some
// arguments or env entries are missing -- a gap stated, never a silent
// shortening, on the same reasoning as LogEntry.DroppedBytes.
type ResolvedCommand struct {
	Binary    string   `json:"binary"`
	Args      []string `json:"args"`
	WorkDir   string   `json:"work_dir,omitempty"`
	Env       []string `json:"env"`
	Masked    bool     `json:"masked,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
}

// bytes is the command's weight against a spec buffer's capacity: every string
// it carries. Charged through logRecord.size(), so a marker carrying a command
// is bounded by the SAME operator setting the output text is -- the retention
// bound LogStore advertises has to stay true for the marker records too, and a
// command attached to a record is retained exactly as long as that record is.
func (c *ResolvedCommand) bytes() int {
	if c == nil {
		return 0
	}
	n := len(c.Binary) + len(c.WorkDir)
	for _, a := range c.Args {
		n += len(a)
	}
	for _, e := range c.Env {
		n += len(e)
	}
	return n
}

// maxResolvedCommandBytes bounds ONE retained command: the sum of every
// argument and env entry it carries. Beyond it, entries are dropped and
// Truncated is set.
//
// It exists because this struct is the one thing in the log path whose size is
// chosen by the DESIRED-CONFIG DOCUMENT rather than by an operator's buffer
// setting, and the retention bound advertised by LogStore has to stay true. A
// real command is 3-5 KiB (a long PATH is the biggest single entry), so 16 KiB
// is generous enough that truncation is a guard against a pathological or
// hostile document rather than a limit an operator can meet -- and it keeps the
// worst case across the default sixteen retained specs at 256 KiB, a rounding
// error beside the 16 MiB of output retention it sits next to.
const maxResolvedCommandBytes = 16 << 10

// resolvedCommand builds the masked, reportable command from an expansion.
//
// maskSpecEnv is the file-mode bit (see case 2 of the masking rule above): true
// on an agent whose specs come from a local file, where a spec-supplied env
// value is the one thing the gateway is deliberately not given. It is a
// parameter rather than a package-level fact because the agent's config source
// is fixed at startup and belongs to the Manager that was built for it, not to
// this function.
//
// spec is read for the fields that need no expansion at all -- binary and
// work_dir are used verbatim by startProcess, so they are verbatim here too.
func (ex expandedSpec) resolvedCommand(spec Spec, maskSpecEnv bool) ResolvedCommand {
	out := ResolvedCommand{
		Binary:  spec.Binary,
		WorkDir: spec.WorkDir,
		Args:    make([]string, 0, len(ex.args)),
		Env:     make([]string, 0, len(ex.env)),
	}
	budget := maxResolvedCommandBytes

	add := func(dst *[]string, s string) {
		if len(s) > budget {
			out.Truncated = true
			return
		}
		budget -= len(s)
		*dst = append(*dst, s)
	}

	for i, a := range ex.args {
		masked, changed := maskSecretSpans(a, ex.argSpans[i])
		out.Masked = out.Masked || changed
		add(&out.Args, masked)
	}
	for i, e := range ex.env {
		var masked string
		switch {
		case maskSpecEnv && ex.envFromSpec[i]:
			// The report's own rule, applied to the same field: the KEY
			// survives so an operator still sees WHICH variables the spec
			// sets, and only the value is withheld.
			key, _, _ := strings.Cut(e, "=")
			masked = key + "=" + envRedactedMask
			out.Masked = true
		default:
			var changed bool
			masked, changed = maskSecretSpans(e, ex.envSpans[i])
			out.Masked = out.Masked || changed
		}
		add(&out.Env, masked)
	}
	return out
}

// maskSecretSpans replaces every recorded ${AGENT_ENV:NAME} span in s with the
// placeholder that produced it, and reports whether it replaced anything.
//
// The mask is the PLACEHOLDER, not a row of bullets, and that choice does two
// jobs at once. It is unmistakably not a value, so a reader (or a colleague
// watching a screen-share) can never mistake the panel for one that leaks; and
// it names the variable, so "--api-key ${AGENT_ENV:HF_TOKEN}" tells the
// operator exactly which variable supplied the argument and therefore exactly
// what to check on the host. It also reveals nothing new: the placeholder is
// the operator's own template text, which the portal's spec editor already
// shows them.
//
// Spans arrive ascending and non-overlapping (expandSpec appends them in
// output order, one per substitution), so a single forward walk is enough. A
// span outside s, or one that runs backwards, is skipped rather than trusted:
// this function must not be the thing that panics on the launch path, and a
// nonsensical span can only come from a bug here, never from a spec.
func maskSecretSpans(s string, spans []secretSpan) (string, bool) {
	if len(spans) == 0 {
		return s, false
	}
	var b strings.Builder
	copied := 0
	changed := false
	for _, sp := range spans {
		if sp.start < copied || sp.end > len(s) || sp.end < sp.start {
			continue
		}
		b.WriteString(s[copied:sp.start])
		b.WriteString("${AGENT_ENV:" + sp.name + "}")
		copied = sp.end
		changed = true
	}
	if !changed {
		return s, false
	}
	b.WriteString(s[copied:])
	return b.String(), true
}
