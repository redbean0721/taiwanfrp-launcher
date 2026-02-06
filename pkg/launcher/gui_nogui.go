//go:build nogui

package launcher

import "errors"

func guiAvailable() bool {
	return false
}

func runGUI() error {
	return errors.New("gui disabled")
}
