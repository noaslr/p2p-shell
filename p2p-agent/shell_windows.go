//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/pion/webrtc/v3"
)

func runShell(dc *webrtc.DataChannel) {
	cmd := exec.Command("cmd.exe")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[-] StdinPipe: %v\n", err)
		os.Exit(1)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[-] StdoutPipe: %v\n", err)
		os.Exit(1)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[-] StderrPipe: %v\n", err)
		os.Exit(1)
	}

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "[-] Shell start: %v\n", err)
		dc.SendText(fmt.Sprintf("\r\n[-] Failed to start shell: %v\r\n", err)) //nolint:errcheck
		dc.Close()
		return
	}
	fmt.Printf("[+] Shell started: cmd.exe (PID %d)\n", cmd.Process.Pid)

	forward := func(r io.Reader) {
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				dc.SendText(string(buf[:n])) //nolint:errcheck
			}
			if err != nil {
				return
			}
		}
	}
	go forward(stdout)
	go forward(stderr)

	// Upload state — only accessed from the single OnMessage goroutine.
	var uploadFile *os.File
	var uploadName string

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		data := msg.Data
		if len(data) == 0 {
			return
		}

		// Binary message → file upload chunk
		if !msg.IsString {
			if uploadFile != nil {
				uploadFile.Write(data) //nolint:errcheck
			}
			return
		}

		// Control message: prefix 0x01
		if data[0] == 0x01 {
			var ctrl struct {
				Type string `json:"type"`
				Name string `json:"name"`
				Size int64  `json:"size"`
				Path string `json:"path"`
			}
			if json.Unmarshal(data[1:], &ctrl) != nil {
				return
			}
			switch ctrl.Type {
			case "exit":
				fmt.Printf("[*] Exit requested by browser\n")
				cmd.Process.Kill() //nolint:errcheck
				dc.Close()
				os.Exit(0)

			case "upload-start":
				destPath := filepath.Join(os.TempDir(), filepath.Base(ctrl.Name))
				f, err := os.Create(destPath)
				if err != nil {
					sendCtrl(dc, map[string]interface{}{"type": "upload-error", "error": err.Error()})
					return
				}
				uploadFile = f
				uploadName = ctrl.Name

			case "upload-end":
				if uploadFile != nil {
					path := uploadFile.Name()
					uploadFile.Close()
					uploadFile = nil
					sendCtrl(dc, map[string]interface{}{
						"type": "upload-ok",
						"name": uploadName,
						"path": path,
					})
					uploadName = ""
				}

			case "download":
				go handleDownload(dc, ctrl.Path)
			}
			return
		}

		// Regular shell input (skip resize on Windows — no PTY)
		stdin.Write(data) //nolint:errcheck
	})

	dc.OnClose(func() {
		cmd.Process.Kill() //nolint:errcheck
		os.Exit(0)
	})

	cmd.Wait() //nolint:errcheck
	dc.Close()
	os.Exit(0)
}
