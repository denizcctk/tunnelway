# Tunnelway pairing service

Small, in-memory rendezvous and signaling service for the Windows client. It does not accept, store, or relay file contents.

## Run locally

```powershell
cd backend/pairing
go run ./cmd/pairing
```

The service listens on `:8080`. Set `ADDR` to change the listen address. `GET /healthz` returns a minimal health response, and the bundled Windows interface is served at `/`. Set `TUNNELWAY_API_URL` at desktop build time to the deployed service URL; development builds default to `http://localhost:8080`.

Set `STUN_URL` to a STUN server URL for cross-network connectivity. Set `ALLOWED_ORIGINS` to a comma-separated exact origin list when deploying; the local defaults include the Windows Tauri origin and localhost development origin.

## Protocol sketch

- `POST /v1/pairings` creates a short-lived pairing code and an opaque session token for the creator.
- `POST /v1/pairings/join` consumes the code once and returns a separate token for the second device.
- Both clients use authenticated `POST /v1/pairings/{session_id}/messages` and long-poll `GET /v1/pairings/{session_id}/events?after={sequence}` to exchange WebRTC offer, answer, ICE candidate, and verification messages.
- `DELETE /v1/pairings/{session_id}` destroys the in-memory session.

The service forwards signaling payloads without interpreting them. The clients must authenticate the peer and complete their safety-number/word check before transferring any file. The signaling service is not a source of trust for the key exchange.

## Initial limits

Pairing codes expire after 2 minutes; sessions expire after 5 minutes. Codes are single-use. Tokens are random bearer secrets and only their SHA-256 hashes are kept in memory. Request bodies and signaling queues are size-limited. There is no database, analytics, or request logging. Rate-limit counters are temporary process memory keyed by the immediate network peer.

Run behind HTTPS before exposing it to the internet. If deployed behind a proxy, configure the proxy so the service can rate-limit by the actual client address; this first version intentionally does not trust arbitrary forwarding headers.

Direct peer connectivity is not guaranteed on every network. This service has no TURN or file-relay path: if ICE cannot establish a direct connection, the client must report that the network could not connect. ICE candidates can reveal network addresses to the other peer and the signaling operator; do not describe direct mode as IP-anonymous.

This is an initial protocol scaffold, not a production security review. The desktop client, authenticated key exchange, replay handling, deployment controls, and abuse protections still need implementation and review.
