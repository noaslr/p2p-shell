//go:build !windows

package main

import (
	"encoding/json"
	"os"
	"os/signal"
	"syscall"

	"github.com/pion/webrtc/v3"
	"golang.org/x/term"
)

// makeRawTerminal puts stdin into raw mode and returns a restore function.
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

// startResizeHandler listens for SIGWINCH and relays terminal resize events
// to the agent. Stops when done is closed.
func startResizeHandler(dc *webrtc.DataChannel, done <-chan struct{}) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-done:
				return
			case <-ch:
				sendResize(dc)
			}
		}
	}()
}

// notifyShutdown registers OS termination signals on ch.
func notifyShutdown(ch chan<- os.Signal) {
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
}
