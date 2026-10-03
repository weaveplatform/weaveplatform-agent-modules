package weavewire

// The display capability lists the console's displays and changes their mode.
// It runs in the console user's session (PlacementPerUserConsole), because a
// display configuration belongs to a desktop: Windows applies it per user,
// macOS to the logged-in window server, and Linux to the user's X server or
// compositor.
//
// The case it exists for is a VM window resized on the host: the host asks
// the guest for a resolution to match. Virtual GPUs accept modes they never
// listed, so a set is not limited to DisplayInfo.Modes; the guest's OS is the
// judge, and refuses what it cannot drive.

const (
	KindDisplayList = "weave.display.list"
	KindDisplaySet  = "weave.display.set"
)

// Bounds a set request is checked against before it reaches the OS. They are
// sanity limits rather than capabilities: 16K is beyond any current panel, and
// no desktop scales past 8x.
const (
	MaxDisplayPixels = 16384
	MaxDisplayScale  = 8
)

// DisplayMode is a resolution in physical pixels and a refresh rate.
type DisplayMode struct {
	Width  int `json:"width"`
	Height int `json:"height"`
	// RefreshHz is 0 where the OS does not report one (most virtual GPUs).
	RefreshHz float64 `json:"refresh_hz,omitempty"`
}

// DisplayInfo is one display attached to the console session.
type DisplayInfo struct {
	// ID is the OS's identifier: a CGDirectDisplayID on macOS, a device name
	// (\\.\DISPLAY1) on Windows, an output name (Virtual-1) on Linux.
	ID      string      `json:"id"`
	Name    string      `json:"name,omitempty"`
	Primary bool        `json:"primary,omitempty"`
	Current DisplayMode `json:"current"`
	// Modes are the modes the OS advertises. Not exhaustive on a virtual GPU,
	// which takes others (see the capability's note).
	Modes []DisplayMode `json:"modes,omitempty"`
	// Scale is the UI scale factor: 2 for a macOS Retina mode, 1.5 for
	// Windows' 150%, an output's scale on Wayland. 0 when the OS has none.
	Scale float64 `json:"scale,omitempty"`
}

// DisplayListResponse is every display in the console session.
type DisplayListResponse struct {
	Displays []DisplayInfo `json:"displays"`
}

// DisplaySetRequest changes one display. Width and Height go together; either
// the mode, the scale or both must be given.
type DisplaySetRequest struct {
	// DisplayID is the display to change; empty means the primary display.
	DisplayID string `json:"display_id,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	// RefreshHz picks among modes of the same size; 0 lets the OS choose.
	RefreshHz float64 `json:"refresh_hz,omitempty"`
	// Scale is the UI scale factor to apply; 0 leaves it as it is. An OS
	// that cannot change its scale this way answers CodeUnsupported.
	Scale float64 `json:"scale,omitempty"`
}

// DisplaySetResponse is the display as it is after the change, which the host
// should trust over what it asked for: an OS may round a custom mode to one it
// can drive.
type DisplaySetResponse struct {
	Display DisplayInfo `json:"display"`
}
