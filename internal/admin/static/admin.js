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
  // earlier response is dropped.
  const pending = new WeakMap();

  class FragmentError extends Error {}

  async function loadFragment(target, url, init = {}) {
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
      if (!type.startsWith("text/html")) throw new FragmentError("not a fragment");
      const text = await res.text();
      const parsed = new DOMParser().parseFromString(text, "text/html");
      const root = parsed.body.firstElementChild;
      if (!root || !root.hasAttribute("data-block")) throw new FragmentError("not a fragment");
      // DOMParser never runs scripts; the fragment keeps none anyway.
      $$("script", root).forEach((s) => s.remove());
      const node = doc.adoptNode(root);
      target.replaceChildren(node);
      enhance(node);
      return { status: res.status, root: node };
    } finally {
      if (pending.get(target) === ctl) pending.delete(target);
    }
  }

  // failed shows the request-failed state of design 0009 D3.13 in the
  // target; retry runs the request again the way the page would without
  // the script.
  function failed(target, retry) {
    const again = el("button", { type: "button", class: "btn sm" }, "Try again");
    again.addEventListener("click", retry);
    const panel = el("div", { class: "failed", role: "alert", "data-block": "failed" },
      el("span", {}, "Could not reach cuttle. Check that it is running, then try again."), again);
    target.replaceChildren(panel);
  }

  // announceResult reads the first status of a loaded fragment aloud.
  function announceResult(root) {
    const status = $(".banner strong, [role=status], [role=alert]", root);
    if (status) announce(status.textContent.trim().slice(0, 240));
  }

  // A submit button with data-fragment="<id>" posts its form for the
  // block that replaces the children of the element with that id, or of
  // data-target. After a failed request the form submits normally.
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
    loadFragment(target, url, init).then((r) => {
      done();
      announceResult(r.root);
      const dlg = target.closest("dialog");
      if (dlg && !dlg.open) openDialog(dlg, sub);
    }, (err) => {
      done();
      if (err.name === "AbortError") return;
      form.dataset.plain = "1";
      failed(target, () => form.requestSubmit(sub));
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

    const snapshot = (form) => {
      const p = new URLSearchParams(new FormData(form));
      p.delete("csrf");
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
      // leaving lets a watched form's own submit leave the page.
      leaving(form) {
        if (watched.has(form)) leaving = true;
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
