// ansibled web UI. No dependencies; every page also works without it.
(function () {
  "use strict";
  document.documentElement.classList.add("js");

  var store = {
    get: function (k) { try { return window.localStorage.getItem(k); } catch (e) { return null; } },
    set: function (k, v) { try { window.localStorage.setItem(k, v); } catch (e) { /* ignore */ } }
  };

  // ---------------------------------------------------------------- times
  function fmtDuration(ms) {
    var s = Math.max(0, Math.floor(ms / 1000));
    var h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), r = s % 60;
    if (h) return h + "h" + (m < 10 ? "0" : "") + m + "m";
    if (m) return m + "m" + (r < 10 ? "0" : "") + r + "s";
    return r + "s";
  }
  function fmtRelative(t) {
    var d = (Date.now() - t.getTime()) / 1000;
    if (d < 0) d = 0;
    if (d < 45) return "just now";
    if (d < 3600) return Math.round(d / 60) + " min ago";
    if (d < 86400) return Math.round(d / 3600) + " h ago";
    if (d < 7 * 86400) return Math.round(d / 86400) + " d ago";
    return t.toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
  }
  function updateTimes(root) {
    (root || document).querySelectorAll("time[data-rel]").forEach(function (el) {
      var t = new Date(el.getAttribute("datetime"));
      if (isNaN(t)) return;
      el.textContent = fmtRelative(t);
      if (!el.title) el.title = t.toLocaleString();
    });
  }
  function updateElapsed(root) {
    (root || document).querySelectorAll("[data-elapsed]").forEach(function (el) {
      var t = new Date(el.getAttribute("data-elapsed"));
      if (!isNaN(t)) el.textContent = fmtDuration(Date.now() - t.getTime());
    });
  }
  function tick() { updateElapsed(); }
  setInterval(tick, 1000);
  setInterval(function () { updateTimes(); }, 30000);

  // ---------------------------------------------------------- small bits
  function wireRows(root) {
    (root || document).querySelectorAll("tr[data-href]").forEach(function (tr) {
      tr.addEventListener("click", function (e) {
        if (e.target.closest("a, button, input, select")) return;
        if (e.metaKey || e.ctrlKey) { window.open(tr.getAttribute("data-href"), "_blank"); return; }
        window.location.href = tr.getAttribute("data-href");
      });
    });
  }

  function wireForms() {
    document.querySelectorAll("form[data-autosubmit] select").forEach(function (sel) {
      sel.addEventListener("change", function () { sel.form.submit(); });
    });
    document.querySelectorAll("form[data-confirm]").forEach(function (form) {
      var btn = form.querySelector("button");
      var label = btn.querySelector("span");
      var original = label.textContent, timer = null;
      form.addEventListener("submit", function (e) {
        if (btn.classList.contains("confirm")) return;
        e.preventDefault();
        btn.classList.add("confirm");
        label.textContent = "Confirm " + original.toLowerCase();
        clearTimeout(timer);
        timer = setTimeout(function () { btn.classList.remove("confirm"); label.textContent = original; }, 4000);
      });
    });
  }

  // Refresh the live part of a list page in place.
  function wireLive() {
    var live = document.getElementById("live");
    if (!live) return;
    var every = parseInt(live.getAttribute("data-live"), 10);
    if (!every) return;
    var busy = false;
    setInterval(function () {
      if (busy || document.hidden) return;
      busy = true;
      var u = new URL(window.location.href);
      u.searchParams.set("partial", "1");
      fetch(u.toString(), { credentials: "same-origin", headers: { "Accept": "text/html" } })
        .then(function (r) {
          if (r.redirected || !r.ok) throw new Error("refresh: " + r.status);
          return r.text();
        })
        .then(function (html) {
          live.innerHTML = html;
          updateTimes(live); updateElapsed(live); wireRows(live);
        })
        .catch(function () { /* keep the old content; try again later */ })
        .then(function () { busy = false; });
    }, every);
  }

  // ------------------------------------------------------------ run page
  function wireRun() {
    var run = document.getElementById("run");
    if (!run) return;
    var tabs = Array.prototype.slice.call(document.querySelectorAll(".tabs [data-tab]"));
    var panels = {};
    document.querySelectorAll("[data-panel]").forEach(function (p) { panels[p.getAttribute("data-panel")] = p; });
    var con = null;

    function show(name, push) {
      if (!panels[name]) name = "summary";
      tabs.forEach(function (t) { t.classList.toggle("on", t.getAttribute("data-tab") === name); });
      Object.keys(panels).forEach(function (k) { panels[k].classList.toggle("on", k === name); });
      if (push) history.replaceState(null, "", "#" + name);
      if (name === "log" && con) con.shown();
    }
    function fromHash() {
      var h = decodeURIComponent(window.location.hash.slice(1));
      if (h.indexOf("h-") === 0) {
        show("changes", false);
        var card = document.getElementById(h);
        if (card) {
          card.open = true;
          card.classList.add("flash");
          setTimeout(function () { card.classList.remove("flash"); }, 1600);
          card.scrollIntoView({ block: "start" });
        }
        return true;
      }
      if (panels[h]) { show(h, false); return true; }
      return false;
    }
    tabs.forEach(function (t) {
      t.addEventListener("click", function (e) { e.preventDefault(); show(t.getAttribute("data-tab"), true); });
    });
    window.addEventListener("hashchange", fromHash);

    document.querySelectorAll("[data-expand]").forEach(function (b) {
      b.addEventListener("click", function () {
        var open = b.getAttribute("data-expand") === "1";
        panels.changes.querySelectorAll("details.hostcard").forEach(function (d) { d.open = open; });
      });
    });

    var activeAtLoad = run.getAttribute("data-active") === "1";
    var el = document.getElementById("console");
    if (el) con = new Console(el, {
      onState: function (st) { applyState(st); },
      onDone: function (st) {
        if (activeAtLoad) setTimeout(function () { window.location.reload(); }, 900);
      }
    });
    if (!fromHash()) show(activeAtLoad ? "log" : "summary", activeAtLoad);

    var badges = document.getElementById("badges");
    function applyState(st) {
      if (!st || !st.state) return;
      if (st.state !== run.getAttribute("data-state")) {
        run.setAttribute("data-state", st.state);
        var b = badges && badges.content.querySelector('[data-state="' + st.state + '"]');
        if (b) document.getElementById("run-badge").innerHTML = b.innerHTML;
      }
      var desc = document.getElementById("run-desc");
      if (desc && st.description) desc.textContent = st.description;
      var dur = document.getElementById("run-duration");
      if (dur) {
        if (st.finished && st.duration) dur.textContent = st.duration;
        else if (st.started && !dur.querySelector("[data-elapsed]")) {
          dur.innerHTML = "";
          var span = document.createElement("span");
          span.setAttribute("data-elapsed", st.started);
          dur.appendChild(span);
          updateElapsed(dur);
        }
      }
    }
  }

  // ------------------------------------------------------------- console
  var SGR_FG = { 30: 1, 31: 1, 32: 1, 33: 1, 34: 1, 35: 1, 36: 1, 37: 1, 90: 1, 91: 1, 92: 1, 93: 1, 94: 1, 95: 1, 96: 1, 97: 1 };
  var ESC = /\x1b\[([0-9;?]*)([ -\/]*)([@-~])/g;
  var HEAD = /^==> \[([^\]]+)\] (.*?)(?: \((\d{4}-\d\d-\d\dT[^)]*)\))?$/;
  var FOOT = /^<== \[([^\]]+)\] (.*)$/;
  var STRIP = /\x1b\[[0-9;?]*[ -\/]*[@-~]/g;

  // ansi appends the text of line to el as spans with colour classes; st is
  // the SGR state carried from line to line.
  function ansi(el, line, st) {
    var last = 0, m;
    function emit(text) {
      if (!text) return;
      var cls = [];
      if (st.fg) cls.push("a" + st.fg);
      if (st.b) cls.push("a-b");
      if (st.d) cls.push("a-d");
      if (st.i) cls.push("a-i");
      if (st.u) cls.push("a-u");
      if (!cls.length) { el.appendChild(document.createTextNode(text)); return; }
      var s = document.createElement("span");
      s.className = cls.join(" ");
      s.textContent = text;
      el.appendChild(s);
    }
    ESC.lastIndex = 0;
    while ((m = ESC.exec(line)) !== null) {
      emit(line.slice(last, m.index));
      last = ESC.lastIndex;
      if (m[3] !== "m") continue;
      var codes = m[1] === "" ? [0] : m[1].split(";").map(Number);
      for (var i = 0; i < codes.length; i++) {
        var c = codes[i];
        if (c === 0) { st.fg = 0; st.b = st.d = st.i = st.u = false; }
        else if (c === 1) st.b = true;
        else if (c === 2) st.d = true;
        else if (c === 3) st.i = true;
        else if (c === 4) st.u = true;
        else if (c === 22) st.b = st.d = false;
        else if (c === 23) st.i = false;
        else if (c === 24) st.u = false;
        else if (c === 39) st.fg = 0;
        else if (SGR_FG[c]) st.fg = c;
        else if ((c === 38 || c === 48) && codes[i + 1] === 5) {
          var n = codes[i + 2];
          if (c === 38 && n < 8) st.fg = 30 + n;
          else if (c === 38 && n < 16) st.fg = 90 + n - 8;
          i += 2;
        } else if ((c === 38 || c === 48) && codes[i + 1] === 2) i += 4;
      }
    }
    emit(line.slice(last));
  }

  function Console(root, hooks) {
    var self = this;
    var body = root.querySelector("#console-body");
    var followBtn = root.querySelector("#follow");
    var followLabel = followBtn.querySelector("span");
    var status = root.querySelector("#console-status");
    var search = root.querySelector("#log-search");
    var hitsLabel = root.querySelector("#log-hits");
    var lines = [], els = [], secOf = [];
    var partialLine = "", sgr = { fg: 0 };
    var sec = null, sections = [];
    var follow = true, pinTop = 0, unseen = 0;
    var query = "", hits = [], cur = -1;
    var done = false;

    if (store.get("ansibled.wrap") === "1") setWrap(true);

    // -------- layout
    function fit() {
      if (!body.offsetParent) return;
      var top = body.getBoundingClientRect().top;
      body.style.height = Math.max(320, window.innerHeight - top - 20) + "px";
    }
    window.addEventListener("resize", fit);
    this.shown = function () { fit(); if (follow) toBottom(); };

    // -------- following
    function atBottom() { return body.scrollHeight - body.scrollTop - body.clientHeight < 24; }
    function toBottom() { body.scrollTop = body.scrollHeight; pinTop = body.scrollTop; }
    function setFollow(on) {
      follow = on;
      if (on) { unseen = 0; toBottom(); }
      followBtn.hidden = on;
      followLabel.textContent = unseen ? "Follow (" + unseen + " new)" : "Follow";
    }
    body.addEventListener("scroll", function () {
      if (atBottom()) { if (!follow) setFollow(true); return; }
      if (body.scrollTop + 4 < pinTop && follow) setFollow(false);
    });
    body.addEventListener("wheel", function (e) { if (e.deltaY < 0 && follow) setFollow(false); }, { passive: true });
    body.addEventListener("keydown", function (e) {
      if ((e.key === "PageUp" || e.key === "ArrowUp" || e.key === "Home") && follow) setFollow(false);
      if (e.key === "End") setFollow(true);
    });
    followBtn.addEventListener("click", function () { setFollow(true); });

    // -------- sections
    function newSection(name, cmd) {
      var s = document.createElement("div");
      s.className = "sec open live";
      var head = document.createElement("div");
      head.className = "sec-head";
      head.innerHTML = '<svg class="i i-chevron" viewBox="0 0 16 16" width="16" height="16" aria-hidden="true"><path d="M6 4l4 4-4 4" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>';
      var n = document.createElement("span"); n.className = "sec-name"; n.textContent = name;
      var c = document.createElement("span"); c.className = "sec-cmd"; c.textContent = cmd; c.title = cmd;
      var st = document.createElement("span"); st.className = "sec-state"; st.textContent = "running…";
      head.appendChild(n); head.appendChild(c); head.appendChild(st);
      var b = document.createElement("div"); b.className = "sec-body";
      s.appendChild(head); s.appendChild(b);
      head.addEventListener("click", function () { s.classList.toggle("open"); s.userToggled = true; });
      body.appendChild(s);
      var o = { el: s, body: b, state: st, name: name };
      sections.push(o);
      return o;
    }
    function closeSection(s, rest) {
      var m = /^exit (-?\d+) in (.+)$/.exec(rest);
      var ok = !m || m[1] === "0";
      s.el.classList.remove("live");
      s.el.classList.add(ok ? "ok" : "fail");
      s.state.textContent = m ? (ok ? "✓ " + m[2] : "✗ exit " + m[1] + " · " + m[2]) : "✓ " + rest;
      if (ok && s.name !== "ansible" && !s.el.userToggled) s.el.classList.remove("open");
    }

    // -------- lines
    function addLine(raw, cls) {
      var cr = raw.lastIndexOf("\r", raw.length - 2);
      if (cr >= 0) raw = raw.slice(cr + 1);
      raw = raw.replace(/\r$/, "");
      var plain = raw.replace(STRIP, "");
      var n = lines.length + 1;
      var hm = HEAD.exec(plain), fm = !hm && FOOT.exec(plain);
      if (hm) sec = newSection(hm[1], hm[2]);
      var el = document.createElement("div");
      el.className = "ln" + (cls ? " " + cls : "");
      el.setAttribute("data-n", n);
      var tx = document.createElement("span");
      tx.className = "tx";
      ansi(tx, raw, sgr);
      el.appendChild(tx);
      if (/^\*\*\* /.test(plain)) el.classList.add("err");
      else if (fm || /^ansibled run \d+/.test(plain)) el.classList.add("note");
      if (hm) el.classList.add("hd");
      (sec ? sec.body : body).appendChild(el);
      lines.push(plain); els.push(el); secOf.push(sec);
      if (fm && sec && fm[1] === sec.name) { closeSection(sec, fm[2]); sec = null; }
      if (query && plain.toLowerCase().indexOf(query) >= 0) { hits.push(lines.length - 1); el.classList.add("hit"); updateHits(); }
      if (!follow) unseen++;
    }
    function append(text) {
      var parts = (partialLine + text).split("\n");
      partialLine = parts.pop();
      for (var i = 0; i < parts.length; i++) addLine(parts[i]);
      afterAppend();
    }
    function afterAppend() {
      var wait = root.querySelector("#console-wait");
      if (wait && lines.length) wait.remove();
      if (follow) toBottom(); else followLabel.textContent = unseen ? "Follow (" + unseen + " new)" : "Follow";
    }
    function finish(state) {
      if (partialLine) { addLine(partialLine); partialLine = ""; afterAppend(); }
      sections.forEach(function (s) {
        if (s.el.classList.contains("live")) {
          s.el.classList.remove("live");
          s.state.textContent = "stopped";
        }
      });
      done = true;
      setStatus(state, false);
      if (!lines.length) {
        var wait = root.querySelector("#console-wait");
        if (wait) wait.textContent = "No output.";
      }
    }
    function setStatus(text, live) {
      status.classList.toggle("live", !!live);
      status.textContent = text;
    }

    // -------- search
    function updateHits() {
      hitsLabel.textContent = query ? (hits.length ? (cur + 1) + "/" + hits.length : "0/0") : "";
    }
    function runSearch() {
      hits.forEach(function (i) { els[i].classList.remove("hit", "hit-cur"); });
      hits = []; cur = -1;
      query = search.value.trim().toLowerCase();
      if (query) {
        for (var i = 0; i < lines.length; i++) {
          if (lines[i].toLowerCase().indexOf(query) >= 0) { hits.push(i); els[i].classList.add("hit"); }
        }
        if (hits.length) go(0);
      }
      updateHits();
    }
    function go(k) {
      if (!hits.length) return;
      if (cur >= 0 && els[hits[cur]]) els[hits[cur]].classList.remove("hit-cur");
      cur = (k + hits.length) % hits.length;
      var i = hits[cur], s = secOf[i];
      if (s && !s.el.classList.contains("open")) s.el.classList.add("open");
      els[i].classList.add("hit-cur");
      if (follow) setFollow(false);
      els[i].scrollIntoView({ block: "center" });
      pinTop = body.scrollTop;
      updateHits();
    }
    var deb = null;
    search.addEventListener("input", function () { clearTimeout(deb); deb = setTimeout(runSearch, 150); });
    search.addEventListener("keydown", function (e) {
      if (e.key === "Enter") { e.preventDefault(); go(cur + (e.shiftKey ? -1 : 1)); }
      if (e.key === "Escape") { search.value = ""; runSearch(); }
    });

    // -------- toolbar
    function setWrap(on) {
      body.classList.toggle("wrap", on);
      var b = root.querySelector('[data-act="wrap"]');
      if (b) b.setAttribute("aria-pressed", on ? "true" : "false");
    }
    root.querySelectorAll("[data-act]").forEach(function (b) {
      b.addEventListener("click", function () {
        switch (b.getAttribute("data-act")) {
          case "wrap": var on = !body.classList.contains("wrap"); setWrap(on); store.set("ansibled.wrap", on ? "1" : "0"); break;
          case "collapse": sections.forEach(function (s) { s.el.classList.remove("open"); }); break;
          case "expand": sections.forEach(function (s) { s.el.classList.add("open"); }); break;
          case "next": go(cur + 1); break;
          case "prev": go(cur - 1); break;
          case "copy":
            var txt = lines.join("\n") + "\n";
            var label = b.textContent;
            (navigator.clipboard ? navigator.clipboard.writeText(txt) : Promise.reject())
              .then(function () { b.textContent = "Copied"; }, function () { b.textContent = "Copy failed"; })
              .then(function () { setTimeout(function () { b.textContent = label; }, 1500); });
            break;
        }
      });
    });
    document.addEventListener("keydown", function (e) {
      if ((e.metaKey || e.ctrlKey) && e.key === "f" && root.offsetParent) { e.preventDefault(); search.focus(); search.select(); }
    });

    // -------- stream
    if (!window.EventSource) { setStatus("needs a newer browser", false); return; }
    var es = new EventSource(root.getAttribute("data-stream"));
    var lastState = null;
    es.addEventListener("log", function (e) {
      var d = JSON.parse(e.data);
      if (d.skipped) addLine("… " + Math.round(d.skipped / 1024) + " KiB of earlier output not shown; use Raw for the whole log", "note");
      append(d.text);
    });
    es.addEventListener("run", function (e) {
      lastState = JSON.parse(e.data);
      if (!lastState.finished) setStatus(lastState.state === "queued" ? "queued" : "live", lastState.state !== "queued");
      if (hooks.onState) hooks.onState(lastState);
    });
    es.addEventListener("done", function () {
      es.close();
      finish(lastState ? lastState.state : "finished");
      if (hooks.onDone) hooks.onDone(lastState);
    });
    es.onerror = function () {
      if (done) return;
      if (es.readyState === 2) setStatus("disconnected", false);
      else setStatus("reconnecting…", false);
    };
    window.addEventListener("pagehide", function () { es.close(); });
    self.es = es;
  }

  // ---------------------------------------------------------------- start
  function start() {
    updateTimes(); updateElapsed(); wireRows(); wireForms(); wireLive(); wireRun();
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", start);
  else start();
})();
