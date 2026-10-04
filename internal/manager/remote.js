// The remote dashboard's client: the manager inlines this before the
// dashboard's own script on the remote page only (GET /r/<token>/ at the
// relay; internal/manager/remote.go), and the dashboard's api() hands
// every call to window.harnessRemote.api. internal/remoteaccess describes
// the protocol and what the relay can and cannot do; every constant here
// must match it byte for byte.
//
// The key: imported as a non-extractable WebCrypto key the moment it is
// typed, and everything derived from it is non-extractable too. "Remember
// on this device" stores that key object in IndexedDB (scoped to this
// page's address), never its bytes: a later page can use it, not read it.
// Nothing goes in localStorage.
(function () {
  "use strict";
  const base = location.pathname.replace(/[^/]*$/, ""); // "/r/<token>/"
  const enc = new TextEncoder();
  const dec = new TextDecoder();
  const SALT = enc.encode("home-harness remote v1");
  const AAD_REQUEST = "home-harness remote v1 request ";
  const AAD_RESPONSE = "home-harness remote v1 response ";
  const ALPHABET = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";
  const MAX_BODY = 8 << 20;
  const MODES = {"read-only": "view only", "standard": "view and run tasks (no admin changes)", "full": "full control"};

  // The local sign-in link carries the operator token: it must never be
  // used through the relay. Scrub it before the dashboard's script runs.
  let notice = "";
  if (/^#login=/.test(location.hash)) {
    history.replaceState(null, "", location.pathname + location.search);
    notice = "That sign-in link is for the manager's own machine and was not used here. Sign in with the remote key.";
  }

  class SignedOut extends Error {}
  class KeyRejected extends Error {}

  const hex = (buf) => Array.from(new Uint8Array(buf), (b) => b.toString(16).padStart(2, "0")).join("");
  function unhex(s) {
    if (typeof s !== "string" || !/^([0-9a-f]{2})+$/.test(s)) throw new Error("The manager's reply was malformed.");
    const out = new Uint8Array(s.length / 2);
    for (let i = 0; i < out.length; i++) out[i] = parseInt(s.substr(i * 2, 2), 16);
    return out;
  }

  // Same rules as remoteaccess.NormalizeKey.
  function normalizeKey(typed) {
    const k = String(typed).toUpperCase().replace(/[\s-]/g, "").replace(/O/g, "0").replace(/[IL]/g, "1");
    if (k.length !== 24) return "";
    for (const c of k) if (!ALPHABET.includes(c)) return "";
    return k;
  }

  // The GCM nonce: sequence number (8 bytes), then frame index (4 bytes).
  function nonce(seq, index) {
    const b = new Uint8Array(12);
    const v = new DataView(b.buffer);
    v.setUint32(0, Math.floor(seq / 4294967296));
    v.setUint32(4, seq >>> 0);
    v.setUint32(8, index);
    return b;
  }

  function derive(baseKey, info, algorithm, usages) {
    return crypto.subtle.deriveKey({name: "HKDF", hash: "SHA-256", salt: SALT, info: enc.encode(info)}, baseKey, algorithm, false, usages);
  }

  async function plainError(resp) {
    try {
      const v = await resp.json();
      if (v && v.error) return String(v.error);
    } catch (e) { /* not JSON */ }
    return "The relay answered " + resp.status + " " + resp.statusText + ".";
  }

  function post(endpoint, body) {
    return fetch(base + endpoint, {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify(body || {}),
      cache: "no-store", credentials: "omit", referrerPolicy: "no-referrer"});
  }

  // ---- sign-in: challenge, proof, the manager's proof, session keys
  let session = null;

  async function login(baseKey) {
    let resp = await post("hello");
    if (!resp.ok) throw new Error(await plainError(resp));
    const sn = String((await resp.json()).nonce || "");
    const cn = hex(crypto.getRandomValues(new Uint8Array(16)));
    const proofKey = await derive(baseKey, "login", {name: "HMAC", hash: "SHA-256", length: 256}, ["sign", "verify"]);
    const proof = hex(await crypto.subtle.sign("HMAC", proofKey, enc.encode("client " + sn + " " + cn)));
    resp = await post("login", {nonce: sn, clientNonce: cn, proof});
    if (resp.status === 403) throw new KeyRejected(await plainError(resp));
    if (!resp.ok) throw new Error(await plainError(resp));
    const r = await resp.json();
    if (!await crypto.subtle.verify("HMAC", proofKey, unhex(r.serverProof), enc.encode("server " + sn + " " + cn))) {
      throw new Error("The manager's reply could not be verified: this is not your manager.");
    }
    session = {
      id: String(r.session), mode: String(r.mode), seq: 0,
      c2s: await derive(baseKey, "c2s " + sn + " " + cn, {name: "AES-GCM", length: 256}, ["encrypt"]),
      s2c: await derive(baseKey, "s2c " + sn + " " + cn, {name: "AES-GCM", length: 256}, ["decrypt"]),
    };
  }

  // ---- one sealed call, and its sealed reply as an ordinary Response
  async function call(s, path, method, type, body, signal) {
    const seq = ++s.seq;
    const head = enc.encode(JSON.stringify({method, path, type}));
    const plain = new Uint8Array(4 + head.length + body.length);
    new DataView(plain.buffer).setUint32(0, head.length);
    plain.set(head, 4);
    plain.set(body, 4 + head.length);
    const sealed = await crypto.subtle.encrypt({name: "AES-GCM", iv: nonce(seq, 0), additionalData: enc.encode(AAD_REQUEST + s.id)}, s.c2s, plain);
    const resp = await fetch(base + "call", {method: "POST", body: sealed, cache: "no-store", credentials: "omit", referrerPolicy: "no-referrer", signal,
      headers: {"Content-Type": "application/octet-stream", "X-Harness-Session": s.id, "X-Harness-Seq": String(seq)}});
    // Anything not sealed is only ever an error: the relay can fake those.
    if (resp.status !== 200 || resp.headers.get("X-Harness-Sealed") !== "1" || !resp.body) {
      const message = await plainError(resp);
      if (resp.status === 401 || resp.status === 404) throw new SignedOut(message);
      throw new Error(message);
    }
    return openReply(resp.body, s, seq);
  }

  async function openReply(stream, s, seq) {
    const reader = stream.getReader();
    const aad = enc.encode(AAD_RESPONSE + s.id);
    let buf = new Uint8Array(0);
    let index = 0;
    let ended = false;
    async function need(n) {
      while (buf.length < n) {
        const {value, done} = await reader.read();
        if (done) throw new Error("The reply was cut short.");
        const joined = new Uint8Array(buf.length + value.length);
        joined.set(buf);
        joined.set(value, buf.length);
        buf = joined;
      }
    }
    async function frame() {
      await need(4);
      const size = new DataView(buf.buffer, buf.byteOffset, 4).getUint32(0);
      if (size > 1 << 20) throw new Error("The manager's reply was malformed.");
      await need(4 + size);
      const sealed = buf.slice(4, 4 + size);
      buf = buf.subarray(4 + size);
      let plain;
      try {
        plain = new Uint8Array(await crypto.subtle.decrypt({name: "AES-GCM", iv: nonce(seq, index), additionalData: aad}, s.s2c, sealed));
      } catch (e) {
        throw new Error("The reply could not be verified: it was changed on the way.");
      }
      index++;
      if (!plain.length || (index === 1) !== (plain[0] === 72 /* H */)) throw new Error("The manager's reply was malformed.");
      if (plain[0] === 69 /* E */) ended = true;
      return plain;
    }
    const head = JSON.parse(dec.decode((await frame()).subarray(1)));
    const status = Number(head.status);
    if (!(status >= 200 && status <= 599)) throw new Error("The manager's reply was malformed.");
    const headers = head.type ? {"Content-Type": String(head.type)} : {};
    if (status === 204 || status === 205 || status === 304) {
      while (!ended) await frame();
      return new Response(null, {status, headers});
    }
    const body = new ReadableStream({
      async pull(controller) {
        try {
          const f = await frame();
          if (f[0] === 68 /* D */) controller.enqueue(f.slice(1));
          else if (f[0] === 69 /* E */) controller.close();
          else throw new Error("The manager's reply was malformed.");
        } catch (e) {
          controller.error(e);
          reader.cancel().catch(() => {});
        }
      },
      cancel() { return reader.cancel(); },
    });
    return new Response(body, {status, headers});
  }

  // ---- what the dashboard calls instead of fetch
  let ready = null;
  let markReady = null;
  function whenSignedIn() {
    if (!ready) ready = new Promise((resolve) => { markReady = resolve; });
    return ready;
  }

  async function api(path, options) {
    const opts = options || {};
    const method = String(opts.method || "GET").toUpperCase();
    const type = new Headers(opts.headers || {}).get("Content-Type") || "";
    const body = opts.body == null ? new Uint8Array(0) : new Uint8Array(await new Response(opts.body).arrayBuffer());
    if (body.length > MAX_BODY) throw new Error("Files over 8 MiB can't be sent remotely; do that at home.");
    for (let attempt = 0; ; attempt++) {
      while (!session) await whenSignedIn();
      const s = session;
      try {
        return await call(s, path, method, type, body, opts.signal);
      } catch (e) {
        // The manager refused the session before running anything, so
        // trying again after signing in again is safe.
        if (!(e instanceof SignedOut) || attempt >= 3) throw e;
        if (session === s) signedOut(e.message);
      }
    }
  }

  function signedIn() {
    hideCard();
    showBanner();
    whenSignedIn();
    markReady();
  }

  function signedOut(message) {
    session = null;
    ready = null;
    autoSignIn(message);
  }

  let signingIn = false;
  async function autoSignIn(message) {
    if (signingIn) return;
    signingIn = true;
    try {
      const stored = await loadKey();
      if (stored) {
        try {
          await login(stored);
          signedIn();
          return;
        } catch (e) {
          if (e instanceof KeyRejected) await forgetKey();
          message = e.message;
        }
      }
      showCard(message);
    } finally {
      signingIn = false;
    }
  }

  // ---- the remembered key (IndexedDB; a CryptoKey object, not bytes)
  function keyStore(mode, op) {
    return new Promise((resolve) => {
      let open;
      try { open = indexedDB.open("home-harness-remote", 1); } catch (e) { resolve(null); return; }
      open.onupgradeneeded = () => open.result.createObjectStore("keys");
      open.onerror = () => resolve(null);
      open.onsuccess = () => {
        try {
          const tx = open.result.transaction("keys", mode);
          const req = op(tx.objectStore("keys"));
          tx.oncomplete = () => resolve(req.result || null);
          tx.onerror = tx.onabort = () => resolve(null);
        } catch (e) { resolve(null); }
      };
    });
  }
  const loadKey = () => keyStore("readonly", (st) => st.get(base));
  const saveKey = (key) => keyStore("readwrite", (st) => st.put(key, base));
  const forgetKey = () => keyStore("readwrite", (st) => st.delete(base));

  // ---- the sign-in card and the "remote" banner (built with textContent)
  let card = null;
  let banner = null;
  function make(tag, props, children) {
    const e = Object.assign(document.createElement(tag), props || {});
    for (const c of children || []) e.append(c);
    return e;
  }

  function showCard(message) {
    if (!card) {
      const input = make("input", {type: "password", id: "remoteKey", autocomplete: "current-password", spellcheck: false,
        placeholder: "XXXX-XXXX-XXXX-XXXX-XXXX-XXXX"});
      const remember = make("input", {type: "checkbox", id: "remoteRemember", checked: true});
      const button = make("button", {id: "remoteGo", textContent: "Sign in"});
      card = make("div", {className: "card signin", id: "remoteSignin"}, [
        make("strong", {textContent: "Remote sign-in"}),
        make("p", {className: "hint", textContent: "You are reaching your manager through your relay. Enter its remote key: "
          + "on the manager's machine run \"harnessctl remote key\", or open \"Remote access\" on its own dashboard."}),
        input,
        make("label", {}, [remember, " Remember on this device (stored so that even this page can't read it back)"]),
        button,
        make("p", {className: "error", id: "remoteError"}),
        make("p", {className: "hint", textContent: "The relay can't read or change what you see and do here, or reuse your sign-in. "
          + "If it were compromised it could stop this page working, or change this page's code to capture the key as you type it. "
          + "If you ever doubt the relay, make a new key at home (harnessctl remote rotate)."}),
      ]);
      button.addEventListener("click", submitKey);
      input.addEventListener("keydown", (event) => { if (event.key === "Enter") submitKey(); });
      document.querySelector("h1").after(card);
    }
    document.getElementById("remoteError").textContent = message || "";
    card.hidden = false;
    if (banner) banner.hidden = true;
  }

  function hideCard() {
    if (card) card.hidden = true;
  }

  function showBanner() {
    if (!banner) {
      const forget = make("a", {href: "#", textContent: "Sign out and forget this device"});
      forget.addEventListener("click", (event) => { event.preventDefault(); signOut(); });
      banner = make("p", {className: "card", id: "remoteBanner"}, [make("strong", {id: "remoteMode"}), " ", forget]);
      document.querySelector("h1").after(banner);
    }
    document.getElementById("remoteMode").textContent = "Remote, through your relay: " + (MODES[session.mode] || session.mode) + ".";
    banner.hidden = false;
  }

  async function submitKey() {
    const input = document.getElementById("remoteKey");
    const error = document.getElementById("remoteError");
    const button = document.getElementById("remoteGo");
    error.textContent = "";
    const typed = input.value.trim();
    let baseKey;
    if (typed) {
      const k = normalizeKey(typed);
      if (!k) { error.textContent = "A remote key is 24 letters and digits, like XXXX-XXXX-XXXX-XXXX-XXXX-XXXX."; return; }
      baseKey = await crypto.subtle.importKey("raw", enc.encode(k), "HKDF", false, ["deriveKey"]);
      input.value = "";
    } else {
      baseKey = await loadKey();
      if (!baseKey) { error.textContent = "Enter the remote key."; return; }
    }
    button.disabled = true;
    try {
      await login(baseKey);
      if (typed) {
        if (document.getElementById("remoteRemember").checked) await saveKey(baseKey); else await forgetKey();
      }
      signedIn();
    } catch (e) {
      if (e instanceof KeyRejected && !typed) await forgetKey();
      error.textContent = e.message;
    } finally {
      button.disabled = false;
    }
  }

  async function signOut() {
    await forgetKey();
    const s = session;
    session = null;
    if (s) {
      try { await call(s, "/remote-session/logout", "POST", "", new Uint8Array(0)); } catch (e) { /* leaving anyway */ }
    }
    location.reload();
  }

  // The dashboard's own "Sign out of this browser" means the same here.
  document.addEventListener("click", (event) => {
    if (!event.target.closest || !event.target.closest("#signout")) return;
    event.preventDefault();
    event.stopImmediatePropagation();
    signOut();
  }, true);

  window.harnessRemote = {api, mode: () => (session ? session.mode : "")};
  if (!window.crypto || !crypto.subtle || !window.indexedDB || !window.ReadableStream) {
    showCard("This browser can't sign in remotely: it lacks Web Crypto or streams.");
    return;
  }
  autoSignIn(notice);
})();
