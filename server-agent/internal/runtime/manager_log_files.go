// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"log/slog"
	"unicode/utf8"
)

// This file wires a spec's declared log files (#430, design §5.3, §7, §9)
// into a generation's lifecycle. Everything here runs on the owner goroutine
// except the one waiter goroutine awaitLogFileTails starts per exit, which
// touches no owner state and reports back through a command.
//
// The lifecycle of one generation N:
//
//   - Before exec (startProcess): every declaration is resolved by the path
//     policy (prepareLogFiles). A refused one is only marked file_not_permitted
//     -- a log file never fails a start. A ${CONFIG_DIR} file is removed, so
//     N's file is N's alone and is tailed from offset 0; if the removal fails,
//     the name is stale and its tailer starts at the end of what is there,
//     after a file_stale marker. The tailers of N-1 for the same names are told
//     to end their grace now (endPredecessorGraces): N-1 is dead, so nothing
//     more of it can arrive, and its final read starts before N exists. That
//     read is not awaited, so where both read one inode (an external path, a
//     stale name) a few early bytes of N can still reach N-1's stream; a
//     ${CONFIG_DIR} file N-1 held is fully separated. A deleted state ends its
//     tailers' grace in deleteState.
//   - After exec: one tailer per accepted declaration, writing into the
//     generation's sibling stream (procLog.ForStream), which carries the
//     generation, the instance prefix and the generation's secret masker.
//   - At exit, of any kind, before anything can start N+1: every tailer is
//     stopped (its grace begins) and kept on the specState until N+1 ends it.
//     A crash's or failed start's last_error -- or the one a failure recorded
//     before killing N (a startup timeout and its kin, runningProc.failure)
//     -- gets the tails once every tailer is done (awaitLogFileTails,
//     handleLogFileTails), and only while it still describes N. A plain stop
//     gets none.
//
// Shutdown: a tailer of an exit during Close, and every tailer still in its
// grace when Close begins, is ended at once (EndGrace) -- the agent is going
// away, so there is nobody to keep the grace for -- and the waiter goroutines
// are in Manager.wg, so Close returns only after every tailer has finished.

// logFileGrace is the post-exit grace every generation's tailers get
// (tailerConfig.Grace). A var, not the constant, only so a test can raise it:
// set before NewManager and restored after Close, it is read by the owner
// goroutine alone, when a generation's tailers start.
var logFileGrace = logFileTailGrace

// logFileTailBytes bounds each LastError.LogFileTails entry (§9).
const logFileTailBytes = 2048

// prepareLogFiles resolves spec's declared log files for one launch and makes
// the generation's ${CONFIG_DIR} files its own. targets are the accepted
// declarations in declaration order, refused the names the path policy
// refused (it logs why), stale the ${CONFIG_DIR} names whose earlier file
// could not be removed. port, vendor, vendors and configFilesDir feed the
// path's placeholder expansion exactly as for the launch itself.
//
// If any target lies directly in the instance's ${CONFIG_DIR}, that
// directory is created (0755) when it is missing, as writeSpecConfigFiles
// creates it for a config file: a spec whose only ${CONFIG_DIR} use is its log
// needs it before exec, or the server cannot create the file. A failure is
// logged and the start goes ahead; the tailer then reports file_waiting.
//
// Nothing here fails the start: a log file is a diagnostic, never a reason for
// a model not to run. The refused names are marked file_not_permitted only
// once the exec has succeeded (startLogFileTailers), so a start whose exec
// fails records no file_not_permitted for them.
func prepareLogFiles(spec Spec, pol LocalPolicy, port int, vendor GPUVendor, vendors GPUVendors, configFilesDir string) (targets []logFileTarget, refused []string, stale map[string]bool) {
	configDir := ""
	for _, lf := range spec.LogFiles {
		target, err := pol.resolveLogFile(spec, lf, port, vendor, vendors, configFilesDir)
		if err != nil {
			refused = append(refused, lf.Name)
			continue
		}
		targets = append(targets, target)
		if target.InConfigDir {
			configDir = target.ConfigDir
		}
	}
	if configDir != "" {
		if err := ensureInstanceConfigDir(configDir); err != nil {
			slog.Warn("runtime: cannot create the spec's config dir for its log file", "spec", spec.ID, "error", err)
		}
	}
	for _, target := range targets {
		if !target.InConfigDir {
			continue
		}
		if err := removeConfigDirLogFile(target); err != nil {
			slog.Warn("runtime: an earlier run's log file could not be removed; this run tails it from its end", "spec", spec.ID, "log_file", target.Name, "error", err)
			if stale == nil {
				stale = map[string]bool{}
			}
			stale[target.Name] = true
		}
	}
	return targets, refused, stale
}

// endPredecessorGraces ends the grace of every earlier generation's tailer of
// st whose name the next generation declares again (targets), and forgets
// every tailer that has finished. A name the next generation does not declare
// keeps its grace: nothing of the next generation can reach that stream.
// EndGrace is not awaited before the next generation's exec, so where both
// generations read one inode (an external path or a stale name) a few early
// bytes of the next generation can still reach the predecessor's stream; a
// ${CONFIG_DIR} file is separated fully, because the predecessor holds the
// unlinked inode and its grace never adopts the new file.
func endPredecessorGraces(st *specState, targets []logFileTarget) {
	keep := st.graceTailers[:0]
	for _, t := range st.graceTailers {
		if t.finished() {
			continue
		}
		if declaresLogFile(targets, t.cfg.Target.Name) {
			t.EndGrace()
			continue
		}
		keep = append(keep, t)
	}
	clear(st.graceTailers[len(keep):])
	st.graceTailers = keep
}

func declaresLogFile(targets []logFileTarget, name string) bool {
	for _, t := range targets {
		if t.Name == name {
			return true
		}
	}
	return false
}

// startLogFileTailers starts one tailer per target on plog's generation, after
// the exec succeeded, and marks every refused name file_not_permitted on its
// stream. A stale name's tailer starts at the file's end, after file_stale; an
// external path's (not directly in ${CONFIG_DIR}) at its end as of now; a
// ${CONFIG_DIR} file -- removed before exec -- from offset 0. Each first open
// runs here, synchronously, on the owner: a few bounded syscalls, like the
// config-file writes before exec.
func startLogFileTailers(plog *procLog, targets []logFileTarget, refused []string, stale map[string]bool) []*fileTailer {
	for _, name := range refused {
		plog.ForStream(name).Mark(logEventFileNotPermitted)
	}
	if len(targets) == 0 {
		return nil
	}
	tailers := make([]*fileTailer, 0, len(targets))
	for _, target := range targets {
		sib := plog.ForStream(target.Name)
		if stale[target.Name] {
			sib.Mark(logEventFileStale)
		}
		tailers = append(tailers, startFileTailer(tailerConfig{
			Target:     target,
			Log:        sib,
			StartAtEOF: !target.InConfigDir || stale[target.Name],
			Grace:      logFileGrace,
		}))
	}
	return tailers
}

// stopLogFileTailers starts the grace of every tailer of proc, which has just
// exited, and keeps them on st so the next generation can end that grace. The
// grace is ended at once instead during Close, and when st is nil: a
// generation whose state is gone, or is being deleted with its spec, has no
// next generation to end it, and nobody left to read its last lines.
func (o *owner) stopLogFileTailers(st *specState, proc *runningProc) {
	for _, t := range proc.tailers {
		t.Stop()
		if o.closing || st == nil {
			t.EndGrace()
		}
	}
	if st != nil && !o.closing {
		st.graceTailers = append(st.graceTailers, proc.tailers...)
	}
}

// endAllLogFileGraces ends the grace of every tailer still in one (Close).
func (o *owner) endAllLogFileGraces() {
	for _, st := range o.specs {
		for _, t := range st.graceTailers {
			t.EndGrace()
		}
		st.graceTailers = nil
	}
}

// awaitLogFileTails starts the goroutine that waits for every tailer of the
// exited proc to finish and, when le is the last_error proc's exit recorded,
// posts the end of each file's text (cmdLogFileTails). le is nil for an exit
// that recorded none (a stop, a removal); the goroutine then only waits, so
// Close still waits for every tailer. It is in Manager.wg; its wait is bounded
// by the grace and one final read, and postCmd gives up once the owner has
// stopped. The next generation never waits for it.
func (o *owner) awaitLogFileTails(proc *runningProc, le *LastError) {
	if len(proc.tailers) == 0 {
		return
	}
	tailers := proc.tailers
	names := make([]string, len(tailers))
	for i, t := range tailers {
		names[i] = t.cfg.Target.Name
	}
	key, plog, m := proc.specID, proc.log, o.m
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for _, t := range tailers {
			<-t.Stop()
		}
		if le == nil {
			return
		}
		var tails []LogFileTail
		for _, name := range names {
			if tail := runeStart(plog.ForStream(name).Tail(logFileTailBytes)); tail != "" {
				tails = append(tails, LogFileTail{Name: name, Tail: tail})
			}
		}
		if len(tails) > 0 {
			m.postCmd(cmdLogFileTails{key: key, le: le, tails: tails})
		}
	}()
}

// runeStart drops the continuation bytes a byte-cut tail may begin with, so
// the tail opens on a whole character.
func runeStart(s string) string {
	for i := 0; i < len(s) && i < utf8.UTFMax; i++ {
		if utf8.RuneStart(s[i]) {
			return s[i:]
		}
	}
	return s
}

// noteGenerationFailure records st's current last_error as proc's own
// failure: called right after a failure was recorded for a live generation
// that the owner then kills for it (runningProc.failure).
func noteGenerationFailure(st *specState, proc *runningProc) {
	proc.failure = st.lastError
}

// cmdLogFileTails carries the log-file tails of one exited generation to the
// owner: key is the instance's state key, le the last_error that generation's
// exit recorded.
type cmdLogFileTails struct {
	key   string
	le    *LastError
	tails []LogFileTail
}

func (cmdLogFileTails) isCommand() {}

// handleLogFileTails attaches the tails to the instance's last_error only if
// it is still the very one the exit recorded (pointer identity): every later
// last_error -- the next failure, a refusal, an admission wait -- is a new
// value, and a successful start clears it, so a replaced or cleared
// last_error never gets another generation's tails. The last_error is
// replaced by a copy rather than changed in place: the old value may already
// have been handed out by Status.
func (o *owner) handleLogFileTails(c cmdLogFileTails) {
	st := o.specs[c.key]
	if st == nil || st.lastError == nil || st.lastError != c.le {
		return
	}
	le := *c.le
	le.LogFileTails = c.tails
	st.lastError = &le
	o.signalTransition()
}
