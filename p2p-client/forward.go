package main

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/pion/webrtc/v3"
)

// forwardSpec describes a single local port forward rule.
type forwardSpec struct {
	LocalAddr  string // e.g. "127.0.0.1:8080"
	RemoteHost string // e.g. "localhost"
	RemotePort int    // e.g. 80
}

// forwardList implements flag.Value, allowing -L to be repeated.
type forwardList []forwardSpec

func (fl *forwardList) String() string {
	if len(*fl) == 0 {
		return ""
	}
	parts := make([]string, len(*fl))
	for i, f := range *fl {
		parts[i] = fmt.Sprintf("%s → %s:%d", f.LocalAddr, f.RemoteHost, f.RemotePort)
	}
	return strings.Join(parts, ", ")
}

func (fl *forwardList) Set(s string) error {
	spec, err := parseForwardSpec(s)
	if err != nil {
		return err
	}
	*fl = append(*fl, spec)
	return nil
}

// parseForwardSpec parses "[localHost:]localPort:remoteHost:remotePort".
// Mirrors SSH's -L flag syntax.
func parseForwardSpec(s string) (forwardSpec, error) {
	parts := strings.Split(s, ":")
	var localAddr, remoteHost, remotePortStr string

	switch len(parts) {
	case 3:
		// localPort:remoteHost:remotePort
		localAddr = "127.0.0.1:" + parts[0]
		remoteHost = parts[1]
		remotePortStr = parts[2]
	case 4:
		// localHost:localPort:remoteHost:remotePort
		localAddr = parts[0] + ":" + parts[1]
		remoteHost = parts[2]
		remotePortStr = parts[3]
	default:
		return forwardSpec{}, fmt.Errorf("invalid format %q — use [localHost:]localPort:remoteHost:remotePort", s)
	}

	// Validate local address
	if _, _, err := net.SplitHostPort(localAddr); err != nil {
		return forwardSpec{}, fmt.Errorf("invalid local address %q: %v", localAddr, err)
	}

	// Validate remote port
	remotePort, err := strconv.Atoi(remotePortStr)
	if err != nil || remotePort < 1 || remotePort > 65535 {
		return forwardSpec{}, fmt.Errorf("invalid remote port %q", remotePortStr)
	}

	if remoteHost == "" {
		return forwardSpec{}, fmt.Errorf("remote host cannot be empty")
	}

	return forwardSpec{
		LocalAddr:  localAddr,
		RemoteHost: remoteHost,
		RemotePort: remotePort,
	}, nil
}

// listenForward binds on spec.LocalAddr and for each incoming TCP connection
// creates a new WebRTC DataChannel that the agent bridges to the remote target.
func listenForward(pc *webrtc.PeerConnection, spec forwardSpec) {
	ln, err := net.Listen("tcp", spec.LocalAddr)
	if err != nil {
		fmt.Printf("[-] Forward listen %s: %v\n", spec.LocalAddr, err)
		return
	}
	defer ln.Close()
	fmt.Printf("[+] Forwarding %s → %s:%d\n", spec.LocalAddr, spec.RemoteHost, spec.RemotePort)

	connIdx := 0
	for {
		tcpConn, err := ln.Accept()
		if err != nil {
			return
		}
		connIdx++
		label := fmt.Sprintf("fwd-%d-%s", connIdx, randStr(6))
		go handleForwardConn(pc, tcpConn, label, spec.RemoteHost, spec.RemotePort)
	}
}

// handleForwardConn manages one forwarded TCP connection over a WebRTC DataChannel.
//
// Protocol:
//  1. Client opens DataChannel "fwd-<id>" on the PeerConnection.
//  2. On DC open, client sends JSON: {"host":"remoteHost","port":remotePort}
//  3. Agent dials the TCP target and replies: {"ok":true} or {"ok":false,"error":"..."}
//  4. After ok, bidirectional binary stream: DC messages → TCP, TCP bytes → DC.
func handleForwardConn(pc *webrtc.PeerConnection, tcpConn net.Conn, label, remoteHost string, remotePort int) {
	defer tcpConn.Close()

	ordered := true
	dc, err := pc.CreateDataChannel(label, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		fmt.Printf("[-] CreateDataChannel %s: %v\n", label, err)
		return
	}

	// Buffered so OnMessage never blocks even if we haven't reached the select yet.
	ready := make(chan error, 1)

	// Step 1: send target JSON when the channel opens.
	dc.OnOpen(func() {
		b, _ := json.Marshal(map[string]interface{}{
			"host": remoteHost,
			"port": remotePort,
		})
		dc.SendText(string(b)) //nolint:errcheck
	})

	// Step 2: first message is the agent's ok/err handshake; subsequent are TCP data.
	waitingForHandshake := true
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if waitingForHandshake {
			waitingForHandshake = false
			var resp struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}
			if json.Unmarshal(msg.Data, &resp) != nil {
				ready <- fmt.Errorf("bad handshake from agent")
				return
			}
			if !resp.OK {
				ready <- fmt.Errorf("agent refused: %s", resp.Error)
				return
			}
			ready <- nil
			return
		}
		// Data phase: forward bytes from agent → TCP
		tcpConn.Write(msg.Data) //nolint:errcheck
	})

	dc.OnClose(func() {
		tcpConn.Close()
	})

	// Wait for the handshake to complete.
	select {
	case err := <-ready:
		if err != nil {
			fmt.Printf("[-] Forward %s: %v\n", label, err)
			dc.Close()
			return
		}
	case <-time.After(15 * time.Second):
		fmt.Printf("[-] Forward %s: timeout waiting for agent\n", label)
		dc.Close()
		return
	}

	// TCP → DataChannel pump (binary messages).
	buf := make([]byte, 32768)
	for {
		n, err := tcpConn.Read(buf)
		if n > 0 {
			dc.Send(buf[:n]) //nolint:errcheck
		}
		if err != nil {
			break
		}
	}
	dc.Close()
}
