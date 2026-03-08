package main

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"

	"github.com/pion/webrtc/v3"
)

// handleForwardChannel services a port-forward DataChannel opened by p2p-client.
//
// Protocol (mirrors p2p-client/forward.go):
//  1. First message (text): {"host":"remoteHost","port":remotePort}
//  2. Agent dials the TCP target; replies {"ok":true} or {"ok":false,"error":"..."}
//  3. After ok: bidirectional binary stream — DC messages → TCP, TCP bytes → DC.
func handleForwardChannel(dc *webrtc.DataChannel) {
	dc.OnOpen(func() {
		fmt.Printf("[+] Forward channel open: %s\n", dc.Label())
	})

	// waitingForTarget is accessed only from OnMessage (single goroutine in pion).
	waitingForTarget := true
	var tcpConn net.Conn

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if waitingForTarget {
			waitingForTarget = false

			// Parse the JSON target sent by p2p-client.
			var target struct {
				Host string `json:"host"`
				Port int    `json:"port"`
			}
			if err := json.Unmarshal(msg.Data, &target); err != nil || target.Host == "" || target.Port == 0 {
				sendFwdResp(dc, false, "invalid target descriptor")
				dc.Close()
				return
			}

			addr := net.JoinHostPort(target.Host, strconv.Itoa(target.Port))
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				sendFwdResp(dc, false, err.Error())
				dc.Close()
				return
			}
			tcpConn = conn
			sendFwdResp(dc, true, "")
			fmt.Printf("[+] Forward %s → %s\n", dc.Label(), addr)

			// TCP → DataChannel pump.
			go func() {
				defer dc.Close()
				buf := make([]byte, 32768)
				for {
					n, err := conn.Read(buf)
					if n > 0 {
						dc.Send(buf[:n]) //nolint:errcheck
					}
					if err != nil {
						return
					}
				}
			}()
			return
		}

		// Data phase: forward bytes from client → TCP.
		if tcpConn != nil && len(msg.Data) > 0 {
			tcpConn.Write(msg.Data) //nolint:errcheck
		}
	})

	dc.OnClose(func() {
		if tcpConn != nil {
			tcpConn.Close()
		}
		fmt.Printf("[*] Forward channel closed: %s\n", dc.Label())
	})
}

func sendFwdResp(dc *webrtc.DataChannel, ok bool, errMsg string) {
	b, _ := json.Marshal(map[string]interface{}{"ok": ok, "error": errMsg})
	dc.SendText(string(b)) //nolint:errcheck
}
