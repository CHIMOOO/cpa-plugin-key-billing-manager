import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import test from "node:test";
import { parse } from "@babel/parser";
import { parse as parseHTML } from "parse5";

const root = new URL("../internal/plugin/", import.meta.url);
const en = JSON.parse(fs.readFileSync(new URL("locales/en.json", root)));
const zh = JSON.parse(fs.readFileSync(new URL("locales/zh-CN.json", root)));
const runtime = fs.readFileSync(new URL("i18n.js", root), "utf8");
const ui = fs.readFileSync(new URL("ui.html", root), "utf8");
const slots = (text) => [...text.matchAll(/\{([a-zA-Z][\w]*)\}/g)].map((match) => match[1]).sort();

test("catalog keys and interpolation arguments match", () => {
  assert.deepEqual(Object.keys(en).sort(), Object.keys(zh).sort());
  for (const [key, value] of Object.entries(en)) {
    assert.match(key, /^[a-z][a-z0-9_]*(?:\.[a-z0-9_]+)+$/, key);
    assert.ok(value.trim(), key);
    assert.ok(!/\p{Script=Han}/u.test(value), key);
    assert.deepEqual(slots(value), slots(zh[key]), key);
  }
});

test("visible HTML copy is localized, with English fallbacks matching the catalog", () => {
  const technicalLabels = new Set(["API Key", "Claude", "Antigravity", "Codex", "xAI", "Kimi", "CPA Key Billing", "Codex Turn State", "cpa-plugin-codex-turn-state"]);
  const visit = (node, translated = false) => {
    if (["script", "style", "svg"].includes(node.tagName)) return;
    const attrs = Object.fromEntries((node.attrs || []).map(attr => [attr.name, attr.value]));
    if (attrs["data-i18n"]) {
      translated = true;
      assert.ok(node.childNodes.every(child => child.nodeName === "#text"), "translation must not replace controls");
      assert.equal(node.childNodes.map(child => child.value).join("").trim(), en[attrs["data-i18n"]], attrs["data-i18n"]);
    }
    if (node.nodeName === "#text" && /[a-zA-Z]/.test(node.value) && !translated) {
      const text = node.value.trim();
      assert.ok(technicalLabels.has(text) || /^:\s*v\d+\.\d+\.\d+$/.test(text), `Untranslated HTML text: ${text}`);
    }
    for (const attr of ["title", "aria-label", "placeholder"]) {
      if (!attrs[attr]) continue;
      const key = attrs["data-i18n-" + attr];
      assert.ok(key, `Untranslated ${attr}: ${attrs[attr]}`);
      assert.equal(attrs[attr], en[key], key);
    }
    for (const child of node.childNodes || []) visit(child, translated);
  };
  visit(parseHTML(ui));
});

test("all literal UI message references exist and scripts parse", () => {
  for (const [, source] of ui.matchAll(/<script>\s*([\s\S]*?)<\/script>/g)) {
    const ast = parse(source);
    const walk = (node) => {
      if (!node || typeof node !== "object") return;
      if (node.type === "CallExpression" && node.callee.name === "m" && node.arguments[0]?.type === "StringLiteral")
        assert.ok(Object.hasOwn(en, node.arguments[0].value), node.arguments[0].value);
      for (const [key, child] of Object.entries(node)) {
        if (["loc", "extra", "comments"].includes(key)) continue;
        if (Array.isArray(child)) child.forEach(walk); else if (child && typeof child === "object") walk(child);
      }
    };
    walk(ast);
    assert.ok(!/\p{Script=Han}/u.test(source), "UI source must use catalog entries");
  }
  for (const [, key] of ui.matchAll(/data-i18n(?:-[\w-]+)?="([^"]+)"/g)) assert.ok(Object.hasOwn(en, key), key);
});

function environment(stored, hostLanguage, { crossOrigin = false, search = "" } = {}) {
  const listeners = {}, root = { lang: "" };
  const document = { documentElement: root, createTreeWalker: () => ({ currentNode: root, nextNode: () => false }), querySelectorAll: () => [] };
  const storage = new Map(stored ? [["cpa-key-billing:language", stored]] : []);
  const window = { addEventListener: (name, fn) => { listeners[name] = fn; }, dispatchEvent: () => {} };
  window.parent = hostLanguage ? { document: { documentElement: { lang: hostLanguage } } } : window;
  const posted = [];
  if (crossOrigin) window.parent = {
    get document() { throw new Error("Cross-origin access denied"); },
    postMessage: (data, origin) => posted.push({ data, origin })
  };
  const context = vm.createContext({ window, document, URL, URLSearchParams, NodeFilter: { SHOW_ELEMENT: 1, SHOW_TEXT: 4 },
    location: { search }, MutationObserver: class { observe() {} }, CustomEvent: class {},
    localStorage: { getItem: (key) => storage.get(key), setItem: (key, value) => storage.set(key, value) },
    BILLING_MESSAGES: { en, "zh-CN": zh } });
  vm.runInContext(runtime, context);
  return { i18n: window.billingI18n, window, listeners, posted, storage, evaluate: source => vm.runInContext(source, context), change: (language) => {
    storage.set("cpa-key-billing:language", language); listeners.storage({ key: "cpa-key-billing:language" });
  } };
}

test("English defaults, stored preference, and host precedence", () => {
  assert.equal(environment().i18n.current(), "en");
  assert.equal(environment("invalid").i18n.current(), "en");
  assert.equal(environment("zh-CN").i18n.current(), "zh-CN");
  assert.equal(environment("zh-CN", "en").i18n.current(), "en");
  assert.equal(environment("en", "zh-TW").i18n.current(), "zh-CN");
  assert.equal(environment("zh-CN", "ru").i18n.current(), "en");
});

test("cached messages and nested parameters translate without changing user data", () => {
  const { i18n, change } = environment();
  const userLabel = "<img src=x onerror=alert(1)> 自定义";
  const value = i18n.message("ui.failed_to_load_value_value", { v0: i18n.message("ui.subscription_plans"), v1: userLabel });
  assert.equal(String(value), `Subscription plans could not be loaded: ${userLabel}`);
  const error = new i18n.UIError(value);
  change("zh-CN");
  assert.equal(String(error.message), `订阅计划加载失败：${userLabel}`);
  assert.equal(i18n.serverMessage({ message_key: "unknown" }, "raw upstream text"), "raw upstream text");
  assert.equal(i18n.chartValue(value), String(value));
});

test("backend catalog and formatting metadata match the UI catalog", () => {
  const catalog = JSON.parse(fs.readFileSync(new URL("../internal/messages/catalog.json", import.meta.url)));
  for (const entry of Object.values(catalog)) {
    assert.ok(Object.hasOwn(en, entry.key), entry.key);
    assert.deepEqual(slots(en[entry.key]), entry.formats.map((_, index) => `v${index}`).sort(), entry.key);
  }
});

test("invalid server translation metadata falls back to the original message", () => {
  const { i18n } = environment();
  const key = "ui.upstream_value", fallback = "Original server diagnostic";
  for (const params of [null, [], "invalid", {}, { v0: null }, { v0: {} }, { v0: Infinity }]) {
    assert.equal(i18n.serverMessage({ message_key: key, message_params: params }, fallback), fallback);
  }
  const params = Object.create({ v0: "inherited value" });
  assert.equal(i18n.serverMessage({ message_key: key, message_params: params }, fallback), fallback);
  assert.equal(String(i18n.serverMessage({ message_key: key, message_params: { v0: "custom 模型" } }, fallback)), "Upstream: custom 模型");
  assert.equal(String(i18n.serverMessage({ message_key: "ui.no_email_provided", message_params: null }, fallback)), "No email provided");
});

test("display helpers retain translatable labels instead of freezing the current language", () => {
  const env = environment();
  const source = [...ui.matchAll(/<script>\s*([\s\S]*?)<\/script>/g)].at(-1)[1];
  const functions = parse(source).program.body.filter(node => node.type === "FunctionDeclaration" &&
    ["displayLabel", "matchesTerm"].includes(node.id.name));
  assert.equal(functions.length, 2);
  env.evaluate(`const DISPLAY_SEPARATOR = " · "; const m = window.billingI18n.message;\n` +
    functions.map(node => source.slice(node.start, node.end)).join("\n"));
  const label = env.evaluate('displayLabel(m("ui.no_email_provided"))');
  assert.ok(env.i18n.isMessage(label));
  assert.equal(String(label), "No email provided");
  assert.ok(env.evaluate('matchesTerm(m("ui.no_email_provided"), "email")'));
  env.change("zh-CN");
  assert.equal(String(label), "未提供邮箱");
  assert.ok(env.evaluate('matchesTerm(m("ui.no_email_provided"), "邮箱")'));
});

test("English durations use singular and plural forms", () => {
  const { i18n, change } = environment();
  assert.equal(String(i18n.message("time.duration_day", { count: 1 })), "1 day");
  assert.equal(String(i18n.message("time.duration_day", { count: 2 })), "2 days");
  assert.equal(String(i18n.message("time.last_hour", { count: 1 })), "Last 1 hour");
  assert.equal(String(i18n.message("quota.request_count", { count: 1, value: "1" })), "1 request");
  assert.equal(String(i18n.message("quota.request_count", { count: 1000, value: "1,000" })), "1,000 requests");
  change("zh-CN");
  assert.equal(String(i18n.message("time.duration_day", { count: 1 })), "1 天");
});

test("cross-origin language messages require the expected parent, origin, and version", () => {
  const env = environment("en", undefined, {
    crossOrigin: true, search: "?lang=zh-CN&parent_origin=https%3A%2F%2Fpanel.example"
  });
  assert.equal(env.i18n.current(), "zh-CN");
  assert.equal(env.posted[0].origin, "https://panel.example");
  assert.equal(env.posted[0].data.type, "cpa:plugin:ready");
  const valid = { source: env.window.parent, origin: "https://panel.example",
    data: { type: "cpa:plugin:locale", version: 1, language: "en" } };
  for (const event of [
    { ...valid, source: {} },
    { ...valid, origin: "https://untrusted.example" },
    { ...valid, data: { ...valid.data, version: 2 } },
    { ...valid, data: { ...valid.data, type: "unrelated" } },
    { ...valid, data: { ...valid.data, language: null } }
  ]) {
    env.listeners.message(event);
    assert.equal(env.i18n.current(), "zh-CN");
  }
  env.listeners.message(valid);
  assert.equal(env.i18n.current(), "en");
  env.listeners.message({ ...valid, data: { ...valid.data, language: "zh-TW" } });
  assert.equal(env.i18n.current(), "zh-CN");
  assert.equal(env.storage.get("cpa-key-billing:language"), "en", "host language must not overwrite the standalone preference");
  assert.equal(environment(undefined, undefined, { crossOrigin: true, search: "?parent_origin=data%3Atext%2Fhtml%2C" }).listeners.message, undefined);
});

test("date labels retained by open controls follow the current language", () => {
  const { i18n, change } = environment();
  const date = new Date("2026-09-18T11:12:13Z");
  const options = { year: "numeric", month: "long", timeZone: "UTC" };
  const label = i18n.date(date, options);
  assert.equal(String(label), new Intl.DateTimeFormat("en-US", options).format(date));
  change("zh-CN");
  assert.equal(String(label), new Intl.DateTimeFormat("zh-CN", options).format(date));
});
