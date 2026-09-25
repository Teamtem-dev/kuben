package api

import (
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/internal/store"
)

// The bounds of a freeze and of a silence.
const (
	MaxFreezeMs  = maxFreezeMs
	MaxSilenceMs = maxSilenceMs
)

// ControlText exposes controlText.
func ControlText(name, value string, maxChars int) (string, error) {
	return controlText(name, value, maxChars)
}

// ControlWindow exposes controlWindow.
func ControlWindow(body *gen.CreateWindow, now, maxMs int64) (int64, int64, error) {
	return controlWindow(body, now, maxMs)
}

// CheckOwner exposes checkOwner.
func CheckOwner(body *gen.OwnerDto) (store.NewOwner, error) { return checkOwner(body) }
