"use strict";
(() => {
  const storageKey = "cpa-key-billing:language";
  const embedded = window.parent !== window;
  const bindings = new WeakMap();
  const normalize = (value) => /^zh(?:-|$)/i.test(String(value || "")) ? "zh-CN" : "en";
  const readStored = () => {
    try { return normalize(localStorage.getItem(storageKey)); } catch (_) { return "en"; }
  };
  let parentDocument = null;
  if (embedded) { try { parentDocument = window.parent.document; } catch (_) {} }
  const params = new URLSearchParams(location.search);
  let parentOrigin = "";
  try {
    const candidate = new URL(params.get("parent_origin"));
    if (["http:", "https:"].includes(candidate.protocol)) parentOrigin = candidate.origin;
  } catch (_) {}
  let language = embedded ? normalize(parentDocument?.documentElement.lang || params.get("lang")) : readStored();

  class Message {
    constructor(key, params = {}) { this.key = key; this.params = params; }
    toString() {
      const key = Number(this.params.count) === 1 && Object.hasOwn(BILLING_MESSAGES.en, this.key + "_one") ? this.key + "_one" : this.key;
      const template = BILLING_MESSAGES[language]?.[key] ?? BILLING_MESSAGES.en[key] ?? this.key;
      return template.replace(/\{([a-zA-Z][\w]*)\}/g, (_, key) => String(this.params[key] ?? `{${key}}`));
    }
  }

  class DateMessage extends Message {
    constructor(value, options) { super(""); this.value = value; this.options = options; }
    toString() { return new Intl.DateTimeFormat(language === "en" ? "en-US" : "zh-CN", this.options).format(new Date(this.value)); }
  }

  const message = (key, params) => new Message(key, params);
  function textNode(value) {
    const node = document.createTextNode(String(value ?? ""));
    if (value instanceof Message) bindings.set(node, value);
    return node;
  }
  function setText(node, value) {
    node.replaceChildren(textNode(value));
    return value;
  }
  function setAttribute(node, name, value) {
    let attributes = bindings.get(node);
    if (!attributes) bindings.set(node, attributes = new Map());
    if (value instanceof Message) attributes.set(name, value); else attributes.delete(name);
    node.setAttribute(name, String(value ?? ""));
    return value;
  }
  function refreshBindings() {
    const walker = document.createTreeWalker(document.documentElement, NodeFilter.SHOW_ELEMENT | NodeFilter.SHOW_TEXT);
    do {
      const node = walker.currentNode, value = bindings.get(node);
      if (value instanceof Message) node.nodeValue = String(value);
      else if (value) for (const [name, msg] of value) node.setAttribute(name, String(msg));
    } while (walker.nextNode());
    for (const select of document.querySelectorAll('[data-page-action="language"]')) select.value = language;
  }
  function apply(value, persist = false) {
    const next = normalize(value), changed = language !== next;
    language = next;
    document.documentElement.lang = next;
    if (persist && !embedded) { try { localStorage.setItem(storageKey, next); } catch (_) {} }
    refreshBindings();
    if (changed) window.dispatchEvent(new CustomEvent("billing-language-change", { detail: next }));
  }
  function initialize() {
    for (const node of document.querySelectorAll("[data-i18n]")) setText(node, message(node.dataset.i18n));
    for (const node of document.querySelectorAll("*")) {
      for (const attr of node.attributes) {
        if (attr.name.startsWith("data-i18n-")) setAttribute(node, attr.name.slice(10), message(attr.value));
      }
    }
    for (const group of document.querySelectorAll(".standalone-actions")) {
      const select = document.createElement("select");
      select.className = "language-select";
      select.dataset.pageAction = "language";
      setAttribute(select, "aria-label", message("language.select"));
      for (const [value, label] of [["en", "English"], ["zh-CN", "简体中文"]]) {
        const option = document.createElement("option");
        option.value = value; option.textContent = label; select.append(option);
      }
      select.value = language;
      select.addEventListener("change", () => apply(select.value, true));
      group.prepend(select);
    }
    refreshBindings();
  }
  class UIError extends Error {
    constructor(value) {
      super(String(value));
      Object.defineProperty(this, "message", { configurable: true, get: () => value });
    }
  }
  function serverMessage(value, fallback) {
    if (typeof value?.message_key !== "string" || !Object.hasOwn(BILLING_MESSAGES.en, value.message_key)) return fallback;
    const params = value.message_params ?? {};
    if (typeof params !== "object" || Array.isArray(params)) return fallback;
    // Incomplete metadata must fall back to the original diagnostic.
    for (const [, key] of BILLING_MESSAGES.en[value.message_key].matchAll(/\{([a-zA-Z][\w]*)\}/g)) {
      if (!Object.hasOwn(params, key) || !["string", "number", "boolean"].includes(typeof params[key])) return fallback;
      if (typeof params[key] === "number" && !Number.isFinite(params[key])) return fallback;
    }
    return message(value.message_key, params);
  }
  const boundText = (node) => bindings.get(node.firstChild) || node.textContent;
  const isMessage = (value) => value instanceof Message;
  function chartValue(value) {
    if (isMessage(value)) return String(value);
    if (Array.isArray(value)) return value.map(chartValue);
    if (typeof value === "function") return function (...args) { return chartValue(value.apply(this, args)); };
    if (value && Object.getPrototypeOf(value) === Object.prototype)
      return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, chartValue(item)]));
    return value;
  }
  document.documentElement.lang = language;
  if (parentDocument) {
    new MutationObserver(() => apply(parentDocument.documentElement.lang)).observe(parentDocument.documentElement, {
      attributes: true, attributeFilter: ["lang"]
    });
  }
  if (embedded && parentOrigin) {
    window.addEventListener("message", (event) => {
      if (event.source !== window.parent || event.origin !== parentOrigin || event.data?.type !== "cpa:plugin:locale" || event.data.version !== 1) return;
      if (typeof event.data.language === "string") apply(event.data.language);
    });
    window.parent.postMessage({ type: "cpa:plugin:ready", version: 1 }, parentOrigin);
  }
  if (!embedded) window.addEventListener("storage", (event) => {
    if (event.key === storageKey || event.key === null) apply(readStored());
  });
  window.billingI18n = { message, date: (value, options) => new DateMessage(value, options), setText, setAttribute, textNode, initialize, serverMessage, UIError, boundText, isMessage, chartValue,
    current: () => language, locale: () => language === "en" ? "en-US" : "zh-CN", normalize };
})();
