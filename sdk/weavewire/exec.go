package weavewire

// Exec is the largest surface the capability modules expose: running a process inside a
// guest is how setup automation, diagnosis and `weave ssh` all work.
//
// Its shape is set by the channel. Core carries addressed messages, not
// streams, and there is exactly one channel per guest — so stdio is
// multiplexed over it as Chunk events tagged with the exec id, rather than a
// vsock port per exec. A second wire is not an option: the frame protocol has
// no resync, so a second reader breaks it permanently.

// Host-to-guest.
const (
	KindExecStart = "weave.exec.start"
	// KindExecStdin carries a Chunk of process input. Sent WITHOUT a
	// correlation id: a reply per chunk would serialise the stream to one
	// chunk per round trip, and there is nothing useful to say in it. Failures
	// surface on the exit event.
	KindExecStdin  = "weave.exec.stdin"
	KindExecResize = "weave.exec.resize"
	KindExecSignal = "weave.exec.signal"
)

// Guest-to-host events. No correlation id — these answer nothing.
const (
	KindExecStdout = "weave.exec.stdout"
	KindExecStderr = "weave.exec.stderr"
	// KindExecExit is the ONLY authoritative end of an exec: stdout closing
	// means the process stopped writing, not that it finished.
	KindExecExit = "weave.exec.exit"
)

// ExecRequest asks the guest to run a process.
//
// Argv is executed directly, never through a shell. A guest pasting these into
// `sh -c` would turn every argument containing a space or a semicolon into an
// injection, and the host cannot escape correctly for three guest OSes.
type ExecRequest struct {
	// ExecID is assigned by the HOST, and every message about this exec
	// carries it.
	//
	// The host must know it before sending. The guest replies to the start
	// command and then immediately begins emitting output, both down one
	// ordered channel — so a host that learned the id from the reply would be
	// racing its own read loop, and the first chunks of a fast command (often
	// all of them) would arrive addressed to an id it had not recorded yet.
	ExecID string   `json:"exec_id"`
	Argv   []string `json:"argv"`
	// Env replaces the process environment when set; empty inherits.
	Env []string `json:"env,omitempty"`
	Dir string   `json:"dir,omitempty"`
	// TTY runs the process under a pseudo-terminal, giving it a real
	// controlling terminal. Without one most programs detect a pipe and switch
	// to block buffering, so output arrives in lumps or not until exit.
	TTY  bool   `json:"tty,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	// Stdin declares the host will send input. When false the process gets an
	// immediately-closed stdin, so a command that reads input exits instead of
	// blocking forever on a stream nobody will write.
	Stdin bool `json:"stdin,omitempty"`
}

// ExecStartResponse reports a process that started. Failure to start comes back
// as the command's error instead: the host must be able to tell "did not start"
// from "started and failed", because only the second has an exit code.
type ExecStartResponse struct {
	ExecID string `json:"exec_id"`
	PID    int    `json:"pid"`
}

type ExecResizeRequest struct {
	ExecID string `json:"exec_id"`
	Cols   uint16 `json:"cols"`
	Rows   uint16 `json:"rows"`
}

// ExecSignalRequest delivers a signal. Portable names rather than numbers,
// which differ across the guest OSes — and Windows has none at all.
type ExecSignalRequest struct {
	ExecID string `json:"exec_id"`
	Signal string `json:"signal"`
}

const (
	SignalTerm = "TERM"
	SignalKill = "KILL"
	SignalInt  = "INT"
)

// ExecExit ends an exec. Code is meaningful only when Err is empty. A process
// killed by a signal reports that signal and a conventional 128+n code, so a
// caller reading only the code still sees a failure.
type ExecExit struct {
	ExecID string `json:"exec_id"`
	Code   int    `json:"code"`
	Signal string `json:"signal,omitempty"`
	// Err is set when the guest could not run the process to completion: a
	// policy refusal mid-flight, an I/O failure, output over its cap.
	Err string `json:"err,omitempty"`
}

// IsOrderedInbound reports whether a host→guest kind must be applied in the
// order the host sent it, rather than concurrently with its neighbours.
//
// Exec stdin and a clipboard upload qualify. Their chunks are meaningful only
// in sequence, and the EOF that ends them is carried in the same kind — so a dispatcher
// that runs them concurrently can close the pipe before the data it was meant to
// carry, and every write after that fails with "file already closed". The host
// sends them in order and the channel preserves that order; this is the marker
// that says not to discard it.
//
// A clipboard set joins the same queue: it applies the upload chunks the host
// sent before it, and behind them in the queue it cannot run until every one
// has been taken. The queue is one
// module's, so this delays nothing outside the clipboard.
//
// Everything else is independent: an inventory request has no ordering
// relationship with a power request, and forcing one would only let a slow
// operation delay an urgent one.
func IsOrderedInbound(kind string) bool {
	switch kind {
	case KindExecStdin, KindClipboardUpload, KindClipboardSet:
		return true
	}
	return false
}
