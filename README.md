# Tunnelway

Windows-first, account-free direct file transfer using one-time device pairing.

## Components

- [Windows desktop app](desktop/README.md): a Tauri v2 shell that will produce an NSIS `.exe` installer.
- [Pairing service](backend/pairing/README.md): short-lived in-memory pairing and WebRTC signaling. It does not receive or relay file contents.

The current client pairs devices and compares the WebRTC DTLS fingerprints before marking the direct channel verified. File selection and file transfer are the next client milestone. A STUN endpoint must be configured for reliable connections across separate networks. There is no TURN or file-relay fallback.

No transfer history or account data is stored. Pairing and signaling messages exist temporarily in process memory until the session expires or is closed. Direct WebRTC connections can expose each peer's IP address to the other peer and network services used for connectivity discovery.
