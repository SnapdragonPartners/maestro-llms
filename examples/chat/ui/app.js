// app.js — single-file SPA logic for the chat demo. No framework deps.
//
// Flow:
//   - On load, fetch /api/providers to populate the model dropdown.
//   - Keep an in-memory `history` array; each message is {role, text}.
//   - On Send: append a user message, POST /api/chat with the full
//     history, append the assistant reply (or error) on response.
//
// State is intentionally in-memory: reloading the page resets the
// conversation. A real app would persist this; the demo's point is the
// wiring, not the persistence story.

(function () {
  const $ = (id) => document.getElementById(id);
  const modelSelect = $("model");
  const log = $("log");
  const input = $("input");
  const sendBtn = $("send");
  const resetBtn = $("reset");
  const status = $("status");
  const estimate = $("estimate");

  /** @type {{role: "user"|"assistant", text: string}[]} */
  let history = [];

  // --- bootstrap ----------------------------------------------------

  async function loadProviders() {
    try {
      const res = await fetch("/api/providers");
      if (!res.ok) throw new Error("HTTP " + res.status);
      const options = await res.json();
      if (!options.length) {
        status.textContent = "no providers detected — set credentials in env and restart";
        return;
      }
      modelSelect.innerHTML = "";
      for (const opt of options) {
        const el = document.createElement("option");
        el.value = opt.id;
        el.textContent = opt.label;
        modelSelect.appendChild(el);
      }
      status.textContent = `ready — ${options.length} model${options.length > 1 ? "s" : ""}`;
    } catch (err) {
      status.textContent = "failed to load providers: " + err.message;
    }
  }

  // --- rendering ----------------------------------------------------

  function appendMessage(role, text, meta) {
    const wrap = document.createElement("div");
    wrap.className = "msg " + role;
    wrap.textContent = text;
    if (meta) {
      const m = document.createElement("span");
      m.className = "meta";
      m.textContent = meta;
      wrap.appendChild(m);
    }
    log.appendChild(wrap);
    log.scrollTop = log.scrollHeight;
  }

  function appendError(text) {
    appendMessage("error", text);
  }

  // --- send ---------------------------------------------------------

  async function send() {
    const text = input.value.trim();
    if (!text) return;
    const modelId = modelSelect.value;
    if (!modelId) {
      appendError("no model selected");
      return;
    }
    history.push({ role: "user", text });
    appendMessage("user", text);
    input.value = "";
    updateEstimate();
    sendBtn.disabled = true;
    sendBtn.textContent = "Thinking…";

    try {
      const res = await fetch("/api/chat", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ modelId, history }),
      });
      const data = await res.json();
      if (!res.ok || data.error) {
        appendError(data.error || ("HTTP " + res.status));
        // Roll back the optimistic history entry on failure so a retry
        // doesn't duplicate the user message in subsequent requests.
        history.pop();
        return;
      }
      history.push({ role: "assistant", text: data.text });
      const meta = `${data.stopReason} · ${data.inputTokens}/${data.outputTokens} tokens · ${data.latencyMs} ms`;
      appendMessage("assistant", data.text || "(empty response)", meta);
    } catch (err) {
      appendError("network error: " + err.message);
      history.pop();
    } finally {
      sendBtn.disabled = false;
      sendBtn.textContent = "Send";
      input.focus();
    }
  }

  // --- token estimate (client-side, char-based) ---------------------
  // Approximates the server's llms.EstimateTextTokens — rune-counted,
  // neutral bias (~4 chars/token), ceiling-divided. See ADR-0013.
  // Not exact (JavaScript counts UTF-16 code units, not Go runes), but
  // close enough for a UI hint.
  function approxTokens(s) {
    if (!s) return 0;
    const runes = [...s].length;
    return Math.ceil(runes / 4);
  }
  function updateEstimate() {
    const n = approxTokens(input.value);
    estimate.textContent = `~${n} token${n === 1 ? "" : "s"}`;
  }

  // --- events -------------------------------------------------------

  input.addEventListener("input", updateEstimate);
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      send();
    }
  });
  sendBtn.addEventListener("click", send);
  resetBtn.addEventListener("click", () => {
    history = [];
    log.innerHTML = "";
    input.focus();
  });

  loadProviders().then(() => input.focus());
  updateEstimate();
})();
