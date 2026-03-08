package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"github.com/pion/webrtc/v3"
)

// sendCtrl sends an agent→browser control message: prefix 0x02 + JSON.
func sendCtrl(dc *webrtc.DataChannel, v interface{}) {
	b, _ := json.Marshal(v)
	dc.SendText("\x02" + string(b)) //nolint:errcheck
}

// handleDownload reads a local file and streams it to the browser as binary
// DataChannel messages, framed by download-start / download-end control messages.
func handleDownload(dc *webrtc.DataChannel, path string) {
	f, err := os.Open(path)
	if err != nil {
		sendCtrl(dc, map[string]interface{}{"type": "download-error", "error": err.Error()})
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		sendCtrl(dc, map[string]interface{}{"type": "download-error", "error": err.Error()})
		return
	}

	sendCtrl(dc, map[string]interface{}{
		"type": "download-start",
		"name": filepath.Base(path),
		"size": info.Size(),
	})

	buf := make([]byte, 16384)
	for {
		n, rdErr := f.Read(buf)
		if n > 0 {
			dc.Send(buf[:n]) //nolint:errcheck
		}
		if rdErr == io.EOF || rdErr != nil {
			break
		}
	}

	sendCtrl(dc, map[string]interface{}{"type": "download-end"})
}
