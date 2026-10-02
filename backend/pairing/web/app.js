(() => {
  const ui = document.getElementById("tunnelway-ui");
  if (!ui) return;

  const codeNode = ui.querySelector("#pair-code");
  const copyButton = ui.querySelector("#copy-code");
  const codeInput = ui.querySelector("#peer-code");
  const connectButton = ui.querySelector("#connect");
  const statusBox = ui.querySelector("#pair-status");
  const statusMessage = ui.querySelector("#pair-message");
  const fingerprintBox = ui.querySelector("#fingerprint-box");
  const fingerprintNode = ui.querySelector("#fingerprint-code");
  const approveButton = ui.querySelector("#approve-pair");
  const networkNote = ui.querySelector("#network-note");

  connectButton.disabled = true;
  copyButton.disabled = true;

  const state = {
    api: "",
    code: "",
    sessionId: "",
    token: "",
    role: "",
    cursor: 0,
    pollStopped: false,
    pollController: null,
    pc: null,
    channel: null,
    pendingCandidates: [],
    fingerprint: "",
    approvedHere: false,
    approvedThere: false,
    startingOffer: false,
    handlingOffer: false,
  };

  function showStatus(message) {
    statusBox.style.display = "block";
    statusMessage.textContent = message;
  }

  function formatCode(value) {
    return value.replace(/\D/g, "").replace(/(\d{3})(?=\d)/g, "$1 · ");
  }

  async function getApiBase() {
    const invoke = window.__TAURI__?.core?.invoke;
    if (typeof invoke === "function") {
      return (await invoke("pairing_api_base")).replace(/\/$/, "");
    }
    return location.origin.replace(/\/$/, "");
  }

  async function api(path, options = {}) {
    const headers = new Headers(options.headers || {});
    if (state.token) headers.set("Authorization", "Bearer " + state.token);
    if (options.body) headers.set("Content-Type", "application/json");
    const response = await fetch(state.api + path, {
      ...options,
      headers,
      cache: "no-store",
      credentials: "omit",
    });
    if (response.status === 204 || response.status === 202) return null;
    const payload = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(payload.error || "service_unavailable");
    return payload;
  }

  function cleanCode(value) {
    return value.replace(/\D/g, "");
  }

  function stopPolling() {
    state.pollStopped = true;
    state.pollController?.abort();
    state.pollController = null;
  }

  async function endSession() {
    stopPolling();
    state.pc?.close();
    state.pc = null;
    state.channel = null;
    if (state.sessionId && state.token) {
      try {
        await api("/v1/pairings/" + encodeURIComponent(state.sessionId), { method: "DELETE" });
      } catch {
        // The service expires abandoned sessions automatically.
      }
    }
    state.sessionId = "";
    state.token = "";
    state.code = "";
    state.role = "";
  }

  async function initializeHost() {
    try {
      state.api = await getApiBase();
      if (!state.api) throw new Error("service_not_configured");
      const config = await api("/v1/config");
      if (!config.stun_url) {
        networkNote.hidden = false;
        networkNote.textContent = "STUN yapılandırılmadı. Farklı ağlardaki cihazlar doğrudan bağlanamayabilir.";
      }
      const created = await api("/v1/pairings", { method: "POST" });
      state.sessionId = created.session_id;
      state.token = created.token;
      state.code = created.code;
      state.role = "host";
      state.cursor = 0;
      state.pollStopped = false;
      codeNode.textContent = formatCode(state.code);
      copyButton.disabled = false;
      showStatus("Tek kullanımlık kod hazır. Diğer cihazda bu kodu gir.");
      pollEvents();
    } catch (error) {
      codeNode.textContent = "— — —";
      showStatus(error.message === "service_not_configured"
        ? "Eşleştirme servisi bu uygulama derlemesi için yapılandırılmadı."
        : "Eşleştirme servisine ulaşılamadı. Bağlantı ayarını kontrol et.");
    } finally {
      connectButton.disabled = false;
    }
  }

  async function connectToCode() {
    const code = cleanCode(codeInput.value);
    if (code.length !== 9) {
      codeInput.setCustomValidity("9 haneli eşleştirme kodunu gir.");
      codeInput.reportValidity();
      return;
    }
    if (code === state.code) {
      codeInput.setCustomValidity("Bu cihazın kendi kodunu giremezsin.");
      codeInput.reportValidity();
      return;
    }

    connectButton.disabled = true;
    showStatus("Kod kontrol ediliyor…");
    try {
      state.api ||= await getApiBase();
      if (!state.api) throw new Error("service_not_configured");
      const joined = await api("/v1/pairings/join", {
        method: "POST",
        body: JSON.stringify({ code }),
      });
      await endSession();
      state.sessionId = joined.session_id;
      state.token = joined.token;
      state.role = "peer";
      state.cursor = 0;
      state.pollStopped = false;
      codeNode.textContent = "— — —";
      codeInput.value = "";
      await createPeerConnection();
      showStatus("Cihaz bulundu. Doğrudan bağlantı kuruluyor…");
      pollEvents();
    } catch {
      showStatus("Kod geçersiz, süresi dolmuş veya bağlantı kurulamadı.");
    } finally {
      connectButton.disabled = false;
    }
  }

  async function createPeerConnection() {
    const config = await api("/v1/config");
    const iceServers = config.stun_url ? [{ urls: config.stun_url }] : [];
    const pc = new RTCPeerConnection({ iceServers });
    state.pc = pc;

    pc.onicecandidate = (event) => {
      if (event.candidate) sendSignal("candidate", event.candidate.toJSON());
    };
    pc.onconnectionstatechange = () => {
      if (pc.connectionState === "connected") {
        showStatus("Doğrudan cihaz bağlantısı kuruldu. İki ekrandaki doğrulama kodunu karşılaştır.");
        updateFingerprint();
      } else if (pc.connectionState === "failed") {
        showStatus("Doğrudan bağlantı kurulamadı. Bu ağ çifti ilk sürümde desteklenmiyor.");
      } else if (pc.connectionState === "disconnected") {
        showStatus("Cihaz bağlantısı kesildi.");
      }
    };

    if (state.role === "host") {
      state.channel = pc.createDataChannel("tunnelway-control");
      attachChannel(state.channel);
    } else {
      pc.ondatachannel = (event) => {
        state.channel = event.channel;
        attachChannel(state.channel);
      };
    }
  }

  function attachChannel(channel) {
    channel.onopen = () => updateFingerprint();
    channel.onmessage = (event) => {
      let message;
      try {
        message = JSON.parse(event.data);
      } catch {
        return;
      }
      if (message.type === "approved") {
        state.approvedThere = true;
        updateApprovalState();
      }
    };
    channel.onerror = () => showStatus("Doğrudan cihaz bağlantısında hata oluştu.");
  }

  async function beginOffer() {
    if (state.startingOffer || state.pc) return;
    state.startingOffer = true;
    try {
      await createPeerConnection();
      const offer = await state.pc.createOffer();
      await state.pc.setLocalDescription(offer);
      await sendSignal("offer", state.pc.localDescription);
    } catch {
      showStatus("Bağlantı başlatılamadı. Yeniden dene.");
    }
  }

  async function handleSignal(message) {
    if (message.kind === "peer_joined" && state.role === "host") {
      await beginOffer();
      return;
    }
    if (message.kind === "candidate") {
      if (state.pc?.remoteDescription) {
        await state.pc.addIceCandidate(message.data).catch(() => {});
      } else {
        state.pendingCandidates.push(message.data);
      }
      return;
    }
    if (message.kind === "offer" && state.role === "peer" && !state.handlingOffer) {
      state.handlingOffer = true;
      try {
        await state.pc.setRemoteDescription(message.data);
        await drainCandidates();
        const answer = await state.pc.createAnswer();
        await state.pc.setLocalDescription(answer);
        await sendSignal("answer", state.pc.localDescription);
      } catch {
        showStatus("Cihaz eşleşemedi. Yeni bir kodla tekrar dene.");
      }
      return;
    }
    if (message.kind === "answer" && state.role === "host") {
      try {
        await state.pc.setRemoteDescription(message.data);
        await drainCandidates();
      } catch {
        showStatus("Cihaz eşleşemedi. Yeni bir kodla tekrar dene.");
      }
    }
  }

  async function drainCandidates() {
    const pending = state.pendingCandidates.splice(0);
    for (const candidate of pending) {
      await state.pc.addIceCandidate(candidate).catch(() => {});
    }
  }

  async function sendSignal(kind, data) {
    if (!state.sessionId || !state.token) return;
    try {
      await api("/v1/pairings/" + encodeURIComponent(state.sessionId) + "/messages", {
        method: "POST",
        body: JSON.stringify({ kind, data }),
      });
    } catch {
      if (!state.pollStopped) showStatus("Eşleştirme bağlantısı kesildi.");
    }
  }

  async function pollEvents() {
    while (!state.pollStopped && state.sessionId) {
      state.pollController = new AbortController();
      try {
        const result = await api(
          "/v1/pairings/" + encodeURIComponent(state.sessionId) + "/events?after=" + state.cursor,
          { signal: state.pollController.signal },
        );
        for (const event of result.events || []) {
          state.cursor = Math.max(state.cursor, event.id);
          await handleSignal(event);
        }
      } catch (error) {
        if (error.name === "AbortError" || state.pollStopped) return;
        showStatus("Eşleştirme servisiyle bağlantı kesildi.");
        await new Promise((resolve) => setTimeout(resolve, 1200));
      }
    }
  }

  function getFingerprints(sdp) {
    const matches = [...sdp.matchAll(/^a=fingerprint:sha-256\s+([0-9A-F:]+)$/gim)];
    return matches.map((match) => match[1].replace(/:/g, "").toLowerCase());
  }

  async function updateFingerprint() {
    const pc = state.pc;
    if (!pc || !state.channel || state.channel.readyState !== "open" ||
        !pc.localDescription?.sdp || !pc.remoteDescription?.sdp || state.fingerprint) return;
    const fingerprints = [...new Set([
      ...getFingerprints(pc.localDescription.sdp),
      ...getFingerprints(pc.remoteDescription.sdp),
    ])].sort();
    if (fingerprints.length < 2 || !crypto.subtle) {
      showStatus("Doğrulama kodu üretilemedi. Bağlantıyı kapat ve tekrar dene.");
      return;
    }
    const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(fingerprints.join("|")));
    const short = [...new Uint8Array(digest).slice(0, 6)]
      .map((byte) => byte.toString(16).padStart(2, "0")).join("").toUpperCase();
    state.fingerprint = short.match(/.{1,4}/g).join(" · ");
    fingerprintNode.textContent = state.fingerprint;
    fingerprintBox.hidden = false;
    showStatus("Doğrudan bağlantı kuruldu. İki cihazdaki doğrulama kodları eşleşmeli.");
  }

  function updateApprovalState() {
    if (state.approvedHere && state.approvedThere) {
      showStatus("Cihazlar doğrulandı ve doğrudan bağlandı. Dosya aktarımı sonraki aşamada eklenecek.");
    } else if (state.approvedHere) {
      showStatus("Sen onayladın. Diğer cihazın doğrulamasını bekliyor.");
    }
  }

  copyButton.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(state.code);
      copyButton.textContent = "Kopyalandı";
      setTimeout(() => { copyButton.textContent = "Kodu kopyala"; }, 1400);
    } catch {
      copyButton.textContent = "Kodu seçip kopyala";
    }
  });

  codeInput.addEventListener("input", () => {
    codeInput.setCustomValidity("");
    const digits = cleanCode(codeInput.value).slice(0, 9);
    codeInput.value = formatCode(digits);
  });
  connectButton.addEventListener("click", connectToCode);
  codeInput.addEventListener("keydown", (event) => {
    if (event.key === "Enter") connectToCode();
  });
  approveButton.addEventListener("click", () => {
    if (!state.channel || state.channel.readyState !== "open" || !state.fingerprint) return;
    state.approvedHere = true;
    state.channel.send(JSON.stringify({ type: "approved" }));
    approveButton.disabled = true;
    updateApprovalState();
  });

  window.addEventListener("pagehide", () => {
    stopPolling();
    if (state.sessionId && state.token) {
      fetch(state.api + "/v1/pairings/" + encodeURIComponent(state.sessionId), {
        method: "DELETE",
        headers: { Authorization: "Bearer " + state.token },
        keepalive: true,
        credentials: "omit",
      }).catch(() => {});
    }
    state.pc?.close();
  });

  initializeHost();
})();
