// p2p-client — CLI counterpart to p2p-web.
//
// Acts as the WebRTC ANSWERER (like the browser). Registers with PeerJS,
// prints its Peer ID, and waits for a p2p-agent to connect.
//
// Usage:
//
//	p2p-client [-L [localHost:]localPort:remoteHost:remotePort]
//
// Environment variables:
//
//	P2P_STUN_SERVER   Custom STUN server URL (prepended to defaults)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
)

// PeerJS cloud signaling server (free tier) — mirrors p2p-agent constants.
const (
	peerJSHost = "0.peerjs.com"
	peerJSPort = "443"
	peerJSPath = "/peerjs"
	peerJSKey  = "peerjs"
)

const shellURL = "https://github.com/noaslr/p2p-shell"

// sigMsg is the generic PeerJS signaling envelope (same as in p2p-agent).
type sigMsg struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Src     string          `json:"src,omitempty"`
	Dst     string          `json:"dst,omitempty"`
}

func randStr(n int) string {
	const alpha = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = alpha[rand.Intn(len(alpha))]
	}
	return string(b)
}

func main() {
	rand.Seed(time.Now().UnixNano()) //nolint:staticcheck

	var forwards forwardList
	flag.Var(&forwards, "L", "Local port forward: [localHost:]localPort:remoteHost:remotePort (repeatable)")
	flag.Parse()

	// ── ICE servers ─────────────────────────────────────────────────────
	iceServers := []webrtc.ICEServer{
		{URLs: []string{"stun:stun.l.google.com:19302"}},
		{URLs: []string{"stun:stun1.l.google.com:19302"}},
		{URLs: []string{"stun:stun.cloudflare.com:3478"}},
		{URLs: []string{"stun:stun.stunprotocol.org:3478"}},
	}
	if customSTUN := os.Getenv("P2P_STUN_SERVER"); customSTUN != "" {
		iceServers = append([]webrtc.ICEServer{{URLs: []string{customSTUN}}}, iceServers...)
		fmt.Printf("[*] Custom STUN server: %s\n", customSTUN)
	}

	// ── PeerJS signaling ─────────────────────────────────────────────────
	myID := randStr(16)
	sigURL := url.URL{
		Scheme: "wss",
		Host:   peerJSHost + ":" + peerJSPort,
		Path:   peerJSPath,
		RawQuery: url.Values{
			"key":   {peerJSKey},
			"id":    {myID},
			"token": {randStr(8)},
		}.Encode(),
	}

	fmt.Printf("[*] p2p-client — %s\n", shellURL)
	fmt.Printf("[*] Connecting to signaling: %s\n", peerJSHost)

	ws, _, err := websocket.DefaultDialer.Dial(sigURL.String(), nil)
	if err != nil {
		log.Fatalf("[-] Signaling connect failed: %v", err)
	}
	defer ws.Close()

	// Expect OPEN
	_, raw, err := ws.ReadMessage()
	if err != nil {
		log.Fatalf("[-] Read OPEN: %v", err)
	}
	var openMsg sigMsg
	json.Unmarshal(raw, &openMsg) //nolint:errcheck
	if openMsg.Type != "OPEN" {
		log.Fatalf("[-] Expected OPEN, got: %s", openMsg.Type)
	}

	fmt.Printf("[+] Peer ID: %s\n", myID)
	fmt.Printf("[*] Run on the agent:\n\n    p2p-agent --id %s\n\n", myID)
	if len(forwards) > 0 {
		fmt.Printf("[*] Port forwards queued:\n")
		for _, f := range forwards {
			fmt.Printf("    %s → %s:%d\n", f.LocalAddr, f.RemoteHost, f.RemotePort)
		}
		fmt.Println()
	}
	fmt.Printf("[*] Waiting for agent to connect...\n")

	// ── Wait for OFFER ───────────────────────────────────────────────────
	var (
		offer   webrtc.SessionDescription
		agentID string
		connID  string
	)
	for {
		_, raw, err := ws.ReadMessage()
		if err != nil {
			log.Fatalf("[-] Signaling read: %v", err)
		}
		var msg sigMsg
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}
		if msg.Type != "OFFER" {
			continue // ignore HEARTBEAT and anything else
		}

		var payload struct {
			SDP struct {
				SDP  string `json:"sdp"`
				Type string `json:"type"`
			} `json:"sdp"`
			ConnectionID string `json:"connectionId"`
		}
		if json.Unmarshal(msg.Payload, &payload) != nil || payload.SDP.SDP == "" {
			fmt.Println("[!] Malformed OFFER, ignoring")
			continue
		}

		agentID = msg.Src
		connID = payload.ConnectionID
		offer = webrtc.SessionDescription{
			Type: webrtc.SDPTypeOffer,
			SDP:  payload.SDP.SDP,
		}
		fmt.Printf("[*] Offer received from agent: %s\n", agentID)
		break
	}

	// ── WebRTC PeerConnection ────────────────────────────────────────────
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: iceServers})
	if err != nil {
		log.Fatalf("[-] PeerConnection: %v", err)
	}
	defer pc.Close()

	// Shell DataChannel arrives from agent (agent is the offerer, so it creates the DC).
	shellReady := make(chan *webrtc.DataChannel, 1)

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		switch {
		case dc.Label() == "peerjs-dc":
			dc.OnOpen(func() {
				shellReady <- dc
			})
		case strings.HasPrefix(dc.Label(), "fwd-"):
			// Unexpected: agent-initiated forward (not used in current protocol).
			// The client always initiates forwards, not the agent.
		}
	})

	// Track connection state for port forward gating and failure detection.
	connected := make(chan struct{})
	var connectedOnce sync.Once
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateConnected:
			connectedOnce.Do(func() { close(connected) })
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateDisconnected:
			fmt.Fprintf(os.Stderr, "\r\n[-] WebRTC connection %s\r\n", state)
			os.Exit(1)
		}
	})

	// ── Set remote description, create and send answer ───────────────────
	if err := pc.SetRemoteDescription(offer); err != nil {
		log.Fatalf("[-] SetRemoteDescription: %v", err)
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		log.Fatalf("[-] CreateAnswer: %v", err)
	}

	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		log.Fatalf("[-] SetLocalDescription: %v", err)
	}
	fmt.Printf("[*] Gathering ICE candidates...\n")
	<-gatherDone
	fmt.Printf("[*] ICE gathering complete\n")

	localDesc := pc.LocalDescription()
	sdpJSON, _ := json.Marshal(map[string]interface{}{
		"type": localDesc.Type.String(),
		"sdp":  localDesc.SDP,
	})
	payloadJSON, _ := json.Marshal(map[string]interface{}{
		"sdp":          json.RawMessage(sdpJSON),
		"type":         "data",
		"connectionId": connID,
		"browser":      "p2p-client/pion",
	})
	answerEnv, _ := json.Marshal(sigMsg{
		Type:    "ANSWER",
		Payload: payloadJSON,
		Dst:     agentID,
	})
	if err := ws.WriteMessage(websocket.TextMessage, answerEnv); err != nil {
		log.Fatalf("[-] Send answer: %v", err)
	}
	fmt.Printf("[*] Answer sent — establishing P2P connection...\n")

	// ── Continue signaling loop for trickled CANDIDATEs ──────────────────
	go func() {
		for {
			_, raw, err := ws.ReadMessage()
			if err != nil {
				return
			}
			var msg sigMsg
			if json.Unmarshal(raw, &msg) != nil {
				continue
			}
			switch msg.Type {
			case "CANDIDATE":
				var p struct {
					Candidate struct {
						Candidate     string `json:"candidate"`
						SDPMid        string `json:"sdpMid"`
						SDPMLineIndex uint16 `json:"sdpMLineIndex"`
					} `json:"candidate"`
				}
				json.Unmarshal(msg.Payload, &p) //nolint:errcheck
				if p.Candidate.Candidate != "" {
					mid := p.Candidate.SDPMid
					idx := p.Candidate.SDPMLineIndex
					pc.AddICECandidate(webrtc.ICECandidateInit{ //nolint:errcheck
						Candidate:     p.Candidate.Candidate,
						SDPMid:        &mid,
						SDPMLineIndex: &idx,
					})
				}
			case "LEAVE", "EXPIRE":
				fmt.Fprintf(os.Stderr, "\r\n[!] Agent disconnected\r\n")
				os.Exit(0)
			}
		}
	}()

	// ── Wait for shell DataChannel ───────────────────────────────────────
	var dc *webrtc.DataChannel
	select {
	case dc = <-shellReady:
		fmt.Printf("[+] Shell session open!\n")
	case <-time.After(45 * time.Second):
		log.Fatal("[-] Timeout waiting for DataChannel")
	}

	// ── Launch port forwards once P2P is connected ───────────────────────
	if len(forwards) > 0 {
		go func() {
			select {
			case <-connected:
			case <-time.After(45 * time.Second):
				fmt.Fprintf(os.Stderr, "[-] Timeout waiting for P2P connection for port forwards\n")
				return
			}
			for _, f := range forwards {
				go listenForward(pc, f)
			}
		}()
	}

	// ── Terminal session ─────────────────────────────────────────────────
	restore, err := makeRawTerminal()
	if err != nil {
		log.Fatalf("[-] Raw terminal mode: %v", err)
	}

	done := make(chan struct{})
	var doneOnce sync.Once
	closeDone := func() { doneOnce.Do(func() { close(done) }) }

	// OS signal → clean exit (restore terminal before quitting)
	sigCh := make(chan os.Signal, 1)
	notifyShutdown(sigCh)
	go func() {
		<-sigCh
		closeDone()
	}()

	sendResize(dc)
	startResizeHandler(dc, done)

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		// Suppress agent control messages (file transfer status, etc.)
		if msg.IsString && len(msg.Data) > 0 && msg.Data[0] == 0x02 {
			return
		}
		os.Stdout.Write(msg.Data) //nolint:errcheck
	})

	dc.OnClose(func() { closeDone() })

	// stdin → DataChannel
	go func() {
		defer closeDone()
		buf := make([]byte, 256)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				// Send as text — the agent treats binary as file upload chunks.
				dc.SendText(string(buf[:n])) //nolint:errcheck
			}
			if err != nil {
				return
			}
		}
	}()

	<-done
	restore()
	signal.Stop(sigCh)
	fmt.Fprintf(os.Stderr, "\r\n[*] Session ended\r\n")
}
