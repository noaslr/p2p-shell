//go:build windows

package main

import (
	"encoding/json"
	"os"
	"os/signal"

	"github.com/pion/webrtc/v3"
	"golang.org/x/term"
)

// makeRawTerminal puts the Windows console into raw mode via SetConsoleMode.
func makeRawTerminal() (func(), error) {
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return func() {}, err
	}
	return func() { term.Restore(fd, oldState) }, nil //nolint:errcheck
}

// sendResize reads the current terminal dimensions and sends a resize control
// message to the agent: \x01{"type":"resize","rows":N,"cols":N}
func sendResize(dc *webrtc.DataChannel) {
	cols, rows, err := term.GetSize(int(os.Stdin.Fd()))
	if err != nil || cols == 0 || rows == 0 {
		return
	}
	b, _ := json.Marshal(map[string]interface{}{
		"type": "resize",
		"rows": rows,
		"cols": cols,
	})
	dc.SendText("\x01" + string(b)) //nolint:errcheck
}

// startResizeHandler is a no-op on Windows — there is no SIGWINCH equivalent.
// A future enhancement could poll term.GetSize on a ticker.
func startResizeHandler(_ *webrtc.DataChannel, _ <-chan struct{}) {}

// notifyShutdown registers the console interrupt signal on ch.
func notifyShutdown(ch chan<- os.Signal) {
	signal.Notify(ch, os.Interrupt)
}
