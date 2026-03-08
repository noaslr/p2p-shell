// p2p-agent — connects back to a P2P Shell browser session over WebRTC.
//
// Usage:
//   p2p-agent --id <PEER_ID>
//
// The PEER_ID is displayed on the P2P Shell web page.
// The agent registers with the PeerJS signaling server, sends a WebRTC
// offer to the browser peer, waits for the answer, then opens a PTY shell
// (Linux/macOS) or cmd.exe pipe (Windows) over the DataChannel.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/url"
	"os"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
)

// PeerJS cloud signaling server (free tier)
const (
	peerJSHost = "0.peerjs.com"
	peerJSPort = "443"
	peerJSPath = "/peerjs"
	peerJSKey  = "peerjs"
)

const shellURL = "https://github.com/noaslr/p2p-shell"

// sigMsg is a generic PeerJS signaling envelope.
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

	targetID := flag.String("id", "", "Browser peer ID shown on the P2P Shell page")
	flag.Parse()

	// Fall back to environment variable if --id flag was not provided.
	if *targetID == "" {
		if envID := os.Getenv("P2P_TARGET_ID"); envID != "" {
			*targetID = envID
		}
	}

	if *targetID == "" {
		fmt.Fprintf(os.Stderr, "Usage: p2p-agent --id <PEER_ID>\n       P2P_TARGET_ID=<PEER_ID> p2p-agent\n\nOpen %s to get your Peer ID.\n", shellURL)
		os.Exit(1)
	}

	myID := randStr(16)
	connID := "dc_" + randStr(10)

	// ── Connect to PeerJS signaling ──────────────────────────────────────
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

	fmt.Printf("[*] p2p-agent — %s\n", shellURL)
	fmt.Printf("[*] Connecting to signaling: %s\n", peerJSHost)
	ws, _, err := websocket.DefaultDialer.Dial(sigURL.String(), nil)
	if err != nil {
		log.Fatalf("[-] Signaling connect failed: %v", err)
	}
	defer ws.Close()

	// Expect OPEN
	_, raw, err := ws.ReadMessage()
	if err != nil {
		log.Fatalf("[-] Read OPEN failed: %v", err)
	}
	var openMsg sigMsg
	json.Unmarshal(raw, &openMsg) //nolint:errcheck
	if openMsg.Type != "OPEN" {
		log.Fatalf("[-] Expected OPEN, got: %s", openMsg.Type)
	}
	fmt.Printf("[*] Registered peer ID: %s\n", myID)
	fmt.Printf("[*] Targeting browser : %s\n", *targetID)

	// ── WebRTC PeerConnection ────────────────────────────────────────────
	iceServers := []webrtc.ICEServer{
		{URLs: []string{"stun:stun.l.google.com:19302"}},
		{URLs: []string{"stun:stun1.l.google.com:19302"}},
		{URLs: []string{"stun:stun.cloudflare.com:3478"}},
		{URLs: []string{"stun:stun.stunprotocol.org:3478"}},
	}
	// Prepend a custom STUN server if provided via environment variable.
	if customSTUN := os.Getenv("P2P_STUN_SERVER"); customSTUN != "" {
		iceServers = append([]webrtc.ICEServer{{URLs: []string{customSTUN}}}, iceServers...)
		fmt.Printf("[*] Custom STUN server: %s\n", customSTUN)
	}
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: iceServers,
	})
	if err != nil {
		log.Fatalf("[-] PeerConnection: %v", err)
	}
	defer pc.Close()

	// ── DataChannel ─────────────────────────────────────────────────────
	ordered := true
	dc, err := pc.CreateDataChannel("peerjs-dc", &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		log.Fatalf("[-] DataChannel: %v", err)
	}

	dcOpen := make(chan struct{})
	dc.OnOpen(func() { close(dcOpen) })

	// ── Create offer (complete ICE — gather before sending) ─────────────
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		log.Fatalf("[-] CreateOffer: %v", err)
	}
	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(offer); err != nil {
		log.Fatalf("[-] SetLocalDescription: %v", err)
	}
	fmt.Printf("[*] Gathering ICE candidates...\n")
	<-gatherDone
	fmt.Printf("[*] ICE gathering complete\n")

	localDesc := pc.LocalDescription()

	// Build PeerJS-compatible OFFER payload
	sdpJSON, _ := json.Marshal(map[string]interface{}{
		"type": localDesc.Type.String(),
		"sdp":  localDesc.SDP,
	})
	payloadJSON, _ := json.Marshal(map[string]interface{}{
		"sdp":           json.RawMessage(sdpJSON),
		"type":          "data",
		"connectionId":  connID,
		"metadata":      nil,
		"label":         "peerjs-dc",
		"serialization": "raw",
		"reliable":      true,
		"browser":       "p2p-agent/pion",
	})
	offerEnv, _ := json.Marshal(sigMsg{
		Type:    "OFFER",
		Payload: payloadJSON,
		Dst:     *targetID,
	})

	if err := ws.WriteMessage(websocket.TextMessage, offerEnv); err != nil {
		log.Fatalf("[-] Send offer: %v", err)
	}
	fmt.Printf("[*] Offer sent — waiting for browser to accept...\n")

	// ── Inbound signaling loop ───────────────────────────────────────────
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
			case "ANSWER":
				var p struct {
					SDP struct {
						SDP  string `json:"sdp"`
						Type string `json:"type"`
					} `json:"sdp"`
				}
				json.Unmarshal(msg.Payload, &p) //nolint:errcheck
				if err := pc.SetRemoteDescription(webrtc.SessionDescription{
					Type: webrtc.SDPTypeAnswer,
					SDP:  p.SDP.SDP,
				}); err != nil {
					fmt.Printf("[-] SetRemoteDescription: %v\n", err)
				} else {
					fmt.Printf("[*] Answer received — establishing P2P connection...\n")
				}

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
						Candidate:        p.Candidate.Candidate,
						SDPMid:           &mid,
						SDPMLineIndex:    &idx,
					})
				}

			case "LEAVE", "EXPIRE":
				fmt.Printf("[!] Peer left\n")
				os.Exit(0)

			// HEARTBEAT — silently ignored
			}
		}
	}()

	// ── Wait for DataChannel open ────────────────────────────────────────
	select {
	case <-dcOpen:
		fmt.Printf("[+] Shell DataChannel open!\n")
	case <-time.After(45 * time.Second):
		log.Fatal("[-] Timeout waiting for DataChannel")
	}

	// ── Launch PTY / pipe shell ──────────────────────────────────────────
	runShell(dc)
}
