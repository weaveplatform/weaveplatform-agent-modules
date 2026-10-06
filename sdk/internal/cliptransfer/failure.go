package cliptransfer

import "errors"

var (
	// ErrIntegrity reports bytes that are not the item that was sent: lost
	// chunks, more or fewer bytes than declared, or another digest.
	ErrIntegrity = errors.New("not the item that was sent")
	// ErrSourceChanged reports a source with more or fewer bytes than it was
	// sized at: changed since it was copied.
	ErrSourceChanged = errors.New("the source changed since it was sized")
)

// Failure is an item that did not cross, and why: one of the
// weavewire.ClipboardReason values, with the error behind it.
type Failure struct {
	Reason string
	Err    error
}

func (f *Failure) Error() string {
	if f.Err == nil {
		return f.Reason
	}
	return f.Reason + ": " + f.Err.Error()
}

func (f *Failure) Unwrap() error { return f.Err }

// ReasonOf is the reason err gives for an item that did not cross, or
// fallback when err is not a Failure.
func ReasonOf(err error, fallback string) string {
	var f *Failure
	if errors.As(err, &f) {
		return f.Reason
	}
	return fallback
}
