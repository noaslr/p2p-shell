/**
 * P2P Shell — script.js
 *
 * Browser acts as the LISTENER.  The Go agent is the CALLER (sends the
 * WebRTC OFFER via PeerJS signaling), so the browser just needs to call
 * peer.on('connection', ...) and PeerJS handles the offer/answer exchange.
 *
 * DataChannel I/O is accessed via conn.dataChannel directly to bypass
 * PeerJS's serialization layer and get raw bytes.
 *
 * Control messages FROM browser TO agent are prefixed with 0x01:
 *   \x01{"type":"resize","rows":N,"cols":N}
 *   \x01{"type":"exit"}
 *   \x01{"type":"upload-start","name":"file.txt","size":N}
 *   \x01{"type":"upload-end"}
 *   \x01{"type":"download","path":"/remote/path"}
 *
 * Control messages FROM agent TO browser are prefixed with 0x02:
 *   \x02{"type":"upload-ok","name":"file.txt","path":"/tmp/file.txt"}
 *   \x02{"type":"upload-error","error":"..."}
 *   \x02{"type":"download-start","name":"file.txt","size":N}
 *   \x02{"type":"download-end"}
 *   \x02{"type":"download-error","error":"..."}
 *
 * Binary DataChannel messages are file data chunks (bidirectional).
 */

'use strict';

/* ═══════════════════════════════════════════════════════════
   PEER CONFIG — STUN servers
   ═══════════════════════════════════════════════════════════ */
const PEER_CONFIG = {
    config: {
        iceServers: [
            { urls: 'stun:stun.l.google.com:19302'    },
            { urls: 'stun:stun1.l.google.com:19302'   },
            { urls: 'stun:stun2.l.google.com:19302'   },
            { urls: 'stun:stun.cloudflare.com:3478'   },
            { urls: 'stun:stun.stunprotocol.org:3478' },
        ]
    }
};

/* ═══════════════════════════════════════════════════════════
   STATE
   ═══════════════════════════════════════════════════════════ */
let peer     = null;
let terminal = null;
let fitAddon = null;
let myPeerID = null;

// Raw RTCDataChannel — set when a connection is open, null otherwise.
let dc = null;

// Accumulated terminal output (raw bytes, including ANSI codes).
let sessionLog = '';

// File download state
let downloadMeta   = null;   // {name, size} when a download is in progress
let downloadChunks = [];     // ArrayBuffer[]

/* ═══════════════════════════════════════════════════════════
   DOM
   ═══════════════════════════════════════════════════════════ */
const $  = id => document.getElementById(id);
const el = (tag, cls) => { const e = document.createElement(tag); if (cls) e.className = cls; return e; };

function showPanel(id) {
    document.querySelectorAll('.panel').forEach(p => p.classList.remove('active'));
    $(id).classList.add('active');
    if (id === 'panel-terminal' && fitAddon) {
        setTimeout(() => fitAddon.fit(), 50);
    }
}

function setHeaderStatus(text, dotCls) {
    $('hdr-status-text').textContent = text;
    const dot = $('hdr-status').querySelector('.status-dot');
    dot.className = `status-dot ${dotCls}`;
}

/* ═══════════════════════════════════════════════════════════
   BASE URL (for download commands)
   ═══════════════════════════════════════════════════════════ */
function baseURL() {
    try {
        return window.top.location.origin;
    } catch {
        return window.location.origin;
    }
}

/* ═══════════════════════════════════════════════════════════
   COMMAND GENERATION
   ═══════════════════════════════════════════════════════════ */
const PLATFORMS = {
    'linux-amd64': {
        bin:  'linux-amd64',
        tmpl: (base, pid) =>
`curl -sSL ${base}/b/p2p-agent/linux-amd64 -o /tmp/p2p-agent \\
  && chmod +x /tmp/p2p-agent \\
  && /tmp/p2p-agent --id ${pid}`,
    },
    'linux-arm64': {
        bin:  'linux-arm64',
        tmpl: (base, pid) =>
`curl -sSL ${base}/b/p2p-agent/linux-arm64 -o /tmp/p2p-agent \\
  && chmod +x /tmp/p2p-agent \\
  && /tmp/p2p-agent --id ${pid}`,
    },
    'linux-arm': {
        bin:  'linux-arm',
        tmpl: (base, pid) =>
`curl -sSL ${base}/b/p2p-agent/linux-arm -o /tmp/p2p-agent \\
  && chmod +x /tmp/p2p-agent \\
  && /tmp/p2p-agent --id ${pid}`,
    },
    'darwin-arm64': {
        bin:  'darwin-arm64',
        tmpl: (base, pid) =>
`curl -sSL ${base}/b/p2p-agent/darwin-arm64 -o /tmp/p2p-agent \\
  && chmod +x /tmp/p2p-agent \\
  && /tmp/p2p-agent --id ${pid}`,
    },
    'darwin-amd64': {
        bin:  'darwin-amd64',
        tmpl: (base, pid) =>
`curl -sSL ${base}/b/p2p-agent/darwin-amd64 -o /tmp/p2p-agent \\
  && chmod +x /tmp/p2p-agent \\
  && /tmp/p2p-agent --id ${pid}`,
    },
    'windows-amd64': {
        bin:  'windows-amd64.exe',
        tmpl: (base, pid) =>
`powershell -c "Invoke-WebRequest '${base}/b/p2p-agent/windows-amd64.exe' -OutFile $env:TEMP\\p2p-agent.exe; & $env:TEMP\\p2p-agent.exe --id ${pid}"`,
    },
};

function updateCmd(platform) {
    const pre = $('cmd-display');
    if (!myPeerID) {
        pre.innerHTML = '<span class="cmd-comment"># Waiting for Peer ID…</span>';
        return;
    }
    const p = PLATFORMS[platform];
    if (!p) return;
    const raw = p.tmpl(baseURL(), myPeerID);
    pre.innerHTML = highlightCmd(raw, myPeerID);
    $('btn-copy-cmd').disabled = false;
}

function highlightCmd(text, pid) {
    return text
        .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
        .replace(/(curl|wget|chmod|powershell|git|go)\b/g, '<span class="cmd-kw">$1</span>')
        .replace(/(#[^\n]*)/g, '<span class="cmd-comment">$1</span>')
        .replace(/(https?:\/\/[^\s\\]+)/g, '<span class="cmd-str">$1</span>')
        .replace(new RegExp(pid, 'g'), `<span class="cmd-id">${pid}</span>`)
        .replace(/(--id)/g, '<span class="cmd-flag">$1</span>');
}

/* ═══════════════════════════════════════════════════════════
   XTERM TERMINAL
   ═══════════════════════════════════════════════════════════ */
function initTerminal() {
    if (terminal) { terminal.dispose(); }

    terminal = new Terminal({
        theme: {
            background:    '#0c0c0c',
            foreground:    '#d4d4d4',
            cursor:        '#00b894',
            cursorAccent:  '#0c0c0c',
            selectionBackground: 'rgba(0,184,148,0.25)',
            black:         '#1e1e1e',
            red:           '#f48771',
            green:         '#4ec9b0',
            yellow:        '#dcdcaa',
            blue:          '#569cd6',
            magenta:       '#c586c0',
            cyan:          '#4fc1ff',
            white:         '#d4d4d4',
            brightBlack:   '#666',
            brightRed:     '#f44747',
            brightGreen:   '#b5cea8',
            brightYellow:  '#d7ba7d',
            brightBlue:    '#9cdcfe',
            brightMagenta: '#da71d6',
            brightCyan:    '#4dc9b0',
            brightWhite:   '#e6edf3',
        },
        fontFamily: "'Cascadia Code', 'Fira Code', 'Consolas', monospace",
        fontSize:   14,
        lineHeight: 1.2,
        cursorBlink: true,
        allowProposedApi: true,
    });

    fitAddon = new FitAddon.FitAddon();
    terminal.loadAddon(fitAddon);
    terminal.open($('terminal-container'));
    fitAddon.fit();

    terminal.writeln('\x1b[1;32m[P2P Shell]\x1b[0m \x1b[90mWebRTC reverse shell — end-to-end encrypted\x1b[0m');
    terminal.writeln('\x1b[90m─────────────────────────────────────────────\x1b[0m');

    return terminal;
}

/* ═══════════════════════════════════════════════════════════
   AGENT → BROWSER CONTROL MESSAGES (prefix 0x02)
   ═══════════════════════════════════════════════════════════ */
function handleAgentControl(msg) {
    switch (msg.type) {
        case 'download-start':
            downloadMeta   = msg;
            downloadChunks = [];
            terminal.writeln(`\r\n\x1b[33m[Receiving: ${msg.name} (${msg.size} bytes)…]\x1b[0m`);
            break;

        case 'download-end': {
            const blob = new Blob(downloadChunks);
            const url  = URL.createObjectURL(blob);
            const a    = document.createElement('a');
            a.href     = url;
            a.download = downloadMeta.name;
            a.click();
            URL.revokeObjectURL(url);
            terminal.writeln(`\r\n\x1b[32m[Download complete: ${downloadMeta.name}]\x1b[0m`);
            downloadMeta   = null;
            downloadChunks = [];
            break;
        }

        case 'download-error':
            terminal.writeln(`\r\n\x1b[31m[Download error: ${msg.error}]\x1b[0m`);
            downloadMeta   = null;
            downloadChunks = [];
            break;

        case 'upload-ok':
            terminal.writeln(`\r\n\x1b[32m[Upload complete: ${msg.name} → ${msg.path}]\x1b[0m`);
            break;

        case 'upload-error':
            terminal.writeln(`\r\n\x1b[31m[Upload error: ${msg.error}]\x1b[0m`);
            break;
    }
}

/* ═══════════════════════════════════════════════════════════
   FILE TRANSFER — UPLOAD
   ═══════════════════════════════════════════════════════════ */
async function doUpload(file) {
    if (!dc || dc.readyState !== 'open') return;

    const CHUNK_SIZE = 16384;
    const MAX_BUF    = 256 * 1024; // pause when bufferedAmount exceeds this

    terminal.writeln(`\r\n\x1b[33m[Uploading: ${file.name} (${file.size} bytes)…]\x1b[0m`);

    dc.send('\x01' + JSON.stringify({ type: 'upload-start', name: file.name, size: file.size }));

    let offset = 0;
    while (offset < file.size) {
        // Back-pressure: wait for the send buffer to drain
        while (dc.bufferedAmount > MAX_BUF) {
            await new Promise(r => setTimeout(r, 20));
        }
        const chunk = await file.slice(offset, offset + CHUNK_SIZE).arrayBuffer();
        dc.send(chunk);
        offset += chunk.byteLength;
    }

    dc.send('\x01' + JSON.stringify({ type: 'upload-end' }));
}

/* ═══════════════════════════════════════════════════════════
   FILE TRANSFER — DOWNLOAD REQUEST
   ═══════════════════════════════════════════════════════════ */
function doDownload() {
    if (!dc || dc.readyState !== 'open') return;
    const path = prompt('Remote file path to download:');
    if (!path || !path.trim()) return;
    terminal.writeln(`\r\n\x1b[33m[Requesting: ${path}]\x1b[0m`);
    dc.send('\x01' + JSON.stringify({ type: 'download', path: path.trim() }));
}

/* ═══════════════════════════════════════════════════════════
   EXIT AGENT
   ═══════════════════════════════════════════════════════════ */
function doExitAgent() {
    if (!dc || dc.readyState !== 'open') return;
    if (!confirm('Terminate the remote agent process?')) return;
    dc.send('\x01' + JSON.stringify({ type: 'exit' }));
}

/* ═══════════════════════════════════════════════════════════
   SAVE SESSION LOG
   ═══════════════════════════════════════════════════════════ */
function stripAnsi(str) {
    return str
        .replace(/\x1b\[[0-9;?]*[a-zA-Z]/g, '')            // CSI sequences
        .replace(/\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)/g, '') // OSC sequences
        .replace(/\x1b[^[\]]/g, '')                          // other ESC
        .replace(/\r\n/g, '\n')
        .replace(/\r(?!\n)/g, '\n');
}

function saveLog() {
    if (!sessionLog) {
        alert('No session output to save yet.');
        return;
    }
    const text = stripAnsi(sessionLog);
    const blob = new Blob([text], { type: 'text/plain' });
    const url  = URL.createObjectURL(blob);
    const a    = document.createElement('a');
    a.href     = url;
    a.download = `p2p-shell-${new Date().toISOString().replace(/[:.]/g, '-')}.txt`;
    a.click();
    URL.revokeObjectURL(url);
}

/* ═══════════════════════════════════════════════════════════
   CONNECTION HANDLER
   ═══════════════════════════════════════════════════════════ */
function setupShellConnection(conn) {
    $('term-peer-id').textContent = conn.peer;

    conn.on('open', () => {
        dc = conn.dataChannel;
        if (!dc) {
            console.error('No dataChannel on conn');
            return;
        }
        dc.binaryType = 'arraybuffer';

        // Reset transfer/log state for new session
        sessionLog     = '';
        downloadMeta   = null;
        downloadChunks = [];

        initTerminal();
        showPanel('panel-terminal');

        // Inbound messages from agent
        dc.onmessage = evt => {
            // Binary → file download chunk
            if (evt.data instanceof ArrayBuffer) {
                if (downloadMeta) {
                    downloadChunks.push(evt.data);
                }
                return;
            }

            const text = evt.data;

            // Agent control message (prefix 0x02)
            if (text.charCodeAt(0) === 0x02) {
                try {
                    handleAgentControl(JSON.parse(text.slice(1)));
                } catch (e) {
                    console.error('Bad agent control msg:', e);
                }
                return;
            }

            // Terminal output
            sessionLog += text;
            terminal.write(text);
        };

        // Terminal input → shell
        terminal.onData(data => {
            if (dc.readyState === 'open') dc.send(data);
        });

        // Terminal resize → shell (control message: 0x01 + JSON)
        terminal.onResize(({ rows, cols }) => {
            if (dc.readyState === 'open') {
                dc.send('\x01' + JSON.stringify({ type: 'resize', rows, cols }));
            }
        });

        // Re-fit on window resize
        window.addEventListener('resize', () => fitAddon?.fit());

        // Send initial size
        const { rows, cols } = terminal;
        if (dc.readyState === 'open') {
            dc.send('\x01' + JSON.stringify({ type: 'resize', rows, cols }));
        }

        $('conn-status').innerHTML =
            '<span class="status-dot connected"></span> Connected';
    });

    conn.on('close', () => {
        dc = null;
        if (terminal) {
            terminal.writeln('\r\n\x1b[31m\r\n[Connection closed by agent]\x1b[0m');
        }
        $('conn-status').innerHTML =
            '<span class="status-dot disconnected"></span> Disconnected';
    });

    conn.on('error', err => {
        if (terminal) {
            terminal.writeln(`\r\n\x1b[31m[Error: ${err.message}]\x1b[0m`);
        }
    });
}

/* ═══════════════════════════════════════════════════════════
   PEER SETUP
   ═══════════════════════════════════════════════════════════ */
function initPeer() {
    setHeaderStatus('Connecting to signaling…', 'init');

    peer = new Peer(undefined, PEER_CONFIG);

    peer.on('open', id => {
        myPeerID = id;
        $('peer-id-display').textContent = id;
        $('btn-copy-id').disabled = false;
        $('pulse-dot').classList.remove('hidden');

        setHeaderStatus('Waiting for agent…', 'waiting');
        $('waiting-text').textContent = 'Waiting for agent to connect…';

        const active = document.querySelector('.tab.active');
        if (active) updateCmd(active.dataset.platform);
    });

    peer.on('connection', conn => {
        setHeaderStatus('Agent connected!', 'connected');
        setupShellConnection(conn);
    });

    peer.on('error', err => {
        setHeaderStatus('Error: ' + err.message, 'disconnected');
        $('waiting-text').textContent = 'Error — ' + err.message;
        setTimeout(initPeer, 5000);
    });

    peer.on('disconnected', () => {
        setHeaderStatus('Reconnecting…', 'init');
        peer.reconnect();
    });
}

/* ═══════════════════════════════════════════════════════════
   EVENTS
   ═══════════════════════════════════════════════════════════ */
function bindEvents() {
    // Platform tabs
    document.querySelectorAll('.tab').forEach(tab => {
        tab.addEventListener('click', () => {
            document.querySelectorAll('.tab').forEach(t => t.classList.remove('active'));
            tab.classList.add('active');
            updateCmd(tab.dataset.platform);
        });
    });

    // Copy peer ID
    $('btn-copy-id').addEventListener('click', () => {
        if (myPeerID) {
            navigator.clipboard.writeText(myPeerID)
                .then(() => showCopyFeedback($('btn-copy-id'), '✅'));
        }
    });

    // Copy command
    $('btn-copy-cmd').addEventListener('click', () => {
        const active = document.querySelector('.tab.active');
        if (!active || !myPeerID) return;
        const p = PLATFORMS[active.dataset.platform];
        if (p) {
            const text = p.tmpl(baseURL(), myPeerID);
            navigator.clipboard.writeText(text)
                .then(() => showCopyFeedback($('btn-copy-cmd'), '✅ Copied'));
        }
    });

    // Upload: click hidden file input
    $('btn-upload').addEventListener('click', () => $('file-input').click());
    $('file-input').addEventListener('change', evt => {
        const file = evt.target.files[0];
        if (file) doUpload(file);
        evt.target.value = ''; // reset so the same file can be re-selected
    });

    // Download from remote
    $('btn-download').addEventListener('click', doDownload);

    // Save session log
    $('btn-save-log').addEventListener('click', saveLog);

    // Exit remote agent
    $('btn-exit-agent').addEventListener('click', doExitAgent);

    // New session / reconnect
    $('btn-reconnect').addEventListener('click', () => {
        if (peer) { try { peer.destroy(); } catch {} peer = null; }
        if (terminal) { terminal.dispose(); terminal = null; }
        dc             = null;
        sessionLog     = '';
        downloadMeta   = null;
        downloadChunks = [];
        myPeerID       = null;
        $('peer-id-display').textContent = '—';
        $('btn-copy-id').disabled  = true;
        $('btn-copy-cmd').disabled = true;
        $('pulse-dot').classList.add('hidden');
        const active = document.querySelector('.tab.active');
        if (active) updateCmd(active.dataset.platform);
        showPanel('panel-waiting');
        initPeer();
    });

    // Fit terminal on window resize
    window.addEventListener('resize', () => fitAddon?.fit());
}

function showCopyFeedback(btn, text) {
    const orig = btn.textContent;
    btn.textContent = text;
    setTimeout(() => { btn.textContent = orig; }, 1500);
}

/* ═══════════════════════════════════════════════════════════
   INIT
   ═══════════════════════════════════════════════════════════ */
document.addEventListener('DOMContentLoaded', () => {
    bindEvents();
    initPeer();
});
