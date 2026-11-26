// cuttle admin: the one script of design 0009 D10. Every page works
// without it; it adds fragment requests, dialogs, toasts that close, the
// unsaved-changes guard, list inputs with chips, and the two shortcuts.
// Plain modern JavaScript: no framework, no build step, no eval, no
// inline handlers, no storage, and no markup from strings: elements are
// created with the DOM, and server fragments are parsed with DOMParser.
//
// The core below defines window.cuttle. A page that needs more adds its
// own block at the end of this file, an IIFE that starts when its page's
// markers exist and uses the core through window.cuttle.
"use strict";

(() => {
  const doc = document;
  doc.documentElement.classList.add("js");
  const $ = (sel, root = doc) => root.querySelector(sel);
  const $$ = (sel, root = doc) => Array.from(root.querySelectorAll(sel));
  const reducedMotion = () => matchMedia("(prefers-reduced-motion: reduce)").matches;
  const SVG = "http://www.w3.org/2000/svg";

  // el creates an element with attributes and children; strings become
  // text nodes, and null children are skipped.
  function el(tag, attrs, ...children) {
    const e = doc.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (v !== null && v !== undefined && v !== false) e.setAttribute(k, v === true ? "" : String(v));
    }
    for (const c of children) {
      if (c !== null && c !== undefined) e.append(c);
    }
    return e;
  }

  // icon draws a symbol of the layout's icons.
  function icon(name) {
    const svg = doc.createElementNS(SVG, "svg");
    svg.setAttribute("class", "ic");
    svg.setAttribute("aria-hidden", "true");
    svg.setAttribute("focusable", "false");
    const use = doc.createElementNS(SVG, "use");
    use.setAttribute("href", "#i-" + name);
    svg.append(use);
    return svg;
  }

  // announce reads a short text through the polite live region.
  function announce(text) {
    const region = $("#announce");
    if (!region) return;
    region.textContent = "";
    setTimeout(() => { region.textContent = text; }, 30);
  }

  // busy marks a control as loading after 150 ms (design 0009 D3.13) and
  // returns the function that ends it.
  function busy(control) {
    if (!control) return () => {};
    const timer = setTimeout(() => control.setAttribute("aria-busy", "true"), 150);
    return () => { clearTimeout(timer); control.removeAttribute("aria-busy"); };
  }

  /* ------------------------------------------------------------ toasts */
  // Toasts report a result (design 0009 D7). An ok toast closes after
  // 8 s, unless the pointer or the focus is on it; warn and error toasts
  // stay until closed.
  const toastMs = 8000;

  function closeToast(t) {
    if (!t.isConnected || t.classList.contains("leaving")) return;
    t.classList.add("leaving");
    setTimeout(() => t.remove(), reducedMotion() ? 0 : 160);
  }

  function armToast(t) {
    if (t.dataset.armed) return;
    t.dataset.armed = "1";
    const close = el("button", { type: "button", class: "btn quiet icon-btn sm", "aria-label": "Close" }, icon("x"));
    close.addEventListener("click", () => closeToast(t));
    t.append(close);
    if (t.dataset.toast !== "ok") return;
    let timer = 0;
    const start = () => { clearTimeout(timer); timer = setTimeout(() => closeToast(t), toastMs); };
    const stop = () => clearTimeout(timer);
    t.addEventListener("pointerenter", stop);
    t.addEventListener("pointerleave", start);
    t.addEventListener("focusin", stop);
    t.addEventListener("focusout", start);
    start();
  }

  // toast shows a toast made in the page: kind is ok, warn, or error.
  function toast(kind, title, detail) {
    const box = $("#toasts");
    if (!box) return null;
    const t = el("div", { class: "toast " + kind, role: kind === "ok" ? "status" : "alert", "data-toast": kind },
      icon(kind === "ok" ? "check" : "alert"),
      el("div", { class: "t-body" }, el("strong", {}, title), detail ? el("span", {}, detail) : null));
    box.append(t);
    armToast(t);
    return t;
  }

  /* --------------------------------------------------------- fragments */
  // A fragment is one block of a page that the server renders alone for a
  // request with Cuttle-Fragment: 1 (design 0009 D10). Its root element
  // carries data-block="<name>" and must be able to stand in <body>, such
  // as a div or a section. The latest request for a target wins; an
  // earlier response is dropped. The server may answer with another
  // block that the request allowed in alts, the ids of other targets
  // named like their blocks: a review refused for errors answers the
  // page's issue block instead of the review (design 0009 D7).
  const pending = new WeakMap();
  // altSaved keeps what an alternative target held before an answer
  // went there instead of to the request's own target, such as the
  // errors of a refused review in a form's issue block. A later answer
  // to the request's own target puts that back, so the refusal does not
  // stay behind the dialog (OI-0006 AA-R1); an answer that lands in the
  // alternative target as its own target forgets it.
  const altSaved = new WeakMap();

  // FragmentError is an answer that is not a fragment: a refusal such as
  // 403 after the session ended or 400 for a bad form, or a redirect to
  // the sign-in page. It keeps the status and the server's first line.
  class FragmentError extends Error {
    constructor(status, text, signedOut) {
      super(text);
      this.status = status;
      this.signedOut = signedOut;
    }
  }

  async function loadFragment(target, url, init = {}, alts = []) {
    const previous = pending.get(target);
    if (previous) previous.abort();
    const ctl = new AbortController();
    pending.set(target, ctl);
    try {
      const res = await fetch(url, {
        ...init,
        headers: { "Cuttle-Fragment": "1" },
        credentials: "same-origin",
        signal: ctl.signal,
      });
      const type = res.headers.get("Content-Type") || "";
      const text = await res.text();
      const refused = () => new FragmentError(res.status, type.startsWith("text/plain") ? text.trim().split("\n")[0] : "",
        res.status === 403 || (res.redirected && new URL(res.url).pathname === "/login"));
      if (!type.startsWith("text/html")) throw refused();
      const parsed = new DOMParser().parseFromString(text, "text/html");
      const root = parsed.body.firstElementChild;
      if (!root || !root.hasAttribute("data-block")) throw refused();
      // DOMParser never runs scripts; the fragment keeps none anyway.
      $$("script", root).forEach((s) => s.remove());
      const alt = alts.includes(root.dataset.block) && doc.getElementById(root.dataset.block);
      const dest = alt || target;
      const node = doc.adoptNode(root);
      if (alt && alt !== target) {
        if (!altSaved.has(alt)) altSaved.set(alt, Array.from(alt.childNodes));
      } else {
        altSaved.delete(target);
        for (const id of alts) {
          const other = doc.getElementById(id);
          const saved = other && other !== target && altSaved.get(other);
          if (saved) {
            altSaved.delete(other);
            other.replaceChildren(...saved);
          }
        }
      }
      dest.replaceChildren(node);
      enhance(node);
      return { status: res.status, root: node, target: dest };
    } finally {
      if (pending.get(target) === ctl) pending.delete(target);
    }
  }

  // failed shows the request-failed state of design 0009 D3.13 in the
  // target, saying what happened: no answer, an ended session, or the
  // server's refusal; retry runs the request again the way the page
  // would without the script.
  function failed(target, retry, err) {
    let text = "Could not reach cuttle. Check that it is running, then try again.";
    let signIn = null;
    if (err instanceof FragmentError && err.signedOut) {
      text = "Your session ended or this page expired. Sign in again, then try again.";
      signIn = el("a", { class: "btn sm", href: "/login" }, "Sign in");
    } else if (err instanceof FragmentError) {
      text = "cuttle refused the request" + (err.message ? ": " + err.message.slice(0, 200) : " (" + err.status + ")") + ".";
    }
    const again = el("button", { type: "button", class: "btn sm" }, "Try again");
    again.addEventListener("click", retry);
    const panel = el("div", { class: "failed", role: "alert", "data-block": "failed" }, el("span", {}, text), signIn, again);
    target.replaceChildren(panel);
  }

  // announceResult reads the first status of a loaded fragment aloud.
  function announceResult(root) {
    const status = $(".banner strong, [role=status], [role=alert]", root);
    if (status) announce(status.textContent.trim().slice(0, 240));
  }

  // A submit button with data-fragment="<id>" posts its form for the
  // block that replaces the children of the element with that id, or of
  // data-target; data-alt lists the ids of other targets the answer may
  // name. A target inside a closed dialog opens it. After a failed
  // request the form submits normally.
  doc.addEventListener("submit", (e) => {
    const form = e.target;
    const sub = e.submitter;
    const name = sub && sub.dataset.fragment;
    if (!name || form.dataset.plain) {
      if (!e.defaultPrevented) guards.leaving(form);
      return;
    }
    const target = doc.getElementById(sub.dataset.target || name);
    if (!target) return;
    e.preventDefault();
    const method = (sub.getAttribute("formmethod") || form.getAttribute("method") || "get").toUpperCase();
    let url = sub.getAttribute("formaction") || form.getAttribute("action") || location.pathname;
    const params = new URLSearchParams(new FormData(form, sub));
    const init = { method };
    if (method === "GET") url += (url.includes("?") ? "&" : "?") + params;
    else init.body = params;
    const done = busy(sub);
    const alts = (sub.dataset.alt || "").split(/\s+/).filter(Boolean);
    loadFragment(target, url, init, alts).then((r) => {
      done();
      announceResult(r.root);
      const dlg = r.target.closest("dialog");
      if (dlg && !dlg.open) openDialog(dlg, sub);
    }, (err) => {
      done();
      if (err.name === "AbortError") return;
      form.dataset.plain = "1";
      failed(target, () => form.requestSubmit(sub), err);
    });
  });

  // A link with data-fragment="<id>" loads its href's block into the
  // element with that id; without the script it opens the whole page.
  doc.addEventListener("click", (e) => {
    const a = e.target.closest("a[data-fragment]");
    if (!a || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    const target = doc.getElementById(a.dataset.target || a.dataset.fragment);
    if (!target) return;
    e.preventDefault();
    const done = busy(a);
    loadFragment(target, a.href, { method: "GET" }).then((r) => {
      done();
      announceResult(r.root);
      const dlg = target.closest("dialog");
      if (dlg && !dlg.open) openDialog(dlg, a);
    }, (err) => {
      done();
      if (err.name !== "AbortError") location.assign(a.href);
    });
  });

  /* ----------------------------------------------------------- dialogs */
  // Dialogs are <dialog> elements opened with showModal, which keeps
  // focus inside them; Escape closes them, and focus returns to the
  // control that opened them (design 0009 D11).
  const openers = new WeakMap();

  function openDialog(dlg, opener) {
    if (dlg.open) return;
    openers.set(dlg, opener || doc.activeElement);
    dlg.showModal();
    const first = $("[autofocus]", dlg) || $(".dlg-body input, .dlg-body select, .dlg-body textarea, .dlg-foot .btn.primary", dlg);
    if (first) first.focus();
  }

  function armDialog(dlg) {
    if (dlg.dataset.armed) return;
    dlg.dataset.armed = "1";
    dlg.addEventListener("close", () => {
      const opener = openers.get(dlg);
      openers.delete(dlg);
      if (opener && opener.isConnected && typeof opener.focus === "function") opener.focus();
    });
  }

  doc.addEventListener("click", (e) => {
    const open = e.target.closest("[data-dialog]");
    if (open) {
      const dlg = doc.getElementById(open.dataset.dialog);
      if (dlg && dlg.tagName === "DIALOG") {
        e.preventDefault();
        openDialog(dlg, open);
      }
      return;
    }
    const close = e.target.closest("dialog [data-close]");
    if (close) {
      e.preventDefault();
      close.closest("dialog").close();
    }
  });

  /* ------------------------------------------------- unsaved changes */
  // A form with data-guard="<message>" is watched: once its fields
  // differ from what the page rendered, or the server marked it with
  // data-guard-dirty, the save bar offers Discard and the form's
  // [data-primary] button, and leaving the page asks first (design 0009
  // D3.13). Discard loads data-guard-discard, or resets the form.
  const guards = (() => {
    const watched = new Map();
    let current = null;
    let leaving = false;
    const bar = $("#savebar");

    // A field marked data-unguarded, such as a test input that posts with
    // the form, is not an edit of what the form saves.
    const snapshot = (form) => {
      const p = new URLSearchParams(new FormData(form));
      p.delete("csrf");
      for (const e of form.elements) if (e.name && e.hasAttribute("data-unguarded")) p.delete(e.name);
      return p.toString();
    };

    function show() {
      const dirty = Array.from(watched.entries()).filter(([, s]) => s.dirty).map(([f]) => f);
      current = dirty.length ? dirty[dirty.length - 1] : null;
      if (!bar) return;
      doc.body.classList.toggle("has-savebar", !!current);
      if (!current) {
        bar.hidden = true;
        return;
      }
      $("#savebar-msg", bar).textContent = current.dataset.guard || "Unsaved changes";
      const primary = $("[data-primary]", current);
      const submit = $("#savebar-submit", bar);
      submit.hidden = !primary;
      if (primary) submit.textContent = primary.textContent.trim();
      if (bar.hidden) {
        bar.hidden = false;
        announce(current.dataset.guard || "Unsaved changes");
      }
    }

    function update(form) {
      const s = watched.get(form);
      if (!s) return;
      s.dirty = s.forced || snapshot(form) !== s.initial;
      show();
    }

    function watch(form) {
      if (watched.has(form)) return;
      watched.set(form, { initial: snapshot(form), forced: form.hasAttribute("data-guard-dirty"), dirty: false });
      const changed = () => update(form);
      form.addEventListener("input", changed);
      form.addEventListener("change", changed);
      form.addEventListener("reset", () => setTimeout(() => {
        const s = watched.get(form);
        if (s) s.forced = false;
        form.dispatchEvent(new Event("cuttle:reset"));
        changed();
      }));
      update(form);
    }

    if (bar) {
      $("#savebar-discard", bar).addEventListener("click", () => {
        const form = current;
        if (!form) return;
        if (form.dataset.guardDiscard) {
          leaving = true;
          location.assign(form.dataset.guardDiscard);
          return;
        }
        form.reset();
      });
      $("#savebar-submit", bar).addEventListener("click", () => {
        const primary = current && $("[data-primary]", current);
        if (primary) current.requestSubmit(primary);
      });
    }

    window.addEventListener("beforeunload", (e) => {
      if (leaving || !Array.from(watched.values()).some((s) => s.dirty)) return;
      e.preventDefault();
      e.returnValue = "";
    });
    // The page shows again from the back-forward cache: nothing leaves.
    window.addEventListener("pageshow", () => { leaving = false; });

    return {
      watch,
      update,
      // leaving lets a watched form's own submit leave the page, and
      // a form marked data-leave, such as the review's Apply changes,
      // which posts the edit that the watched form holds.
      leaving(form) {
        if (watched.has(form) || form.hasAttribute("data-leave")) leaving = true;
      },
      dirty: (form) => !!(watched.get(form) || {}).dirty,
    };
  })();

  // Command-Enter or Control-Enter in a form presses its [data-primary]
  // button (design 0009 D3.4, D5).
  doc.addEventListener("keydown", (e) => {
    if (e.key !== "Enter" || !(e.metaKey || e.ctrlKey) || e.altKey || e.shiftKey) return;
    const form = e.target.closest && e.target.closest("form");
    const primary = form && $("[data-primary]", form);
    if (!primary || primary.disabled) return;
    e.preventDefault();
    form.requestSubmit(primary);
  });

  /* -------------------------------------------------------- list input */
  // A textarea with data-list holds one entry per line and is what the
  // form submits; the script shows its entries as chips with a text field
  // that splits typed and pasted text on newlines, commas, and spaces, as
  // the server does (design 0009 D5). data-bad lists the 1-based
  // positions of entries the server refused.
  const splitList = (text) => text.split(/[\s,]+/).filter(Boolean);

  function armList(area) {
    if (area.dataset.armed) return;
    area.dataset.armed = "1";
    const id = area.id || "list-" + Math.random().toString(36).slice(2);
    area.id = id;
    const entry = el("input", {
      type: "text", id: id + "-entry", class: "mono", autocomplete: "off", spellcheck: "false",
      autocapitalize: "off", "aria-describedby": area.getAttribute("aria-describedby"),
      "aria-invalid": area.getAttribute("aria-invalid"),
    });
    const box = el("div", { class: "tokens" }, entry);
    area.after(box);
    area.hidden = true;
    for (const label of $$(`label[for="${CSS.escape(id)}"]`)) label.htmlFor = entry.id;
    const placeholder = area.getAttribute("placeholder") || "Type or paste, one per line";

    const values = () => $$(".tok", box).map((t) => t.dataset.v);
    const write = () => {
      area.value = values().join("\n");
      area.dispatchEvent(new Event("input", { bubbles: true }));
    };
    const chip = (v, bad) => {
      const rm = el("button", { type: "button", "aria-label": "Remove " + v }, icon("x"));
      const t = el("span", { class: bad ? "tok bad" : "tok", "data-v": v }, v, rm);
      rm.addEventListener("click", () => {
        t.remove();
        write();
        placeholderFor();
        entry.focus();
        announce("Removed " + v);
      });
      return t;
    };
    const placeholderFor = () => { entry.placeholder = values().length ? "" : placeholder; };
    const build = () => {
      $$(".tok", box).forEach((t) => t.remove());
      const bad = new Set(splitList(area.dataset.bad || "").map(Number));
      splitList(area.value).forEach((v, i) => entry.before(chip(v, bad.has(i + 1))));
      placeholderFor();
    };
    const add = (text) => {
      const have = values();
      const added = [];
      for (const v of splitList(text)) {
        if (have.includes(v)) {
          const old = $$(".tok", box).find((t) => t.dataset.v === v);
          if (old) {
            old.classList.remove("again");
            void old.offsetWidth;
            old.classList.add("again");
          }
          continue;
        }
        have.push(v);
        added.push(v);
        entry.before(chip(v, false));
      }
      entry.value = "";
      placeholderFor();
      if (added.length) {
        write();
        announce(added.length === 1 ? "Added " + added[0] : "Added " + added.length + " entries");
      }
    };

    entry.addEventListener("keydown", (e) => {
      if ((e.key === "Enter" || e.key === "," || e.key === " ") && entry.value.trim()) {
        e.preventDefault();
        add(entry.value);
      } else if (e.key === "Enter" && !(e.metaKey || e.ctrlKey)) {
        e.preventDefault();
      } else if (e.key === "Backspace" && !entry.value) {
        // The last entry goes back into the field for editing.
        const last = $$(".tok", box).pop();
        if (last) {
          e.preventDefault();
          entry.value = last.dataset.v;
          last.remove();
          write();
        }
      }
    });
    entry.addEventListener("paste", (e) => {
      const text = (e.clipboardData || window.clipboardData).getData("text");
      if (/[\s,]/.test(text.trim())) {
        e.preventDefault();
        add(entry.value + " " + text);
      }
    });
    entry.addEventListener("blur", () => { if (entry.value.trim()) add(entry.value); });
    box.addEventListener("click", (e) => { if (e.target === box) entry.focus(); });
    // A reset restores the textarea; the chips follow it.
    if (area.form) area.form.addEventListener("cuttle:reset", build);
    build();
  }

  /* ------------------------------------------- menus and shortcuts */
  // Escape or a click elsewhere closes an open menu disclosure, the
  // phone navigation included.
  const menus = () => $$("details[open].menu-details, .topbar details[open]");
  doc.addEventListener("keydown", (e) => {
    if (e.key !== "Escape") return;
    for (const d of menus()) {
      d.open = false;
      const s = $("summary", d);
      if (s) s.focus();
    }
  });
  doc.addEventListener("click", (e) => {
    for (const d of menus()) if (!d.contains(e.target)) d.open = false;
  });

  // "/" focuses the page's filter or test field, [data-slash] (design
  // 0009 D5), unless the focus is in a field already.
  doc.addEventListener("keydown", (e) => {
    if (e.key !== "/" || e.metaKey || e.ctrlKey || e.altKey) return;
    const t = e.target;
    if (t.closest && t.closest("input, textarea, select, [contenteditable]")) return;
    const field = $("[data-slash]");
    if (!field) return;
    e.preventDefault();
    field.focus();
    if (field.select) field.select();
  });

  /* ---------------------------------------------------------- refresh */
  // A page with data-refresh="<seconds>", such as /live, reloads itself
  // as its <noscript> meta refresh would, but waits while a menu or a
  // dialog is open, so the refresh never closes what the operator opened.
  const refresh = Number(doc.body.dataset.refresh || 0);
  if (refresh > 0) {
    const tick = () => {
      if (menus().length || $("dialog[open]")) {
        setTimeout(tick, 1000);
        return;
      }
      location.reload();
    };
    setTimeout(tick, refresh * 1000);
  }

  /* ------------------------------------------------------------ start */
  // enhance arms the controls inside root: toasts, dialogs, list inputs,
  // and guarded forms.
  function enhance(root) {
    const all = (sel) => (root.matches && root.matches(sel) ? [root] : []).concat($$(sel, root));
    all(".toast").forEach(armToast);
    all("dialog").forEach(armDialog);
    all("textarea[data-list]").forEach(armList);
    all("form[data-guard]").forEach(guards.watch);
  }

  window.cuttle = Object.freeze({
    $, $$, el, icon, announce, busy, toast, loadFragment, failed, openDialog, enhance,
    guard: Object.freeze({ watch: guards.watch, update: guards.update, dirty: guards.dirty }),
    splitList,
  });
  enhance(doc);
})();

/* -------------------------------------------------------- Domains page */
// A date range needs both dates: with one filled, the browser asks for
// the other before the form goes out, instead of the server's 400.
(() => {
  const { $ } = window.cuttle;
  const from = $("#f-from");
  const to = $("#f-to");
  if (!from || !to) return;
  const pair = () => {
    const one = !from.value !== !to.value;
    from.required = one && !from.value;
    to.required = one && !to.value;
  };
  from.addEventListener("input", pair);
  to.addEventListener("input", pair);
  pair();
})();

/* ------------------------------------------ Rules and upstreams (task C) */
// Issues under their fields (design 0009 D7, D10): the form-issues block
// that a check or a refused review answers holds each field's issues in
// hidden holders; they are copied under the fields, with aria-invalid
// and the refused entries of a list input marked. The holders stay, so
// that a block put back later puts its own issues back too.
(() => {
  const { $, $$, splitList } = window.cuttle;
  function distribute(block) {
    for (const h of $$("[data-issues-for]", block)) {
      const id = h.dataset.issuesFor;
      const slot = document.getElementById(id + "-issues");
      if (!slot) continue;
      slot.replaceChildren(...Array.from(h.childNodes, (n) => n.cloneNode(true)));
      const invalid = h.hasAttribute("data-invalid");
      const field = slot.closest(".field");
      if (field) field.classList.toggle("invalid", invalid);
      const control = document.getElementById(id);
      if (!control || !/^(INPUT|TEXTAREA|SELECT)$/.test(control.tagName)) continue;
      const mark = (el) => (invalid ? el.setAttribute("aria-invalid", "true") : el.removeAttribute("aria-invalid"));
      mark(control);
      if (control.matches("textarea[data-list]")) {
        control.dataset.bad = h.dataset.bad || "";
        const box = control.nextElementSibling;
        if (box && box.classList.contains("tokens")) {
          const bad = new Set(splitList(h.dataset.bad || "").map(Number));
          $$(".tok", box).forEach((t, i) => t.classList.toggle("bad", bad.has(i + 1)));
          const entry = $("input", box);
          if (entry) mark(entry);
        }
      }
    }
  }
  new MutationObserver((records) => {
    for (const r of records) {
      for (const n of r.addedNodes) {
        if (n.nodeType !== 1) continue;
        if (n.matches('[data-block="form-issues"]')) distribute(n);
        else $$('[data-block="form-issues"]', n).forEach(distribute);
      }
    }
  }).observe(document.body, { childList: true, subtree: true });

  // A choice of a row menu closes the menu.
  document.addEventListener("click", (e) => {
    const item = e.target.closest(".menu-details .menu [role=menuitem]");
    if (item && !item.disabled) item.closest("details").open = false;
  });

  // The upstream editor dialog drops its form when it closes. The form in
  // the dialog is not watched for unsaved changes, as the interface
  // editor's dialog is not; the editor's own page is.
  const slot = document.getElementById("upstream-editor");
  const dlg = slot && slot.closest("dialog");
  if (dlg) {
    dlg.addEventListener("close", () => {
      setTimeout(() => { if (!dlg.open) slot.replaceChildren(); }, 50);
    });
  }
})();

/* ------------------------------------------------------- Rule editor */
// The live check and the live test of design 0009 D5: 500 ms after the
// last change the draft is checked, and tested when a target is given;
// the latest request wins. A name is suggested from the first pattern of
// a new rule until the operator types one.
(() => {
  const { $, loadFragment, failed, splitList } = window.cuttle;
  const form = $("form[data-live]");
  if (!form) return;
  const issues = document.getElementById("form-issues");
  const out = document.getElementById("rule-test");
  const url = document.getElementById("rule-test-url");
  const action = form.getAttribute("action");
  const post = (intent, target) => {
    const body = new URLSearchParams(new FormData(form));
    body.set("intent", intent);
    return loadFragment(target, action, { method: "POST", body }).catch((err) => {
      if (err.name !== "AbortError") failed(target, () => post(intent, target), err);
    });
  };
  const test = () => {
    if (!url || !out) return;
    if (!url.value.trim()) {
      out.replaceChildren();
      return;
    }
    post("test", out);
  };
  let checkTimer = 0;
  let testTimer = 0;
  const changed = () => {
    clearTimeout(checkTimer);
    checkTimer = setTimeout(() => {
      post("check", issues);
      test();
    }, 500);
  };
  form.addEventListener("input", changed);
  form.addEventListener("change", changed);
  if (url) {
    url.addEventListener("input", (e) => {
      e.stopPropagation();
      clearTimeout(testTimer);
      testTimer = setTimeout(test, 500);
    });
    url.addEventListener("keydown", (e) => {
      if (e.key !== "Enter" || e.metaKey || e.ctrlKey) return;
      e.preventDefault();
      clearTimeout(testTimer);
      test();
    });
  }
  for (const r of document.querySelectorAll('input[name="source"][form="rule-form"]')) {
    r.addEventListener("change", (e) => { e.stopPropagation(); test(); });
  }
  const name = $("input[data-suggest]", form);
  if (name) {
    let auto = name.value === "";
    name.addEventListener("input", () => { auto = name.value === ""; });
    form.addEventListener("input", (e) => {
      if (!auto || e.target === name) return;
      const first = ["domains", "hosts", "cidrs"].map((n) => splitList(form.elements[n] ? form.elements[n].value : "")[0]).find(Boolean) || "";
      name.value = first.toLowerCase().replace(/[^a-z0-9._-]+/g, "-").replace(/^[^a-z0-9]+/, "").slice(0, 64);
    });
  }
})();

/* -------------------------------------------------------- Rules page */
// Test a URL as you type, marking the chain: the rules tried and passed,
// the one that matched, and those not reached (design 0009 D3.3, D11).
// While the order has unsaved moves, the test runs against that order.
// The filter hides the rows it does not match and turns reordering off.
// Reordering: drag a row by its handle; or focus the handle, press Space
// or Enter, move with the arrow keys, and drop with Space or Enter, or
// cancel with Escape; or use the row menu. Every move changes the page
// only; the save bar reviews them all at once.
(() => {
  const { $, $$, loadFragment, failed, announce, guard } = window.cuttle;
  const out = document.getElementById("rules-test");
  const testForm = document.getElementById("rules-test-form");
  if (!out || !testForm) return;
  const chain = document.getElementById("chain");
  const order = document.getElementById("order-form");
  const def = chain && $(".rule.default", chain);
  const reorderable = !!(chain && order && chain.hasAttribute("data-reorder"));
  const reduced = () => matchMedia("(prefers-reduced-motion: reduce)").matches;
  const rows = () => (chain ? $$(".rule[data-name]", chain) : []);
  let filtering = false;

  // refresh numbers the rows as they stand, with their position fields,
  // handle names, and the moves their menus allow.
  function refresh() {
    const list = rows();
    list.forEach((r, i) => {
      const n = i + 1;
      $(".node", r).textContent = n;
      const pos = $('input[name="position"]', r);
      if (pos) pos.value = n;
      const h = $(".handle", r);
      if (h) {
        h.setAttribute("aria-label", `Move rule ${r.dataset.name}, position ${n} of ${list.length}`);
        h.disabled = filtering;
      }
      for (const b of $$("[data-move]", r)) {
        const up = b.dataset.move === "top" || b.dataset.move === "up";
        b.disabled = filtering || (up ? i === 0 : i === list.length - 1);
      }
    });
  }

  function mark(block) {
    if (!chain) return;
    for (const r of [...rows(), def]) {
      r.classList.remove("passed", "matched", "unreached");
      r.style.transitionDelay = "";
    }
    def.classList.remove("a-direct", "a-upstream", "a-block");
    chain.classList.remove("tested");
    if (!block || !block.hasAttribute("data-match")) return;
    const name = block.dataset.match;
    const port = block.hasAttribute("data-port");
    let state = port ? "unreached" : "passed";
    let matched = null;
    rows().forEach((r, i) => {
      if (!port && name && r.dataset.name === name) {
        r.classList.add("matched");
        matched = r;
        state = "unreached";
      } else {
        r.classList.add(state);
      }
      if (!reduced()) r.style.transitionDelay = i * 40 + "ms";
    });
    if (block.hasAttribute("data-default") && !port) {
      def.classList.add("matched", "a-" + (block.dataset.action || "direct"));
      matched = def;
    } else {
      def.classList.add("unreached");
    }
    requestAnimationFrame(() => chain.classList.add("tested"));
    if (matched) {
      const r = matched.getBoundingClientRect();
      if (r.top > innerHeight || r.bottom < 0) matched.scrollIntoView({ block: "center", behavior: reduced() ? "auto" : "smooth" });
    }
  }

  const input = $("#test-url", testForm);
  const test = () => {
    if (!input.value.trim()) {
      out.replaceChildren();
      mark(null);
      return;
    }
    const src = $('input[name="source"]:checked', testForm);
    const source = src ? src.value : "local";
    let req;
    if (order && guard.dirty(order)) {
      const body = new URLSearchParams(new FormData(order));
      body.set("intent", "test");
      body.set("test", input.value);
      body.set("source", source);
      req = loadFragment(out, order.getAttribute("action"), { method: "POST", body });
    } else {
      req = loadFragment(out, "/rules?" + new URLSearchParams({ test: input.value, source }), { method: "GET" });
    }
    req.then((r) => mark(r.root), (err) => {
      if (err.name !== "AbortError") failed(out, test, err);
    });
  };
  let timer = 0;
  input.addEventListener("input", () => { clearTimeout(timer); timer = setTimeout(test, 500); });
  testForm.addEventListener("change", (e) => { if (e.target.name === "source") test(); });
  testForm.addEventListener("submit", (e) => { e.preventDefault(); clearTimeout(timer); test(); });

  // The filter, as you type, unless the page is filtered already.
  const filter = document.getElementById("rule-filter");
  const meta = document.getElementById("filter-meta");
  if (filter && chain && !filter.value) {
    filter.form.addEventListener("submit", (e) => e.preventDefault());
    filter.addEventListener("input", () => {
      const q = filter.value.trim().toLowerCase();
      let shown = 0;
      for (const r of rows()) {
        const hit = !q || r.dataset.hay.includes(q);
        r.classList.toggle("filtered-out", !hit);
        if (hit) shown++;
      }
      filtering = !!q;
      chain.classList.toggle("filtering", filtering);
      const total = rows().length;
      meta.textContent = q ? `${shown} of ${total} rules match` + (reorderable ? "; reordering is off while filtering." : ".") :
        total + (total === 1 ? " rule" : " rules");
      refresh();
    });
  }

  if (!reorderable) return;
  function moved() {
    refresh();
    guard.update(order);
    if (input.value.trim()) test();
  }
  function moveTo(row, index) {
    const list = rows().filter((r) => r !== row);
    chain.insertBefore(row, list[index] || def);
  }
  refresh();

  // The row menu.
  chain.addEventListener("click", (e) => {
    const b = e.target.closest("[data-move]");
    if (!b || b.disabled) return;
    const row = b.closest(".rule");
    const list = rows();
    const i = list.indexOf(row);
    const to = { top: 0, up: i - 1, down: i + 1, bottom: list.length - 1 }[b.dataset.move];
    moveTo(row, to);
    moved();
    $("summary", row).focus();
    announce(`${row.dataset.name} moved to position ${to + 1} of ${list.length}.`);
  });

  // Drag by the handle.
  let dragged = null;
  const clearDrop = () => $$(".drop-before", chain).forEach((r) => r.classList.remove("drop-before"));
  chain.addEventListener("dragstart", (e) => {
    const h = e.target.closest(".handle");
    if (!h || h.disabled) {
      e.preventDefault();
      return;
    }
    dragged = h.closest(".rule");
    dragged.classList.add("dragging");
    e.dataTransfer.effectAllowed = "move";
    e.dataTransfer.setData("text/plain", dragged.dataset.name);
    e.dataTransfer.setDragImage(dragged, 30, 20);
  });
  chain.addEventListener("dragover", (e) => {
    const r = dragged && e.target.closest(".rule");
    if (!r) return;
    e.preventDefault();
    clearDrop();
    if (r !== dragged) r.classList.add("drop-before");
  });
  chain.addEventListener("drop", (e) => {
    const r = dragged && e.target.closest(".rule");
    if (!r) return;
    e.preventDefault();
    if (r !== dragged) {
      chain.insertBefore(dragged, r);
      moved();
      announce(`${dragged.dataset.name} moved to position ${rows().indexOf(dragged) + 1}.`);
    }
  });
  chain.addEventListener("dragend", () => {
    if (dragged) dragged.classList.remove("dragging");
    dragged = null;
    clearDrop();
  });

  // The keyboard on the handle. An arrow key moves the neighbour across
  // the lifted row, so that the focused handle stays where it is in the
  // document and keeps the focus.
  let lifted = null;
  const drop = (cancel, refocus) => {
    const row = lifted.row;
    if (cancel) moveTo(row, lifted.from);
    row.classList.remove("lifted");
    lifted = null;
    moved();
    if (refocus) $(".handle", row).focus();
    const n = rows().indexOf(row) + 1;
    announce(cancel ? `Move cancelled. ${row.dataset.name} is back at position ${n}.` : `Dropped ${row.dataset.name} at position ${n} of ${rows().length}.`);
  };
  chain.addEventListener("keydown", (e) => {
    const h = e.target.closest(".handle");
    if (!h || h.disabled) return;
    const row = h.closest(".rule");
    const list = rows();
    const i = list.indexOf(row);
    if (e.key === " " || e.key === "Enter") {
      e.preventDefault();
      if (lifted && lifted.row === row) {
        drop(false, true);
        return;
      }
      if (lifted) drop(true, false);
      lifted = { row, from: i };
      row.classList.add("lifted");
      announce(`Picked up ${row.dataset.name}, position ${i + 1} of ${list.length}. Use the arrow keys to move it.`);
    } else if (lifted && lifted.row === row && (e.key === "ArrowUp" || e.key === "ArrowDown")) {
      e.preventDefault();
      const up = e.key === "ArrowUp";
      const other = list[i + (up ? -1 : 1)];
      if (!other) return;
      chain.insertBefore(other, up ? row.nextSibling : row);
      refresh();
      announce(`${row.dataset.name}, position ${rows().indexOf(row) + 1} of ${list.length}.`);
    } else if (lifted && lifted.row === row && e.key === "Escape") {
      e.preventDefault();
      e.stopPropagation();
      drop(true, true);
    }
  });
  chain.addEventListener("focusout", (e) => {
    if (lifted && !lifted.row.contains(e.relatedTarget)) drop(false, false);
  });
})();

/* ------------------------------------------- Interface list editor */
// The editor of design 0009 D3.8 works without the script: each of its
// buttons submits the form for editing with the change in the query.
// With the script the buttons and the Add select change the list in
// place: the hidden inputs that the form posts in order, the positions,
// and the Move buttons at either end.
(() => {
  const { $, $$, el, icon, announce } = window.cuttle;

  function renumber(field) {
    const items = $$(".iflist > li[data-n]", field);
    items.forEach((li, i) => {
      $(".pos", li).textContent = String(i + 1);
      $('[data-ifl="up"]', li).disabled = i === 0;
      $('[data-ifl="down"]', li).disabled = i === items.length - 1;
    });
    const empty = $(".ifl-empty", field);
    if (empty) empty.hidden = items.length > 0;
    const sel = $("select[data-ifl-add]", field);
    if (sel) sel.closest(".iflist-add").hidden = sel.options.length < 2;
    // The unsaved-changes guard watches the form's input events.
    field.dispatchEvent(new Event("input", { bubbles: true }));
  }

  function button(act, label, name, sym) {
    return el("button", { class: "btn quiet icon-btn sm", type: "button", "data-ifl": act, "aria-label": label + " " + name }, icon(sym));
  }

  function entry(field, name, state, text) {
    return el("li", { "data-n": name },
      el("input", { type: "hidden", name: field.dataset.iflist, value: name }),
      el("span", { class: "pos" }),
      el("span", { class: "ifl-name" }, el("span", { class: "name-code" }, name), el("span", { class: "state " + state }, text)),
      el("span", { class: "moves" }, button("up", "Move", name + " up", "up"), button("down", "Move", name + " down", "down")),
      button("remove", "Remove", name, "x"));
  }

  document.addEventListener("click", (e) => {
    const b = e.target.closest("[data-iflist] [data-ifl]");
    if (!b) return;
    e.preventDefault();
    const field = b.closest("[data-iflist]");
    const li = b.closest("li");
    const name = li.dataset.n;
    const act = b.dataset.ifl;
    if (act === "up" && li.previousElementSibling) li.previousElementSibling.before(li);
    else if (act === "down" && li.nextElementSibling && li.nextElementSibling.dataset.n) li.nextElementSibling.after(li);
    else if (act === "remove") {
      const sel = $("select[data-ifl-add]", field);
      const st = $(".state", li);
      if (sel) sel.append(el("option", { value: name, "data-state": st.className.replace("state ", ""), "data-text": st.textContent }, name));
      li.remove();
    }
    renumber(field);
    if (act === "remove") {
      announce("Removed " + name);
      ($('.iflist > li[data-n] [data-ifl="remove"]', field) || $("select[data-ifl-add]", field) || field).focus();
      return;
    }
    li.classList.remove("moved");
    void li.offsetWidth;
    li.classList.add("moved");
    const pos = $(".pos", li).textContent;
    announce(name + ", position " + pos);
    // Keep the focus on the button pressed, or its neighbor at an end.
    const same = $('[data-ifl="' + act + '"]', li);
    (same.disabled ? $('[data-ifl="' + (act === "up" ? "down" : "up") + '"]', li) : same).focus();
  });

  document.addEventListener("change", (e) => {
    const sel = e.target.closest("[data-iflist] select[data-ifl-add]");
    if (!sel || !sel.value) return;
    const field = sel.closest("[data-iflist]");
    const opt = sel.selectedOptions[0];
    $(".iflist", field).insertBefore(entry(field, opt.value, opt.dataset.state || "", opt.dataset.text || ""), $(".ifl-empty", field));
    const name = opt.value;
    opt.remove();
    sel.value = "";
    renumber(field);
    announce("Added " + name);
  });
})();

/* ------------------------------------------------- Remote clients page */
// Direct is the remote default only while Allow direct is on (design 0009
// D3.7); the server draws the same state without the script.
(() => {
  const { $ } = window.cuttle;
  const allow = $('.settings-form input[name="allow_direct"]');
  const direct = $('.settings-form input[name="default"][value="direct"]');
  if (!allow || !direct) return;
  const note = $(".settings-form .no-direct");
  const sync = () => {
    direct.disabled = !allow.checked && !direct.checked;
    if (note) note.hidden = allow.checked;
  };
  allow.addEventListener("change", sync);
  sync();
})();

/* --------------------------------------------------- Interfaces page */
// A row menu closes when its Delete… opens the review.
(() => {
  document.addEventListener("submit", (e) => {
    const menu = e.target.closest && e.target.closest("details.ifmenu");
    if (menu) menu.open = false;
  }, true);
})();
