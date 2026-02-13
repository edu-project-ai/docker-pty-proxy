# Frontend Integration Guide — docker-pty-proxy

## WebSocket URL

```
ws://localhost:8080/attach?id={containerIdOrName}
```

## xterm.js + AttachAddon (Recommended)

```typescript
import { Terminal } from "xterm";
import { AttachAddon } from "@xterm/addon-attach";
import { FitAddon } from "@xterm/addon-fit";

const term = new Terminal({ cursorBlink: true });
const fitAddon = new FitAddon();
term.loadAddon(fitAddon);
term.open(document.getElementById("terminal")!);
fitAddon.fit();

const ws = new WebSocket(`ws://localhost:8080/attach?id=${containerId}`);

// Attach bidirectional stream
const attachAddon = new AttachAddon(ws);
term.loadAddon(attachAddon);

// Send resize on terminal fit
term.onResize(({ cols, rows }) => {
  // Option A: inline JSON message (handled by proxy automatically)
  ws.send(JSON.stringify({ type: "resize", cols, rows }));

  // Option B: HTTP endpoint
  // fetch(`http://localhost:8080/resize?id=${containerId}&w=${cols}&h=${rows}`, { method: "POST" });
});

// Initial resize
fitAddon.fit();
```

## Resize

Two options — pick one:

| Method | Endpoint | Example |
|--------|----------|---------|
| **WebSocket (inline)** | Same WS connection | `ws.send(JSON.stringify({ type: "resize", cols: 120, rows: 40 }))` |
| **HTTP POST** | `/resize?id={id}&w={cols}&h={rows}` | `POST http://localhost:8080/resize?id=abc123&w=120&h=40` |

## Health Check

```
GET http://localhost:8080/healthz
→ 200 { "status": "ok", "docker_api": "1.45" }
```
