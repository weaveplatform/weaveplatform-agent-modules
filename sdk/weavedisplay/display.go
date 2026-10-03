// Package weavedisplay is the display capability: list the console session's
// displays and their modes, and change a display's resolution or scale — the
// guest half of resizing a VM's window on the host.
package weavedisplay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// Backend lists the displays of the console user's session. The module runs
// in that session, so a backend asks the window server, X server or
// compositor directly.
type Backend interface {
	List(ctx context.Context) ([]weavewire.DisplayInfo, error)
}

// Setter is implemented by a backend that can change a display's mode or
// scale. Without it, set answers weavewire.CodeUnsupported. A backend that
// can change one but not the other returns a *weavewire.UnsupportedError for
// the part it cannot do.
//
// req.DisplayID is always resolved to a listed display before Set is called,
// and the size and scale are within weavewire's bounds. Set returns the
// display as it is afterwards.
type Setter interface {
	Set(ctx context.Context, req weavewire.DisplaySetRequest) (weavewire.DisplayInfo, error)
}

// Errors a host can branch on, through the guest error's text.
var (
	// ErrBadRequest: a set that asks for nothing, or for something no
	// display can be.
	ErrBadRequest = errors.New("weavedisplay: bad request")
	// ErrNoDisplay: the display named is not attached, or there is none.
	ErrNoDisplay = errors.New("weavedisplay: no such display")
)

// Service is the display capability over an OS backend.
type Service struct{ b Backend }

// NewService builds the display service.
func NewService(b Backend) *Service { return &Service{b: b} }

// Capability implements weavemodule.Service.
func (s *Service) Capability() weavewire.Capability { return weavewire.Display }

// Register implements weavemodule.Service.
func (s *Service) Register(r *weavemodule.Registrar) error {
	r.Handle(weavewire.KindDisplayList, s.handleList)
	if set, ok := s.b.(Setter); ok {
		r.Handle(
			weavewire.KindDisplaySet,
			func(ctx context.Context, payload []byte) ([]byte, error) {
				return s.handleSet(ctx, set, payload)
			},
		)
	} else {
		r.Unsupported(weavewire.KindDisplaySet, "this OS cannot change a display's mode")
	}
	return nil
}

func (s *Service) list(ctx context.Context) ([]weavewire.DisplayInfo, error) {
	displays, err := s.b.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("weavedisplay: listing: %w", err)
	}
	if displays == nil {
		displays = []weavewire.DisplayInfo{}
	}
	return displays, nil
}

func (s *Service) handleList(ctx context.Context, _ []byte) ([]byte, error) {
	displays, err := s.list(ctx)
	if err != nil {
		return nil, err
	}
	return weavewire.EncodePayload(weavewire.DisplayListResponse{Displays: displays})
}

func (s *Service) handleSet(ctx context.Context, set Setter, payload []byte) ([]byte, error) {
	var req weavewire.DisplaySetRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("%w: decoding set: %w", ErrBadRequest, err)
	}
	if err := validate(req); err != nil {
		return nil, err
	}

	displays, err := s.list(ctx)
	if err != nil {
		return nil, err
	}
	target, err := resolve(displays, req.DisplayID)
	if err != nil {
		return nil, err
	}
	req.DisplayID = target.ID

	// Not checked against target.Modes: a virtual GPU drives sizes it never
	// listed, which is the whole point of matching a resized VM window. The
	// OS refuses what it cannot do, and the backend reports that.
	after, err := set.Set(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("weavedisplay: setting %s: %w", target.ID, err)
	}
	return weavewire.EncodePayload(weavewire.DisplaySetResponse{Display: after})
}

func validate(req weavewire.DisplaySetRequest) error {
	sized := req.Width != 0 || req.Height != 0
	switch {
	case !sized && req.Scale == 0:
		return fmt.Errorf("%w: neither a mode nor a scale was given", ErrBadRequest)
	case sized && (req.Width <= 0 || req.Height <= 0 ||
		req.Width > weavewire.MaxDisplayPixels || req.Height > weavewire.MaxDisplayPixels):
		return fmt.Errorf("%w: %dx%d is not a resolution (each side 1 to %d)",
			ErrBadRequest, req.Width, req.Height, weavewire.MaxDisplayPixels)
	case req.RefreshHz < 0:
		return fmt.Errorf("%w: refresh rate %g", ErrBadRequest, req.RefreshHz)
	case req.Scale < 0 || req.Scale > weavewire.MaxDisplayScale:
		return fmt.Errorf(
			"%w: scale %g is outside 0 to %d",
			ErrBadRequest,
			req.Scale,
			weavewire.MaxDisplayScale,
		)
	}
	return nil
}

// resolve finds the display a request names: by id, or the primary display —
// the first listed when the OS marks none — when it names none.
func resolve(displays []weavewire.DisplayInfo, id string) (weavewire.DisplayInfo, error) {
	if len(displays) == 0 {
		return weavewire.DisplayInfo{}, fmt.Errorf("%w: the session has no displays", ErrNoDisplay)
	}
	for _, d := range displays {
		if (id == "" && d.Primary) || (id != "" && d.ID == id) {
			return d, nil
		}
	}
	if id == "" {
		return displays[0], nil
	}
	return weavewire.DisplayInfo{}, fmt.Errorf("%w: %q", ErrNoDisplay, id)
}
