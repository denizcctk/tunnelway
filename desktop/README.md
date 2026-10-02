# Tunnelway for Windows

This is a Tauri v2 desktop shell around the bundled pairing interface in `backend/pairing/web`. The pairing service remains a separate internet-facing process; it only handles short-lived codes and WebRTC signaling.

## Development on Windows

1. Install Rust, Node.js, Microsoft C++ Build Tools, and WebView2.
2. Start the local pairing service from another PowerShell window:

   ```powershell
   cd backend/pairing
   go run ./cmd/pairing
   ```

3. Start the desktop shell:

   ```powershell
   cd desktop
   npm install
   npm run dev
   ```

Debug builds use `http://localhost:8080` as the pairing API.

## Build the installer

Set `TUNNELWAY_API_URL` to the HTTPS address where the pairing service is deployed, then build:

```powershell
$env:TUNNELWAY_API_URL = "https://your-pairing-service"
npm run build
```

The NSIS setup executable is produced under `desktop/src-tauri/target/release/bundle/nsis`. Do not distribute a release build until its API URL is configured and the service is deployed. A code-signing certificate is also needed to avoid Windows publisher warnings.

The service must allow the Tauri Windows origin `http://tauri.localhost` in `ALLOWED_ORIGINS`. Configure a STUN-only ICE endpoint with `STUN_URL` for cross-network direct connectivity. There is deliberately no TURN or file-relay fallback.
