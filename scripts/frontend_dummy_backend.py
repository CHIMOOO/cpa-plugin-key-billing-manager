#!/usr/bin/env python3

import argparse
import base64
import hashlib
import json
import random
import re
import time
from datetime import datetime, timedelta, timezone
from decimal import Decimal
from zoneinfo import ZoneInfo
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse


ROOT = Path(__file__).resolve().parents[1]
UI_PATH = ROOT / "internal" / "plugin" / "ui.html"
API_BASE = "/v0/management/plugins/cpa-team-manager"
RESOURCE_BASE = "/v0/resource/plugins/cpa-team-manager"
NOW = datetime.now(timezone.utc).replace(minute=0, second=0, microsecond=0)
CALLER_SCOPE_SALT = b"cli-proxy-api:caller-scope:v1\0"


HOST_SHELL = r"""<!doctype html>
<html lang="en" data-host="__HOST_MODE__">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>__HOST_LABEL__ · API Key 团队管理样式预览</title>
<style>
:root{
  --bg-secondary:#faf9f5;--bg-primary:#f0eee8;--bg-tertiary:#e9e6df;--bg-hover:var(--bg-tertiary);
  --text-primary:#2d2a26;--text-secondary:#6d6760;--text-tertiary:#a29c95;
  --border-color:#e3e1db;--border-primary:#d5d2cb;--border-hover:#cecac4;
  --primary-color:#8b8680;--primary-hover:#7f7a74;--primary-active:#726d67;--primary-contrast:#fff;
  --success-badge-bg:#d1fae5;--success-badge-text:#065f46;--success-badge-border:#6ee7b7;
  --failure-badge-bg:#c6574624;--failure-badge-text:#8a3a30;--failure-badge-border:#c6574659;
  color-scheme:light;
}
:root[data-theme=white]{
  --bg-secondary:#fff;--bg-primary:#fff;--bg-tertiary:#f6f6f6;
  --border-color:#e5e5e5;--border-primary:#d9d9d9;--border-hover:#ccc;
}
:root[data-theme=dark]{
  --bg-secondary:#151412;--bg-primary:#1d1b18;--bg-tertiary:#262320;--bg-hover:#2e2a26;
  --text-primary:#f6f4f1;--text-secondary:#c9c3bb;--text-tertiary:#9c958d;
  --border-color:#3a3530;--border-primary:#4a453f;--border-hover:#5a544d;
  --primary-hover:#9a948e;--primary-active:#a6a099;
  --success-badge-bg:#064e3b4d;--success-badge-text:#6ee7b7;--success-badge-border:#059669;
  --failure-badge-bg:#c657463d;--failure-badge-text:#f1b0a6;--failure-badge-border:#c6574680;
  color-scheme:dark;
}
html[data-host=cpamp]{
  --app-bg:#eff2f7;--app-bg-gradient:linear-gradient(120deg,#f0f7ff 0%,#e7f2ff 50%,#edf7ff 100%);
  --app-surface:rgba(255,255,255,.94);--app-surface-strong:#fff;--app-surface-muted:rgba(255,255,255,.68);
  --app-border:rgba(15,23,42,.08);--app-border-strong:rgba(15,23,42,.12);
  --app-text-primary:#2c3e50;--app-text-regular:#5f6c7b;--app-text-muted:#8b95a6;
  --app-accent-soft:rgba(59,130,246,.12);--surface-subtle:#f6faff;
  --app-radius-lg:20px;--app-radius-md:12px;--app-radius-sm:8px;
  --glass-bg:#fff;--glass-border:rgba(255,255,255,.6);--glass-shadow:none;
  --app-input-bg:rgba(255,255,255,.62);--app-input-bg-focus:#fff;
  --app-input-border:var(--app-border-strong);--app-input-border-focus:#3b82f6;
  --color-primary:#3b82f6;--color-primary-light-3:#60a5fa;--color-primary-dark-2:#2563eb;
  --color-success:#22c55e;--success-color:var(--color-success);
  --primary-color:var(--color-primary);--primary-hover:var(--color-primary-light-3);
  --primary-active:var(--color-primary-dark-2);--primary-solid:#2563eb;--primary-solid-hover:#3b82f6;
  --primary-ring:rgba(59,130,246,.22);--primary-contrast:#fff;
  --color-warning:#f59e0b;--color-danger:#ef4444;
  --data-blue-base:#3b82f6;--data-green-base:#22c55e;--data-amber-base:#f59e0b;
  --data-red-base:#ef4444;--data-violet-base:#8b5cf6;--data-cyan-base:#06b6d4;
  --data-badge-success-bg:#f0fdf4;--data-badge-success-text:#16a34a;--data-badge-success-border:#bbf7d0;
  --data-badge-warning-bg:#fffbeb;--data-badge-warning-text:#d97706;--data-badge-warning-border:#fde68a;
  --data-badge-danger-bg:#fef2f2;--data-badge-danger-text:#dc2626;--data-badge-danger-border:#fecaca;
  --data-badge-info-bg:#eff6ff;--data-badge-info-text:#2563eb;--data-badge-info-border:#bfdbfe;
  --data-badge-neutral-bg:#f8fafc;--data-badge-neutral-text:#475569;--data-badge-neutral-border:#cbd5e1;
  --bg-secondary:var(--app-bg);--bg-primary:var(--app-surface);--bg-tertiary:var(--app-surface-muted);
  --bg-hover:var(--app-accent-soft);--text-primary:var(--app-text-primary);
  --text-secondary:var(--app-text-regular);--text-tertiary:var(--app-text-muted);
  --border-color:var(--app-border);--border-primary:var(--app-border-strong);
  --border-hover:rgba(59,130,246,.28);
}
html[data-host=cpamp][data-theme=dark]{
  --app-bg:#0a0a0a;--app-bg-gradient:linear-gradient(120deg,#0b1324 0%,#0a1426 50%,#091521 100%);
  --app-surface:rgba(24,28,40,.9);--app-surface-strong:#1b1f2a;--app-surface-muted:rgba(255,255,255,.08);
  --app-border:rgba(255,255,255,.08);--app-border-strong:rgba(255,255,255,.12);
  --app-text-primary:#e5e5e5;--app-text-regular:#a3a3a3;--app-text-muted:#7a7a7a;
  --app-accent-soft:rgba(96,165,250,.18);--surface-subtle:rgba(255,255,255,.06);
  --glass-bg:rgba(24,28,40,.72);--glass-border:rgba(255,255,255,.1);
  --app-input-bg:#1b1f2a;--app-input-bg-focus:#1b1f2a;--app-input-border-focus:#60a5fa;
  --color-primary:#60a5fa;--color-primary-light-3:#93c5fd;--color-primary-dark-2:#3b82f6;
  --color-success:#4ade80;--success-color:var(--color-success);
  --primary-solid:#60a5fa;--primary-solid-hover:#3b82f6;--primary-ring:rgba(96,165,250,.22);
  --data-blue-base:#60a5fa;--data-green-base:#4ade80;--data-amber-base:#fbbf24;
  --data-red-base:#f87171;--data-violet-base:#a78bfa;--data-cyan-base:#22d3ee;
  --data-badge-success-bg:rgba(74,222,128,.14);--data-badge-success-text:#4ade80;--data-badge-success-border:rgba(74,222,128,.24);
  --data-badge-warning-bg:rgba(251,191,36,.14);--data-badge-warning-text:#fbbf24;--data-badge-warning-border:rgba(251,191,36,.24);
  --data-badge-danger-bg:rgba(248,113,113,.14);--data-badge-danger-text:#f87171;--data-badge-danger-border:rgba(248,113,113,.24);
  --data-badge-info-bg:rgba(96,165,250,.14);--data-badge-info-text:#60a5fa;--data-badge-info-border:rgba(96,165,250,.24);
  --data-badge-neutral-bg:rgba(148,163,184,.12);--data-badge-neutral-text:#94a3b8;--data-badge-neutral-border:rgba(148,163,184,.2);
}
*{box-sizing:border-box}
html,body{width:100%;height:100%;margin:0;overflow:hidden}
body{font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;background:var(--bg-secondary);color:var(--text-primary)}
html[data-host=cpamp] body{background-color:var(--app-bg);background-image:var(--app-bg-gradient)}
.sidebar{position:fixed;inset:0 auto 0 0;z-index:20;width:var(--sidebar-width);padding:14px 10px;
  background:var(--bg-primary);border-right:1px solid var(--border-color)}
.brand{display:flex;align-items:center;gap:10px;height:48px;padding:0 10px;font-size:17px;font-weight:750}
.brand-mark{display:grid;place-items:center;width:34px;height:34px;border-radius:9px;background:var(--primary-color);color:#fff}
.nav-placeholder{display:grid;gap:8px;margin-top:30px}
.nav-placeholder span{height:38px;padding:9px 12px;border-radius:9px;color:var(--text-secondary)}
.nav-placeholder span.active{background:var(--bg-tertiary);color:var(--text-primary);font-weight:650}
.navbar{position:fixed;z-index:30;display:flex;align-items:center;justify-content:space-between;height:var(--header-height);
  color:var(--text-secondary)}
.navbar-left{display:flex;align-items:center;gap:10px;min-width:0}
.mobile-menu,.theme-controls button{appearance:none;display:grid;place-items:center;width:36px;height:36px;padding:0;
  border:1px solid transparent;border-radius:10px;background:transparent;color:inherit;font:inherit;cursor:pointer}
.theme-controls{display:flex;gap:3px;padding:5px;border:1px solid var(--border-color);border-radius:14px;background:var(--bg-primary)}
.theme-controls button:hover,.theme-controls button.active{background:var(--bg-tertiary);color:var(--text-primary)}
.content{position:fixed;overflow:hidden}
#plugin-frame{display:block;width:100%;height:100%;border:0;background:var(--bg-secondary)}
html[data-host=cpamc]{--sidebar-width:216px;--header-height:80px}
html[data-host=cpamc] .navbar{inset:0 0 auto var(--sidebar-width);pointer-events:none}
html[data-host=cpamc] .navbar-left{display:none}
html[data-host=cpamc] .navbar-left>span{display:none}
html[data-host=cpamc] .theme-controls{position:absolute;top:24px;right:24px;pointer-events:auto;box-shadow:0 18px 44px #0000002b}
html[data-host=cpamc] .mobile-menu{display:none}
html[data-host=cpamc] .content{inset:0 0 0 var(--sidebar-width)}
html[data-host=cpamp]{--sidebar-width:210px;--header-height:50px}
html[data-host=cpamp] .sidebar{background:color-mix(in srgb,var(--app-surface) 70%,transparent)}
html[data-host=cpamp] .navbar{inset:0 0 auto var(--sidebar-width);padding:0 20px 0 8px;background:var(--app-surface);border-bottom:1px solid var(--app-border)}
html[data-host=cpamp] .theme-controls{padding:2px;border:0;background:transparent}
html[data-host=cpamp] .theme-white{display:none}
html[data-host=cpamp] .content{inset:var(--header-height) 0 0 var(--sidebar-width)}
html[data-host=cpamp] #plugin-frame{background:var(--bg-primary)}
@media(max-width:768px){
  .sidebar{display:none}
  html[data-host] .navbar{left:0}
  html[data-host] .content{left:0}
  html[data-host=cpamc] .navbar-left{display:block;position:absolute;top:12px;left:12px;pointer-events:auto}
  html[data-host=cpamc] .mobile-menu{display:grid;background:var(--bg-primary);border-color:var(--border-color);box-shadow:0 18px 44px #0000002b}
  html[data-host=cpamc] .theme-controls{top:12px;right:12px}
  html[data-host=cpamp] .navbar{padding-right:8px}
}
</style>
</head>
<body>
<aside class="sidebar">
  <div class="brand"><span class="brand-mark">◈</span><span>__HOST_LABEL__</span></div>
  <div class="nav-placeholder"><span>仪表盘</span><span>AI 提供商</span><span>插件管理</span><span class="active">API Key 团队管理</span></div>
</aside>
<header class="navbar">
  <div class="navbar-left"><button class="mobile-menu" title="菜单">☰</button><span>API Key 团队管理</span></div>
  <div class="theme-controls" aria-label="预览主题">
    <select id="host-language" aria-label="Language"><option value="en">English</option><option value="zh-CN">简体中文</option><option value="zh-TW">繁體中文</option><option value="ru">Русский</option></select>
    <button type="button" data-action="refresh" title="刷新">↻</button>
    <button type="button" data-theme-choice="light" title="浅色主题">◐</button>
    <button type="button" class="theme-white" data-theme-choice="white" title="白色主题">○</button>
    <button type="button" data-theme-choice="dark" title="深色主题">●</button>
  </div>
</header>
<main class="content"><iframe id="plugin-frame" src="/ui" title="API Key 团队管理插件"></iframe></main>
<script>
"use strict";
const HOST_MODE="__HOST_MODE__";
const INITIAL_THEME="__INITIAL_THEME__";
const root=document.documentElement;
const frame=document.getElementById("plugin-frame");
const language=document.getElementById("host-language");
language.value=root.lang;
language.onchange=()=>{root.lang=language.value;};
const systemDark=()=>!!matchMedia("(prefers-color-scheme:dark)").matches;
let selectedTheme=INITIAL_THEME;

function resolveTheme(choice){
  if(choice==="auto")return systemDark()?"dark":"white";
  if(HOST_MODE==="cpamp"&&choice==="light")return "white";
  return choice;
}

function cpampBridgeCSS(theme){
  const computed=getComputedStyle(root);
  const declarations=[];
  for(let index=0;index<computed.length;index++){
    const name=computed.item(index);
    if(!name.startsWith("--"))continue;
    const value=computed.getPropertyValue(name).trim();
    if(value)declarations.push("  "+name+":"+value+";");
  }
  const scope=":where(html[data-cpamp-plugin-host='true'])";
  return scope+"{\n"+declarations.join("\n")+"\ncolor-scheme:"+(theme==="dark"?"dark":"light")+";min-height:100%;background:var(--bg-primary);color:var(--text-primary)}\n"+
    scope+" :where(body){min-height:100vh;margin:0;background:var(--bg-primary);color:var(--text-primary);font-family:Inter,-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;font-size:14px;line-height:1.5}\n"+
    scope+" :where(body,button,input,select,textarea){font-family:Inter,-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif}\n"+
    scope+" :where(input:not([type=checkbox]):not([type=radio]),select,textarea){min-height:34px;border:1px solid var(--app-input-border);border-radius:var(--app-radius-sm);background:var(--app-input-bg);color:var(--text-primary);box-shadow:none}\n"+
    scope+" :where(button,[role=button]){min-height:34px;border:1px solid var(--border-color);border-radius:var(--app-radius-md);background:var(--app-surface-muted);color:var(--text-primary);font-weight:600;line-height:1.2}\n"+
    scope+" :where(thead,th){background:color-mix(in srgb,var(--bg-tertiary) 72%,var(--bg-primary));color:var(--text-secondary)}\n"+
    scope+" :where(.card,[class*=card],[class*=panel]){border-color:var(--border-color);background:var(--bg-primary);color:var(--text-primary)}";
}

function syncCPAMPFrame(theme){
  if(HOST_MODE!=="cpamp")return;
  let doc;
  try{doc=frame.contentDocument}catch(_){return}
  if(!doc||!doc.documentElement||!doc.head)return;
  const childRoot=doc.documentElement;
  childRoot.setAttribute("data-cpamp-plugin-host","true");
  childRoot.setAttribute("data-theme",theme==="dark"?"dark":"white");
  childRoot.classList.toggle("theme-dark",theme==="dark");
  childRoot.classList.toggle("theme-light",theme!=="dark");
  let style=doc.getElementById("cpamp-dummy-host-style");
  if(!style){
    style=doc.createElement("style");
    style.id="cpamp-dummy-host-style";
    const pluginStyle=doc.head.querySelector("style,link[rel~=stylesheet]");
    doc.head.insertBefore(style,pluginStyle);
  }
  style.textContent=cpampBridgeCSS(theme);
}

function applyTheme(choice){
  selectedTheme=choice;
  const theme=resolveTheme(choice);
  const activeChoice=choice==="auto"?(HOST_MODE==="cpamp"&&theme==="white"?"light":theme):choice;
  if(theme==="light")root.removeAttribute("data-theme");else root.setAttribute("data-theme",theme);
  document.querySelectorAll("[data-theme-choice]").forEach(button=>{
    button.classList.toggle("active",button.dataset.themeChoice===activeChoice);
  });
  syncCPAMPFrame(theme);
}

document.querySelectorAll("[data-theme-choice]").forEach(button=>{
  button.addEventListener("click",()=>applyTheme(button.dataset.themeChoice));
});
document.querySelector("[data-action=refresh]").addEventListener("click",()=>frame.contentWindow.location.reload());
frame.addEventListener("load",()=>applyTheme(selectedTheme));
matchMedia("(prefers-color-scheme:dark)").addEventListener("change",()=>{
  if(selectedTheme==="auto")applyTheme("auto");
});
applyTheme(INITIAL_THEME);
</script>
</body>
</html>
"""


def iso(value):
    return value.isoformat().replace("+00:00", "Z")


PLANS = [
    {"id": "engineering", "name": "研发团队", "windows": [
        {"id": "short", "name": "短时额度", "amount_usd": 15, "request_limit": 100, "token_limit": 1000000, "period_seconds": 18000},
        {"id": "budget", "name": "团队预算", "amount_usd": 300, "period_seconds": 2592000},
    ]},
    {"id": "production", "name": "生产服务", "windows": [
        {"id": "short", "name": "峰值保护", "amount_usd": 0, "request_limit": 200, "token_limit": 0,
         "period_seconds": 7200, "cycle_anchor_at": iso(NOW + timedelta(hours=2))},
        {"id": "medium", "name": "服务额度", "amount_usd": 0, "request_limit": 0, "token_limit": 2000000,
         "period_seconds": 86400, "cycle_anchor_at": iso((NOW + timedelta(days=1)).replace(hour=0))},
        {"id": "budget", "name": "生产预算", "amount_usd": 1000,
         "period_seconds": 2592000, "cycle_anchor_at": iso((NOW + timedelta(days=15)).replace(hour=0))},
    ]},
    {"id": "project-credit", "name": "项目额度", "windows": [
        {"id": "budget", "name": "项目预算", "amount_usd": 100, "period_seconds": 864000},
    ]},
]


QUOTA_CYCLES = {}


def refresh_key_quota(key):
    plan = next((item for item in PLANS if item["id"] == key["plan_id"]), None)
    previous = QUOTA_CYCLES.get(key["scope"], {})
    cycles = {}
    now = datetime.now(timezone.utc)
    key.update(plan_name=plan["name"] if plan else "", unlimited=plan is None, blocked=False, partially_blocked=False, windows=[])
    key.pop("retry_at", None)
    for window in plan["windows"] if plan else []:
        cycle = previous.get(window["id"], {})
        started = (cycle.get("plan_id") == key["plan_id"]
                   and cycle.get("period_seconds") == window["period_seconds"]
                   and cycle.get("cycle_anchor_at") == window.get("cycle_anchor_at")
                   and (key.get("deleted_at") or datetime.fromisoformat(cycle["end_at"].replace("Z", "+00:00")) > now))
        if started:
            cycles[window["id"]] = cycle
        else:
            cycle = {}
        if not started and window.get("cycle_anchor_at"):
            anchor = datetime.fromisoformat(window["cycle_anchor_at"].replace("Z", "+00:00"))
            period = timedelta(seconds=window["period_seconds"])
            start = anchor + ((now - anchor) // period) * period
            cycle = dict(start_at=iso(start), end_at=iso(start+period))
            started = True
        dimensions = [dict(metric=metric, limit=limit, used=used, remaining=max(0, limit-used),
                           used_percent=min(100, used / limit * 100), blocked=used >= limit)
                      for metric, limit, used in [
                          ("amount_usd", window.get("amount_usd", 0), cycle.get("spent_usd", 0)),
                          ("tokens", window.get("token_limit", 0), cycle.get("used_tokens", 0)),
                          ("requests", window.get("request_limit", 0), cycle.get("used_requests", 0))] if limit > 0]
        view = dict(id=window["id"], name=window["name"], period_seconds=window["period_seconds"],
                    started=started, blocked=any(dimension["blocked"] for dimension in dimensions),
                    dimensions=dimensions)
        if window.get("scope"):
            view["scope"] = window["scope"]
        if window.get("cycle_anchor_at"):
            view["cycle_anchor_at"] = window["cycle_anchor_at"]
        if started:
            view.update(start_at=cycle["start_at"], end_at=cycle["end_at"])
        if view["blocked"]:
            if window.get("scope"):
                key["partially_blocked"] = True
            else:
                key["blocked"] = True
                key["retry_at"] = max(key.get("retry_at", ""), view["end_at"])
        key["windows"].append(view)
    QUOTA_CYCLES[key["scope"]] = cycles


AUTOMATION_CREDENTIAL_REF = "sha256:" + "f" * 64
UNAVAILABLE_CREDENTIAL_REF = "sha256:" + "1" * 64

CREDENTIALS = [
    {"ref": "sha256:" + "a" * 64, "source": "auth-files", "provider": "codex", "display_name": "dev-team@example.com", "status": "active", "disabled": False, "unavailable": False},
    {"ref": "sha256:" + "b" * 64, "source": "auth-files", "provider": "claude", "display_name": "platform@example.com", "status": "active", "disabled": False, "unavailable": False},
    {"ref": "sha256:" + "c" * 64, "source": "ai-providers", "provider": "codex", "display_name": "sk-proxy…7f3a", "status": "active", "disabled": False, "unavailable": False},
    {"ref": "sha256:" + "d" * 64, "source": "ai-providers", "provider": "deepseek", "display_name": "sk-live…91b2", "status": "disabled", "disabled": True, "unavailable": False},
    {"ref": "sha256:" + "e" * 64, "source": "auth-files", "provider": "xai", "display_name": "disabled@example.com", "status": "disabled", "disabled": True, "unavailable": False},
    {"ref": AUTOMATION_CREDENTIAL_REF, "source": "auth-files", "provider": "codex", "display_name": "automation@example.com", "status": "active", "disabled": False, "unavailable": False},
    {"ref": UNAVAILABLE_CREDENTIAL_REF, "source": "auth-files", "provider": "kimi", "display_name": "research@example.com", "status": "error", "disabled": False, "unavailable": True},
    {"ref": "sha256:" + "2" * 64, "source": "auth-files", "provider": "codex", "display_name": "paused-codex@example.com", "status": "disabled", "disabled": True, "unavailable": False},
]
SYNCED_CREDENTIAL_REFS = set()

ROUTES = [
    {"id": "coding", "name": "代码开发", "rule": {"models": ["gpt-5.6-sol", "gpt-5.5", "codex/deepseek-v4-flash-vision-exp"], "credential_ids": [], "credential_providers": [{"source": "auth-files", "provider": "codex"}]}},
    {"id": "analytics", "name": "数据分析", "rule": {"models": ["claude/deepseek-v4-pro", "claude/deepseek-v4-flash"], "credential_ids": ["sha256:" + "b" * 64], "credential_providers": []}},
    {"id": "economy", "name": "轻量任务", "rule": {"models": ["gpt-5.6-luna"], "credential_ids": [], "credential_providers": [{"source": "auth-files", "provider": "codex"}]}},
    {
        "id": "ci",
        "name": "持续集成",
        "rule": {
            "models": ["gpt-5.6-luna", "gpt-5.6-terra"],
            "credential_ids": [],
            "credential_providers": [{"source": "auth-files", "provider": "codex"}],
            "denied_models": ["gpt-5.6-sol"],
            "denied_credential_ids": ["sha256:" + "a" * 64],
            "denied_credential_providers": [{"source": "ai-providers", "provider": "deepseek"}],
        },
    },
    {
        "id": "text-only",
        "name": "文本服务",
        "rule": {
            "models": [], "credential_ids": [], "credential_providers": [],
            "denied_models": ["gpt-image-2"],
            "denied_credential_ids": ["sha256:" + "d" * 64, "sha256:" + "e" * 64],
            "denied_credential_providers": [],
        },
    },
]

AUTH_FILES = [
    {
        "auth_index": "auth-demo-codex-plus",
        "name": "codex-dev-team@example.com.json",
        "category": "codex",
        "email": "dev-team@example.com",
        "disabled": False,
        "unavailable": False,
        "quota_supported": True,
    },
    {
        "auth_index": "auth-demo-claude",
        "name": "claude-platform@example.com.json",
        "category": "claude",
        "email": "platform@example.com",
        "disabled": False,
        "unavailable": False,
        "quota_supported": True,
    },
    {
        "auth_index": "auth-demo-codex-pro",
        "name": "codex-automation@example.com.json",
        "category": "codex",
        "email": "automation@example.com",
        "disabled": False,
        "unavailable": False,
        "quota_supported": True,
    },
    {
        "auth_index": "auth-demo-antigravity",
        "name": "antigravity-ai-lab@example.com.json",
        "category": "antigravity",
        "email": "ai-lab@example.com",
        "disabled": False,
        "unavailable": False,
        "quota_supported": True,
    },
    {
        "auth_index": "auth-demo-kimi",
        "name": "kimi-research@example.com.json",
        "category": "kimi",
        "email": "research@example.com",
        "disabled": False,
        "unavailable": True,
        "quota_supported": True,
    },
    {
        "auth_index": "auth-demo-xai-active",
        "name": "xai-research@example.com.json",
        "category": "xai",
        "email": "research@example.com",
        "disabled": False,
        "unavailable": False,
        "quota_supported": True,
    },
]

AUTH_FILE_CREDENTIAL_REFS = {
    "auth-demo-codex-plus": "sha256:" + "a" * 64,
    "auth-demo-claude": "sha256:" + "b" * 64,
    "auth-demo-codex-pro": AUTOMATION_CREDENTIAL_REF,
    "auth-demo-kimi": UNAVAILABLE_CREDENTIAL_REF,
    "auth-demo-xai-active": "sha256:" + "e" * 64,
}

for auth_file in AUTH_FILES:
    auth_file["credential_ref"] = AUTH_FILE_CREDENTIAL_REFS.get(auth_file["auth_index"], "sha256:" + "9" * 64)
    auth_file["cache_revision"] = iso(NOW - timedelta(minutes=5))

AUTH_CATEGORY_ORDER = {"claude": 0, "antigravity": 1, "codex": 2, "xai": 3, "kimi": 4}
AUTH_FILES.sort(
    key=lambda item: (
        AUTH_CATEGORY_ORDER.get(item["category"], 5),
        item["category"].lower(),
        item["name"].lower(),
        item["auth_index"],
    )
)


def quota_row(label, remaining_percent, reset_seconds, **extra):
    return {
        "label": label,
        "remaining_percent": remaining_percent,
        "reset_at": iso(NOW + timedelta(seconds=reset_seconds)),
        **extra,
    }


AUTH_FILE_QUOTAS = {
    "auth-demo-codex-pro": {
        "plan": "pro-20x",
        "rate_limit_reset_credits_available_count": 1,
        "quota": [
            quota_row("周限额", 62, 432000, scope="account", window_seconds=604800),
            quota_row(
                "GPT-5.3-Codex-Spark 5 小时限额",
                100,
                18000, scope="spark", window_seconds=18000,
            ),
            quota_row(
                "GPT-5.3-Codex-Spark 周限额",
                100,
                604800, scope="spark", window_seconds=604800,
            ),
        ],
    },
    "auth-demo-codex-plus": {
        "plan": "plus",
        "rate_limit_reset_credits_available_count": 1,
        "quota": [
            quota_row("5 小时限额", 35, 14400, scope="account", window_seconds=18000),
            quota_row("周限额", 90, 518400, scope="account", window_seconds=604800),
        ],
    },
    "auth-demo-claude": {
        "plan": "Team",
        "quota": [
            quota_row("5 小时限额", 76, 12600, scope="account", window_seconds=18000),
            quota_row("周限额", 59, 388800, scope="account", window_seconds=604800),
            {
                "label": "额外用量",
                "used": 12.5,
                "limit": 100,
                "remaining_percent": 87.5,
                "currency": "USD",
            },
        ],
    },
    "auth-demo-antigravity": {
        "plan": "Google AI Pro",
        "quota": [
            quota_row(
                "5 小时限额",
                82,
                64800,
                group_label="Gemini Models",
            ),
            quota_row(
                "周限额",
                93,
                64800,
                group_label="Gemini Models",
            ),
        ],
    },
    "auth-demo-kimi": {
        "quota": [
            quota_row("5 小时限额", 48, 7200),
            quota_row("周限额", 69, 345600),
        ],
    },
    "auth-demo-xai-active": {
        "quota": [
            quota_row("周限额", 78, 410400),
            {
                "label": "月度额度",
                "used": 8.5,
                "limit": 50,
                "remaining_percent": 83,
                "currency": "USD",
                "reset_at": iso(NOW + timedelta(seconds=1814400)),
            },
        ],
    },
}


def auth_file_quota(query):
    auth_index = query.get("auth_index", [""])[0]
    quota = AUTH_FILE_QUOTAS.get(auth_index)
    if quota is None:
        return None
    auth_file = next(item for item in AUTH_FILES if item["auth_index"] == auth_index)
    result = {
        "auth_revision": auth_file["cache_revision"],
        "fetched_at": iso(NOW),
        **quota,
    }
    english = json.loads((UI_PATH.parent / "locales/en.json").read_text(encoding="utf-8"))
    chinese = json.loads((UI_PATH.parent / "locales/zh-CN.json").read_text(encoding="utf-8"))
    labels = {value: key for key, value in chinese.items() if key.startswith("backend.") and "{" not in value}
    result["quota"] = [dict(row) for row in result["quota"]]
    for row in result["quota"]:
        key = labels.get(row.get("label", ""))
        if key:
            row["label"] = english[key]
            row["label_message"] = {"message_key": key}
    return result


KEY_PROFILES = [
    {"label": "代码审查机器人", "plan_id": "engineering", "spent_usd": 128.64, "concurrency_limit": 5, "current_concurrency": 2,
     "route_bindings": {"route_ids": ["coding", "analytics", "economy", "text-only"], "models": [], "credential_ids": [], "credential_providers": []}},
    {"label": "CI 构建服务", "plan_id": "engineering", "spent_usd": 84.27, "concurrency_limit": 10, "current_concurrency": 3,
     "route_bindings": {"route_ids": ["ci"], "models": ["gpt-5.6-terra", "gpt-5.5"],
                        "credential_ids": [AUTOMATION_CREDENTIAL_REF], "credential_providers": [],
                        "denied_models": ["gpt-5.6-sol", "gpt-image-2"], "denied_credential_ids": ["sha256:" + "a" * 64], "denied_credential_providers": []}},
    {"label": "数据分析平台", "plan_id": "production", "spent_usd": 368.91, "concurrency_limit": 5, "current_concurrency": 1,
     "route_bindings": {"route_ids": ["analytics"], "models": [], "credential_ids": [], "credential_providers": []}},
    {"label": "客服助手", "plan_id": "production", "spent_usd": 241.36, "concurrency_limit": 8, "current_concurrency": 2,
     "route_bindings": {"route_ids": ["analytics"], "models": ["gpt-5.5"], "denied_models": ["gpt-image-2"], "credential_ids": [], "credential_providers": []}},
    {"label": "文档生成", "plan_id": "engineering", "spent_usd": 56.48, "concurrency_limit": 3, "current_concurrency": 0,
     "route_bindings": {"route_ids": ["text-only"], "models": [], "credential_ids": [], "credential_providers": []}},
    {"label": "预发布环境", "plan_id": "project-credit", "spent_usd": 43.72, "concurrency_limit": 2, "current_concurrency": 1,
     "route_bindings": {"route_ids": ["economy"], "models": [], "credential_ids": [], "credential_providers": []}},
    {"label": "内部工具", "plan_id": "", "spent_usd": 0, "concurrency_limit": 0, "current_concurrency": 1,
     "route_bindings": {"route_ids": [], "models": [], "credential_ids": [], "credential_providers": []}},
    {"label": "临时测试", "plan_id": "project-credit", "spent_usd": 87.19, "concurrency_limit": 1, "current_concurrency": 0,
     "route_bindings": {"route_ids": [], "models": ["gpt-5.5"], "credential_ids": ["sha256:" + "c" * 64], "credential_providers": []}},
]


def make_key(index):
    profile = KEY_PROFILES[index - 1]
    plan = next((item for item in PLANS if item["id"] == profile["plan_id"]), None)
    result = {
        "scope": hashlib.sha256(CALLER_SCOPE_SALT + f"sk-demo-{index:04d}".encode()).hexdigest(),
        "preview": f"sk-…{index:03d}",
        "label": profile["label"],
        "in_config": True,
        "plan_id": profile["plan_id"],
        "concurrency_limit": profile["concurrency_limit"],
        "current_concurrency": profile["current_concurrency"],
        "route_bindings": profile["route_bindings"],
        "group_ids": ["engineering-group"] if index <= 2 else ["production-group"] if index <= 4 else ["docs-group"] if index == 5 else [],
    }
    cycles = {}
    for position, window in enumerate(plan["windows"] if plan and index != 4 else []):
        ratio = profile["spent_usd"] / plan["windows"][-1]["amount_usd"]
        if index == 2 and position == 0 or index == 3 and position < 2:
            ratio = 1.05
        end = (datetime.fromisoformat(window["cycle_anchor_at"].replace("Z", "+00:00"))
               if window.get("cycle_anchor_at") else NOW + timedelta(seconds=window["period_seconds"] * (0.3 + index * 0.04)))
        cycles[window["id"]] = dict(plan_id=result["plan_id"], period_seconds=window["period_seconds"],
            cycle_anchor_at=window.get("cycle_anchor_at"),
            spent_usd=window["amount_usd"]*ratio,
            used_requests=int(window.get("request_limit", 1000)*ratio),
            used_tokens=int(window.get("token_limit", 10000000)*ratio),
            start_at=iso(end-timedelta(seconds=window["period_seconds"])), end_at=iso(end))
    QUOTA_CYCLES[result["scope"]] = cycles
    refresh_key_quota(result)
    return result


KEYS = [make_key(index) for index in range(1, len(KEY_PROFILES) + 1)]
KEYS[-2]["in_config"] = False
KEYS[-2]["deleted_at"] = iso(NOW - timedelta(days=2))
KEYS[-1]["in_config"] = False
KEYS[-1]["deleted_at"] = iso(NOW - timedelta(days=1))
KEYS[-1]["route_bindings"]["route_ids"] = ["economy"]
LIVE_KEYS = [key for key in KEYS if not key.get("deleted_at")]
ACCESS_CONTROL = {"enabled": True, "deny_ungrouped": False}

TURN_STATE_CONFIG = {
    "enabled": False, "force_astra": False, "inject_mode": "replace-only", "dry_run": True, "learn_responses": True,
    "template_length": 292, "replace_length": 312, "ttl_seconds": 3600, "renew_before_minutes": 0,
    "probe_drop_failed_proxies": False, "probe_drop_degraded_proxies": False, "probe_min_proxies": 10,
    "probe_verify_completion": False, "probe_hourly_limit": 0,
    "probe_static_cooldown_minutes": 55, "probe_rotating_cooldown_minutes": 10,
    "probe_account_cooldown_minutes": 10, "probe_rotating_max_attempts": 10,
    "models": ["gpt-6-astra", "gpt-5.6-sol"], "probe_accounts": [], "probe_proxies": [], "probe_proxies_rotating": [],
}
TURN_STATE_TEMPLATES = []
TURN_STATE_OBSERVATIONS = {"since": iso(NOW), "buckets": [], "events": []}
TURN_STATE_BUDGET_ATTEMPTS = []
TURN_STATE_COUNTERS = {"injected": 0, "learned": 0, "passed": 0}
TURN_STATE_LAST = {}
TURN_STATE_UPLOADS = {}
TURN_STATE_PROGRESS = {}
TURN_STATE_PROBE_STATS = {"attempts": 0, "harvested": 0, "degraded": 0, "failed": 0, "unchanged": 0}
TURN_STATE_WEBSOCKETS = {}
TURN_STATE_RUNNER = {"enabled": False, "revision": 1, "phase": "stopped", "online": True,
                     "instance_id": "dummy-collector", "epoch": "dummy-runner-epoch", "version": "development",
                     "last_seen_at": iso(datetime.now(timezone.utc)), "lease_expires_at": iso(datetime.now(timezone.utc) + timedelta(minutes=1)),
                     "next_check_at": "", "in_flight": False, "events": []}


def ui_message(key):
    english = json.loads((UI_PATH.parent / "locales/en.json").read_text(encoding="utf-8"))
    return {"message": english[key], "message_key": key}


def turn_state_budget():
    now = time.time()
    TURN_STATE_BUDGET_ATTEMPTS[:] = [at for at in TURN_STATE_BUDGET_ATTEMPTS if at > now - 3600]
    limit, used = TURN_STATE_CONFIG.get("probe_hourly_limit", 0), len(TURN_STATE_BUDGET_ATTEMPTS)
    exhausted = bool(limit and used >= limit)
    resume = TURN_STATE_BUDGET_ATTEMPTS[used - limit] + 3600 if exhausted else 0
    return {"limit": limit, "used": used, "remaining": max(0, limit - used) if limit else 0,
            "exhausted": exhausted, "resumes_at": iso(datetime.fromtimestamp(resume, timezone.utc)) if resume else "0001-01-01T00:00:00Z"}


def dummy_template_fingerprint(template):
    return hashlib.sha256(json.dumps([template["account"], template["model"], template.get("issued_at", template["expires_at"])], separators=(",", ":")).encode()).hexdigest()


def dummy_state_auth_index(credential):
    return next((file["auth_index"] for file in AUTH_FILES if file["credential_ref"] == credential["ref"]), "state-" + credential["ref"])


def dummy_state_credential_ref(account_id):
    return "sha256:" + hashlib.sha256(("cpa-key-billing:credential:v1\0" + account_id).encode()).hexdigest()


def turn_state_view():
    config = dict(TURN_STATE_CONFIG)
    for field in ("probe_proxies", "probe_proxies_rotating"):
        config[field] = []
    now = datetime.now(timezone.utc)
    templates = [dict(item, fingerprint=dummy_template_fingerprint(item), remaining_seconds=max(0, int((datetime.fromisoformat(item["expires_at"].replace("Z", "+00:00")) - now).total_seconds())))
                 for item in TURN_STATE_TEMPLATES if datetime.fromisoformat(item["expires_at"].replace("Z", "+00:00")) > now]
    return {"config": config, "templates": templates, "counters": TURN_STATE_COUNTERS,
            "server_time": iso(now), "observations": TURN_STATE_OBSERVATIONS,
            "proxy_config_revision": turn_state_revision(),
            "renewal_lead_seconds": config["renew_before_minutes"] * 60 or min(config["ttl_seconds"] // 4, 300),
            "probe_budget": turn_state_budget(),
            "last_decision": {}, "last_probe": TURN_STATE_LAST, "probe_stats": TURN_STATE_PROBE_STATS, "runner": dict(TURN_STATE_RUNNER),
            "probe_progress": {"active": bool(TURN_STATE_PROGRESS), "result": dict(TURN_STATE_PROGRESS)},
            "probe_supported": True, "probe_unavailable_reason": "",
            "upstream_websocket_management_supported": True,
            "upstream_websocket_patch_path": "/v0/management/auth-files/fields",
            "host_requirement": ui_message("backend.turn_state_host_requirement")["message"],
            "host_requirement_message": {"message_key": "backend.turn_state_host_requirement"},
            "probe_accounts": [{"account": item["ref"], "label": item["display_name"],
                                "auth_index": dummy_state_auth_index(item), "upstream_transport_known": True,
                                "upstream_websockets": TURN_STATE_WEBSOCKETS.get(item["ref"], False),
                                "disable_websockets_patch": {"name": item["ref"], "websockets": False},
                                "disabled": bool(item.get("disabled")) or item.get("status", "").lower() == "disabled"}
                               for item in CREDENTIALS if item["provider"] == "codex" and item["source"] == "auth-files"],
            "proxy_counts": {"static": len(TURN_STATE_CONFIG["probe_proxies"]),
                             "rotating": len(TURN_STATE_CONFIG["probe_proxies_rotating"])}}


def turn_state_revision():
    return hashlib.sha256(json.dumps(TURN_STATE_CONFIG, sort_keys=True).encode()).hexdigest()


def valid_turn_state_pruning(config):
    for field in ("probe_drop_failed_proxies", "probe_drop_degraded_proxies", "probe_verify_completion"):
        if field in config and type(config[field]) is not bool:
            return False
    limit = config.get("probe_hourly_limit", TURN_STATE_CONFIG["probe_hourly_limit"])
    if type(limit) is not int or not 0 <= limit <= 10000:
        return False
    for field, maximum in (("probe_static_cooldown_minutes", 1440),
                           ("probe_rotating_cooldown_minutes", 1440),
                           ("probe_account_cooldown_minutes", 1440),
                           ("probe_rotating_max_attempts", 100)):
        value = config.get(field, TURN_STATE_CONFIG[field])
        if type(value) is not int or not 0 <= value <= maximum:
            return False
    minimum = config.get("probe_min_proxies", TURN_STATE_CONFIG["probe_min_proxies"])
    return type(minimum) is int and 1 <= minimum <= 40000


def masked_dummy_proxy(value):
    parsed = urlparse(value)
    if not parsed.hostname:
        return "(direct)" if not value else "invalid"
    return f"{parsed.scheme}://{'***@' if parsed.username or parsed.password else ''}{parsed.hostname}:{parsed.port or (443 if parsed.scheme == 'https' else 80)}"

ROUTE_RULE_FIELDS = ("models", "credential_ids", "credential_providers", "denied_models", "denied_credential_ids", "denied_credential_providers")


def empty_rule():
    return {field: [] for field in ROUTE_RULE_FIELDS}


GROUPS = [
    {"id": "engineering-group", "name": "研发团队", "route_ids": ["coding", "ci"], "rule": empty_rule()},
    {"id": "production-group", "name": "生产服务", "route_ids": ["analytics"], "rule": empty_rule()},
    {
        "id": "docs-group",
        "name": "文档服务",
        "route_ids": [],
        "rule": {
            **empty_rule(),
            "models": ["gpt-5.5"],
            "credential_ids": ["sha256:" + "c" * 64],
            "credential_providers": [{"source": "auth-files", "provider": "claude"}],
            "denied_models": ["gpt-image-2"],
        },
    },
]

PRICES = [
    {
        "model_id": "gpt-5.6-sol",
        "input_per_1m": 4,
        "output_per_1m": 20,
        "cache_read_per_1m": 0.4,
        "source": "custom",
        "long_context": {
            "threshold_input_tokens": 272000,
            "input_per_1m": 8,
            "output_per_1m": 30,
            "cache_read_per_1m": 0.8,
        },
    },
    {
        "model_id": "gpt-5.5",
        "input_per_1m": 5,
        "output_per_1m": 30,
        "cache_read_per_1m": 0.5,
        "source": "custom",
        "long_context": {
            "threshold_input_tokens": 272000,
            "input_per_1m": 10,
            "output_per_1m": 45,
            "cache_read_per_1m": 1,
        },
    },
    {
        "model_id": "gpt-5.6-luna",
        "input_per_1m": 0.2,
        "output_per_1m": 1.2,
        "cache_read_per_1m": 0.02,
        "source": "custom",
    },
    {
        "model_id": "gpt-5.6-terra",
        "input_per_1m": 2,
        "output_per_1m": 12,
        "cache_read_per_1m": 0.2,
        "source": "custom",
    },
    {
        "model_id": "gpt-image-2",
        "input_per_1m": 5,
        "output_per_1m": 30,
        "cache_read_per_1m": 1.25,
        "source": "custom",
    },
    {
        "model_id": "claude/deepseek-v4-pro",
        "input_per_1m": 0.435,
        "output_per_1m": 0.87,
        "cache_read_per_1m": 0.003625,
        "source": "custom",
    },
    {
        "model_id": "claude/deepseek-v4-flash",
        "input_per_1m": 0.28,
        "output_per_1m": 0.42,
        "source": "custom",
    },
]

def make_cost(uncached, cache_read, cache_write, output, rates, tiered=False, long_context=False, multiplier=1):
    input_price, read_price, write_price, output_price = (rate * multiplier for rate in rates)
    parts = {
        "uncached_input_usd": uncached * input_price / 1_000_000,
        "cache_read_usd": cache_read * read_price / 1_000_000,
        "cache_write_usd": cache_write * write_price / 1_000_000,
        "output_usd": output * output_price / 1_000_000,
    }
    return {
        **parts,
        "multiplier": multiplier,
        "total_usd": sum(parts.values()),
        "uncached_input_tokens": uncached,
        "cache_read_tokens": cache_read,
        "cache_write_tokens": cache_write,
        "billed_output_tokens": output,
        "tiered": tiered,
        "long_context": long_context,
        "threshold_input_tokens": 272000 if tiered else 0,
        "applied_input_per_1m": input_price,
        "applied_cache_read_per_1m": read_price,
        "applied_cache_write_per_1m": write_price,
        "applied_output_per_1m": output_price,
    }


def event_sample(
    key_index,
    source,
    provider,
    model,
    executor,
    effort,
    tier,
    latency_ms,
    ttft_ms,
    reasoning_tokens,
    tokens,
    rates,
    *,
    billing_model="",
    long_context=False,
    failed=False,
    multiplier=1,
):
    uncached, cache_read, cache_write, output = tokens
    if failed:
        uncached = cache_read = cache_write = output = 0
    return {
        "key_index": key_index,
        "source": source,
        "provider": provider,
        "executor_type": executor,
        "reasoning_effort": effort,
        "service_tier": tier,
        "upstream_model": model,
        "billing_model": billing_model or model,
        "failed": failed,
        "latency_ms": latency_ms,
        "ttft_ms": ttft_ms,
        "accounting_quality": "" if failed else "complete",
        "price_source": "custom",
        "cost": make_cost(
            uncached,
            cache_read,
            cache_write,
            output,
            rates,
            model.startswith("gpt-5."),
            long_context,
            multiplier if not failed else 1,
        ),
        "reasoning_tokens": 0 if failed else reasoning_tokens,
    }


# Numeric usage and timing values are sampled from a real export. All identities
# below are synthetic and intentionally unrelated to the source records.
SUCCESS_EVENT_SAMPLES = [
    event_sample(0, "codex · dev-team@example.com", "codex", "gpt-5.6-sol", "CodexExecutor", "high", "priority", 11513, 8209, 266, (712, 91648, 4096, 425), (4, 0.4, 5, 20), multiplier=2.5),
    event_sample(5, "codex · dev-team@example.com", "codex", "gpt-5.6-luna", "CodexWebsocketsExecutor", "low", "auto", 2516, 1431, 10, (1030, 49920, 0, 75), (0.2, 0.02, 0.25, 1.2)),
    event_sample(1, "codex · dev-team@example.com", "codex", "gpt-5.5", "CodexWebsocketsExecutor", "medium", "auto", 2417, 1103, 0, (1194, 95616, 0, 73), (5, 0.5, 5, 30)),
    event_sample(1, "codex · dev-team@example.com", "codex", "gpt-5.6-sol", "CodexWebsocketsExecutor", "high", "auto", 5306, 2121, 21, (798, 169984, 0, 201), (4, 0.4, 5, 20)),
    event_sample(2, "claude · platform@example.com", "claude", "deepseek-v4-pro", "ClaudeExecutor", "high", "auto", 10061, 637, 0, (38049, 0, 0, 474), (0.435, 0.003625, 0.435, 0.87), billing_model="claude/deepseek-v4-pro"),
    event_sample(7, "codex · sk-proxy…7f3a", "codex", "gpt-5.5", "CodexWebsocketsExecutor", "high", "auto", 7288, 4130, 188, (8502, 45440, 0, 309), (5, 0.5, 5, 30)),
    event_sample(6, "codex · dev-team@example.com", "codex", "gpt-5.6-terra", "CodexWebsocketsExecutor", "medium", "auto", 10120, 3379, 81, (1300, 69376, 0, 477), (2, 0.2, 2.5, 12)),
    event_sample(4, "codex · dev-team@example.com", "codex", "gpt-5.6-sol", "CodexWebsocketsExecutor", "high", "auto", 2609, 1881, 6, (1368, 237824, 0, 46), (4, 0.4, 5, 20)),
    event_sample(5, "codex · dev-team@example.com", "codex", "gpt-5.6-luna", "CodexWebsocketsExecutor", "low", "auto", 5002, 2464, 61, (1261, 149248, 0, 147), (0.2, 0.02, 0.25, 1.2)),
    event_sample(7, "codex · sk-proxy…7f3a", "codex", "gpt-5.5", "CodexWebsocketsExecutor", "high", "auto", 7634, 4412, 94, (24196, 19840, 0, 275), (5, 0.5, 5, 30)),
    event_sample(6, "codex · dev-team@example.com", "codex", "gpt-image-2", "CodexExecutor", "", "auto", 43679, 43476, 0, (1658, 0, 0, 915), (5, 1.25, 5, 30)),
    event_sample(3, "claude · platform@example.com", "claude", "deepseek-v4-pro", "ClaudeExecutor", "high", "auto", 16431, 909, 0, (66079, 32768, 0, 535), (0.435, 0.003625, 0.435, 0.87), billing_model="claude/deepseek-v4-pro"),
    event_sample(0, "codex · dev-team@example.com", "codex", "gpt-5.6-sol", "CodexExecutor", "high", "auto", 19542, 17836, 681, (835, 282752, 0, 802), (8, 0.8, 10, 30), long_context=True),
    event_sample(6, "codex · dev-team@example.com", "codex", "gpt-5.6-luna", "CodexWebsocketsExecutor", "low", "auto", 5074, 3149, 49, (28615, 31488, 0, 124), (0.2, 0.02, 0.25, 1.2)),
]

FAILURE_EVENT_SAMPLES = [
    event_sample(1, "codex · dev-team@example.com", "codex", "gpt-5.5", "CodexWebsocketsExecutor", "high", "auto", 4338, 237, 0, (0, 0, 0, 0), (5, 0.5, 5, 30), failed=True),
]

EVENT_SAMPLES = SUCCESS_EVENT_SAMPLES * 2 + FAILURE_EVENT_SAMPLES


def make_request_events():
    entries = []
    for index, sample in enumerate(EVENT_SAMPLES):
        key = KEYS[sample["key_index"]]
        entries.append({
            "id": str(index + 1),
            "at": iso(NOW - timedelta(hours=index * 22, minutes=(index % 4) * 11)),
            "scope": key["scope"],
            "preview": key["preview"],
            "label": key["label"],
            **{name: value for name, value in sample.items() if name != "key_index"},
        })
    return entries


REQUEST_EVENTS = make_request_events()

def event_snapshot(query):
    return int(query.get("snapshot_id", [str(max((int(entry["id"]) for entry in REQUEST_EVENTS), default=0))])[0])

def request_event_view(query, scope=""):
    snapshot = event_snapshot(query)
    selected_key = "" if scope else query.get("api_key", [""])[0]
    selected_model = query.get("model", [""])[0]
    selected_source = query.get("source", [""])[0]
    selected_provider = query.get("provider", [""])[0]
    selected_executor = query.get("executor", [""])[0]
    selected_failed = query.get("failed", [""])[0]
    offset = max(0, int(query.get("offset", ["0"])[0] or 0))
    limit = max(0, int(query.get("limit", ["0"])[0] or 0))
    time_matched = filter_event_time([entry for entry in REQUEST_EVENTS
                                     if int(entry["id"]) <= snapshot and (not scope or entry["scope"] == scope)], query)
    time_matched.sort(key=lambda entry: (entry["at"], int(entry["id"])), reverse=True)
    filter_options = {
        "models": sorted({entry.get("billing_model") or entry.get("upstream_model", "")
                          for entry in time_matched} - {""}, key=str.lower),
        "sources": sorted({entry.get("source", "") for entry in time_matched} - {""}, key=str.lower),
        "providers": sorted({entry.get("provider", "") for entry in time_matched} - {""}, key=str.lower),
        "executors": sorted({entry.get("executor_type", "") for entry in time_matched} - {""}, key=str.lower),
    }
    counts = {"all": 0, "normal": 0, "failed": 0}
    matched = []
    for entry in time_matched:
        if selected_key and entry.get("scope") != selected_key:
            continue
        if selected_model and (entry.get("billing_model") or entry.get("upstream_model")) != selected_model:
            continue
        if selected_source and entry.get("source") != selected_source:
            continue
        if selected_provider and entry.get("provider") != selected_provider:
            continue
        if selected_executor and entry.get("executor_type") != selected_executor:
            continue
        failed = bool(entry.get("failed"))
        counts["all"] += 1
        counts["failed" if failed else "normal"] += 1
        if selected_failed and failed != (selected_failed == "true"):
            continue
        matched.append(entry)
    page = matched[offset:offset + limit] if limit else matched[offset:]
    if scope:
        page = [{key: value for key, value in entry.items()
                 if key not in {"scope", "auth_index", "preview", "label"}} for entry in page]
    result = {"entries": page, "total": len(matched), "snapshot_id": str(snapshot), "status_counts": counts}
    if offset == 0:
        result["filter_options"] = filter_options
    return result


def filter_event_time(entries, query):
    from_raw = query.get("from", [""])[0]
    to_raw = query.get("to", [""])[0]
    from_time = datetime.fromisoformat(from_raw.replace("Z", "+00:00")) if from_raw else None
    to_time = datetime.fromisoformat(to_raw.replace("Z", "+00:00")) if to_raw else None
    return [entry for entry in entries if
            (not from_time or datetime.fromisoformat(entry["at"].replace("Z", "+00:00")) >= from_time) and
            (not to_time or datetime.fromisoformat(entry["at"].replace("Z", "+00:00")) < to_time)]


def account_routing(index):
    key = LIVE_KEYS[index]
    bindings = key["route_bindings"]
    route_ids = set(bindings["route_ids"])
    group_rules = []
    for group in GROUPS:
        if group["id"] in key.get("group_ids", []) and not group.get("disabled", False):
            route_ids.update(group["route_ids"])
            group_rules.append(group["rule"])
    rules = [bindings] + group_rules + [route["rule"] for route in ROUTES if route["id"] in route_ids]
    if not ACCESS_CONTROL["enabled"]:
        rules = []
    result = []
    for prefix in ("", "denied_"):
        models, refs, providers = set(), set(), set()
        for rule in rules:
            models.update(rule.get(prefix + "models", []))
            refs.update(rule.get(prefix + "credential_ids", []))
            providers.update((item["source"], item["provider"]) for item in rule.get(prefix + "credential_providers", []))
        result.extend((models, refs, providers))
    return result


def account_routing_view(index):
    models, refs, providers, denied_models, denied_refs, denied_providers = account_routing(index)
    key = LIVE_KEYS[index]
    group_ids = key.get("group_ids", [])
    managed = ACCESS_CONTROL["enabled"] and (bool(group_ids) or any(key["route_bindings"].values()))
    denied = ACCESS_CONTROL["enabled"] and (
        ACCESS_CONTROL["deny_ungrouped"] and not group_ids or
        bool(group_ids) and not any(group_grants_access(group) for group in GROUPS if group["id"] in group_ids)
    )
    def credential_view(item):
        status = item.get("status", "")
        if item.get("disabled"):
            status = "disabled"
        elif item.get("unavailable") and status in ("", "active"):
            status = "unavailable"
        elif not status:
            status = "active"
        return {"source": item["source"], "provider": item["provider"], "name": item["display_name"], "status": status,
                "denied": item["ref"] in denied_refs or (item["source"], item["provider"]) in denied_providers}
    return {
        "models": sorted(models), "denied_models": sorted(denied_models),
        "credentials": [credential_view(item) for item in CREDENTIALS if item["ref"] in refs or (item["source"], item["provider"]) in providers],
        "denied_credentials": [credential_view(item) for item in CREDENTIALS if item["ref"] in denied_refs] +
            [{"source": source, "provider": provider, "provider_wide": True, "denied": True} for source, provider in sorted(denied_providers)],
        "routing_valid": not denied, "credentials_restricted": managed,
        "warnings": ["访问被默认策略拒绝"] if denied else ["尚未允许任何上游凭证"] if managed and not refs and not providers else [],
    }


def account_auth_files(index):
    _, refs, providers, _, denied_refs, denied_providers = account_routing(index)
    view = account_routing_view(index)
    if not view["routing_valid"]:
        return []
    return [item for item in AUTH_FILES
            if (not view["credentials_restricted"] or AUTH_FILE_CREDENTIAL_REFS.get(item["auth_index"]) in refs or ("auth-files", item["category"]) in providers)
            and AUTH_FILE_CREDENTIAL_REFS.get(item["auth_index"]) not in denied_refs
            and ("auth-files", item["category"]) not in denied_providers]


def refresh_route_counts():
    for route in ROUTES:
        bound = [key for key in KEYS if route["id"] in key["route_bindings"]["route_ids"]]
        route["bound_key_count"] = len(bound)
        route["deleted_key_count"] = sum(bool(key.get("deleted_at")) for key in bound)
        route["fully_unrestricted_keys"] = sum(
            not key.get("deleted_at")
            and len(key["route_bindings"]["route_ids"]) == 1
            and not key["route_bindings"]["models"]
            and not key["route_bindings"]["credential_ids"]
            and not key["route_bindings"]["credential_providers"]
            and not any(key["route_bindings"].get(field) for field in ("denied_models", "denied_credential_ids", "denied_credential_providers"))
            for key in bound
        )


def request_error(event_index, message, status=0, error_type="", code=""):
    event = REQUEST_EVENTS[event_index]
    event["failed"] = True
    error = {"message": message}
    if error_type:
        error["type"] = error_type
    if code:
        error["code"] = code
    if 400 <= status <= 599:
        error["status"] = status
    reason = (f"HTTP {status}：" if status else "") + message
    if error_type:
        reason += f"（{error_type}）"
    return {
        "id": event["id"],
        "at": event["at"],
        "scope": event["scope"],
        "preview": event["preview"],
        "label": event["label"],
        "source": event["source"],
        "provider": event["provider"],
        "executor_type": event["executor_type"],
        "upstream_model": event["upstream_model"],
        "billing_model": event["billing_model"],
        "latency_ms": event["latency_ms"],
        "ttft_ms": event["ttft_ms"],
        "status_code": status,
        "error_type": code or error_type,
        "reason": reason,
        "body": json.dumps({"error": error}, ensure_ascii=False, separators=(",", ":")),
    }


ERRORS = [
    request_error(
        28,
        "Responses websocket connection limit reached (60 minutes). Create a new websocket connection to continue.",
        status=400,
        error_type="invalid_request_error",
        code="websocket_connection_limit_reached",
    ),
    request_error(27, "The model is not supported.", status=400, error_type="invalid_request_error"),
    request_error(26, "websocket: close 1012"),
    request_error(
        25,
        "upstream request timed out",
        status=504,
        error_type="timeout_error",
        code="upstream_timeout",
    ),
]

PLUGIN_LOGS = [
    {
        "id": 3,
        "at": iso(NOW - timedelta(minutes=2)),
        "level": "debug",
        "message": "route " + json.dumps({"key": "代码审查机器人 · sk-demo…0001", "model": "gpt-5.6-sol", "model_policy": "restricted", "model_result": "allow", "credential_policy": "restricted", "credential_result": "selected", "selected_credential": "codex · dev-team@example.com", "outcome": "succeeded", "status": 200}, ensure_ascii=False, separators=(",", ":")),
    },
    {
        "id": 2,
        "at": iso(NOW - timedelta(minutes=11)),
        "level": "info",
        "message": (
            "已加载计费数据库 /srv/cli-proxy-api/plugins/cpa-team-manager-state-v1.db："
            "8 个 API Key、3 个订阅计划、29 条请求事件。已启用。"
        ),
    },
    {
        "id": 1,
        "at": iso(NOW - timedelta(hours=12, minutes=40)),
        "level": "info",
        "message": "已同步 CLIProxyAPI 的 API Key 列表：新增 1 个。",
    },
]


def seed_paginated_history():
    """Provide multiple real pages of synthetic history in the default preview."""
    event_samples = list(REQUEST_EVENTS)
    error_samples = {entry["id"]: entry for entry in ERRORS}
    log_samples = list(PLUGIN_LOGS)
    REQUEST_EVENTS.clear()
    ERRORS.clear()
    PLUGIN_LOGS.clear()
    for batch in range(40):
        for index, sample in enumerate(event_samples):
            sequence = batch * len(event_samples) + index
            identity = {
                "id": str(sequence + 1),
                "at": iso(NOW - timedelta(minutes=sequence)),
            }
            REQUEST_EVENTS.append({**sample, **identity})
            if sample["id"] in error_samples:
                ERRORS.append({**error_samples[sample["id"]], **identity})
        for index, sample in enumerate(log_samples):
            sequence = batch * len(log_samples) + index
            PLUGIN_LOGS.append({
                **sample,
                "id": 40 * len(log_samples) - sequence,
                "at": iso(NOW - timedelta(minutes=sequence)),
            })


def error_view(query, scope=""):
    snapshot = event_snapshot(query)
    rows = filter_event_time([entry for entry in ERRORS
                             if int(entry["id"]) <= snapshot and (not scope or entry["scope"] == scope)], query)
    rows.sort(key=lambda entry: (entry["at"], int(entry["id"])), reverse=True)
    selected = {
        "api_key": "" if scope else query.get("api_key", [""])[0],
        "model": query.get("model", [""])[0],
        "source": query.get("source", [""])[0],
        "provider": query.get("provider", [""])[0],
        "executor": query.get("executor", [""])[0],
        "status_code": query.get("status_code", [""])[0],
        "error_type": query.get("error_type", [""])[0],
    }
    filtered = []
    counts = {}
    empty_type = query.get("error_type_empty", [""])[0] == "true"
    for entry in rows:
        if selected["api_key"] and entry["scope"] != selected["api_key"]:
            continue
        if selected["model"] and entry["billing_model"] != selected["model"]:
            continue
        if selected["source"] and entry["source"] != selected["source"]:
            continue
        if selected["provider"] and entry["provider"] != selected["provider"]:
            continue
        if selected["executor"] and entry["executor_type"] != selected["executor"]:
            continue
        if selected["status_code"] and str(entry["status_code"]) != selected["status_code"]:
            continue
        error_type = entry["error_type"]
        counts[error_type] = counts.get(error_type, 0) + 1
        if empty_type:
            if error_type:
                continue
        elif selected["error_type"] and error_type != selected["error_type"]:
            continue
        filtered.append(entry)
    offset = max(0, int(query.get("offset", ["0"])[0] or 0))
    limit = max(0, int(query.get("limit", ["0"])[0] or 0))
    page = filtered[offset:offset + limit] if limit else filtered[offset:]
    if scope:
        page = [{key: value for key, value in entry.items()
                 if key not in {"scope", "preview", "label", "auth_index"}} for entry in page]
    result = {"entries": page, "total": len(filtered), "snapshot_id": str(snapshot), "error_type_counts": counts}
    if offset == 0:
        result["filter_options"] = {
            "models": sorted({entry["billing_model"] for entry in rows}),
            "sources": sorted({entry["source"] for entry in rows}),
            "providers": sorted({entry["provider"] for entry in rows}),
            "executors": sorted({entry["executor_type"] for entry in rows}),
            "status_codes": sorted({entry["status_code"] for entry in rows if entry["status_code"]}),
            "error_types": sorted({entry["error_type"] for entry in rows if entry["error_type"]}),
        }
    return result


def analysis_view(query, scope=""):
    rows = filter_event_time([entry for entry in REQUEST_EVENTS if not scope or entry["scope"] == scope], query)
    selected = query.get("api_key", [""])[0]
    if selected and not scope:
        rows = [entry for entry in rows if entry["scope"] == selected]

    def distribution(field, label_field=None, unknown="未知"):
        grouped = {}
        for entry in rows:
            key = entry.get(field, "") or unknown
            label = entry.get(label_field, "") if label_field else key
            if not label and field == "scope":
                label = entry.get("preview", "")
            item = grouped.setdefault(key, {"key": key, "label": label or key,
                                            "total_tokens": 0, "requests": 0,
                                            "cost_usd": 0})
            if field == "scope":
                item["preview"] = entry.get("preview", "")
            cost = entry.get("cost", {})
            item["total_tokens"] += sum(cost.get(name, 0) for name in (
                "uncached_input_tokens", "cache_read_tokens", "cache_write_tokens", "billed_output_tokens"))
            item["requests"] += 1
            item["cost_usd"] += cost.get("total_usd", 0)
        token_total = sum(item["total_tokens"] for item in grouped.values())
        request_total = sum(item["requests"] for item in grouped.values())
        for item in grouped.values():
            denominator = token_total if token_total else request_total
            numerator = item["total_tokens"] if token_total else item["requests"]
            item["percent"] = numerator * 100 / max(1, denominator)
        return sorted(grouped.values(), key=lambda item: (-item["total_tokens"], -item["requests"], item["label"]))

    requests = len(rows)
    failed = sum(int(entry.get("failed", False)) for entry in rows)
    input_tokens = sum(
        sum(entry.get("cost", {}).get(name, 0) for name in (
            "uncached_input_tokens", "cache_read_tokens", "cache_write_tokens"
        )) for entry in rows
    )
    cache_read_tokens = sum(entry.get("cost", {}).get("cache_read_tokens", 0) for entry in rows)
    cache_write_tokens = sum(entry.get("cost", {}).get("cache_write_tokens", 0) for entry in rows)
    output_tokens = sum(entry.get("cost", {}).get("billed_output_tokens", 0) for entry in rows)
    cost = {
        "input_usd": sum(entry.get("cost", {}).get("uncached_input_usd", 0) for entry in rows),
        "cache_read_usd": sum(entry.get("cost", {}).get("cache_read_usd", 0) for entry in rows),
        "cache_write_usd": sum(entry.get("cost", {}).get("cache_write_usd", 0) for entry in rows),
        "output_usd": sum(entry.get("cost", {}).get("output_usd", 0) for entry in rows),
    }
    cost["total_usd"] = sum(cost[field] for field in (
        "input_usd", "cache_read_usd", "cache_write_usd", "output_usd"
    ))

    from_time = datetime.fromisoformat(
        query.get("from", [iso(NOW - timedelta(days=30))])[0].replace("Z", "+00:00")
    )
    to_time = datetime.fromisoformat(
        query.get("to", [iso(NOW)])[0].replace("Z", "+00:00")
    )
    bucket_size = timedelta(hours=1)
    bucket_start = from_time
    if to_time - from_time > timedelta(days=1):
        bucket_size = timedelta(days=1)
        browser_zone = ZoneInfo(query.get("timezone", ["UTC"])[0])
        local_from = from_time.astimezone(browser_zone)
        bucket_start = local_from.replace(hour=0, minute=0, second=0, microsecond=0)
    buckets = []
    cursor = bucket_start
    while cursor < to_time:
        buckets.append({"time": cursor, "requests": 0,
                        "input_tokens": 0, "output_tokens": 0,
                        "cache_read_tokens": 0, "cache_write_tokens": 0,
                        "total_cost": 0})
        cursor += bucket_size
    for entry in rows:
        at = datetime.fromisoformat(entry["at"].replace("Z", "+00:00"))
        if at < bucket_start or not buckets:
            continue
        index = len(buckets) - 1
        for candidate in range(1, len(buckets)):
            if at < buckets[candidate]["time"]:
                index = candidate - 1
                break
        item = buckets[index]
        item["requests"] += 1
        entry_cost = entry.get("cost", {})
        item["input_tokens"] += entry_cost.get("uncached_input_tokens", 0)
        item["output_tokens"] += entry_cost.get("billed_output_tokens", 0)
        item["cache_read_tokens"] += entry_cost.get("cache_read_tokens", 0)
        item["cache_write_tokens"] += entry_cost.get("cache_write_tokens", 0)
        item["total_cost"] += entry_cost.get("total_usd", 0)

    def trend(value):
        return [{"time": iso(item["time"]), "value": value(item)} for item in buckets]

    def total_input(item):
        return item["input_tokens"] + item["cache_read_tokens"] + item["cache_write_tokens"]

    trends = {
        "requests": trend(lambda item: item["requests"]),
        "total_tokens": trend(lambda item: total_input(item) + item["output_tokens"]),
        "input_tokens": trend(lambda item: item["input_tokens"]),
        "output_tokens": trend(lambda item: item["output_tokens"]),
        "cache_read_tokens": trend(lambda item: item["cache_read_tokens"]),
        "cache_write_tokens": trend(lambda item: item["cache_write_tokens"]),
        "cache_rate": trend(lambda item: item["cache_read_tokens"] * 100 / total_input(item)
                            if total_input(item) else 0),
        "total_cost": trend(lambda item: item["total_cost"]),
    }

    return {
        "summary": {
            "requests": requests,
            "succeeded": requests - failed,
            "failed": failed,
            "success_rate": (requests - failed) * 100 / requests if requests else 0,
            "total_tokens": input_tokens + output_tokens,
            "input_tokens": input_tokens,
            "output_tokens": output_tokens,
            "cache_read_tokens": cache_read_tokens,
            "cache_write_tokens": cache_write_tokens,
            "cache_rate": cache_read_tokens * 100 / input_tokens if input_tokens else 0,
            "cost": cost,
        },
        "trends": trends,
        "usage_distribution": {
            "api_keys": [] if scope or selected else distribution("scope", "label"),
            "models": distribution("billing_model", unknown="未知模型"),
            "sources": distribution("source", unknown="未知来源"),
        },
    }


for price in PRICES:
    price["in_models"] = True
PRICES.extend([
    {
        "model_id": "demo-reference", "source": "reference",
        "input_per_1m": 1.5, "output_per_1m": 3, "in_models": True,
    },
    {
        "model_id": "demo-free", "source": "custom",
        "input_per_1m": 0, "output_per_1m": 0, "in_models": True,
    },
    {
        "model_id": "demo-unpriced", "source": "none",
        "input_per_1m": 0, "output_per_1m": 0, "in_models": True,
    },
    {
        "model_id": "demo-retired-custom", "source": "custom",
        "input_per_1m": 1, "output_per_1m": 2, "in_models": False,
    },
])
REFERENCE_PRICES = {
    price["model_id"]: dict(price)
    for price in PRICES if price["source"] == "reference"
}


def price_status():
    return {
        "metadata": {
            "source_url": "https://models.dev/catalog.json",
            "content_hash": "dummy-ui-reference-prices-hash",
            "version": 1,
            "model_count": len(REFERENCE_PRICES),
            "fetched_at": "2026-09-05T00:00:00Z",
            "usable": True,
        },
    }


def model_prices(query, include_custom):
    models = set(query.get("model", []))
    rows = {row["model_id"]: row for row in PRICES}
    names = models | ({row["model_id"] for row in PRICES if row["source"] == "custom"} if include_custom else set())
    return [dict(rows.get(model, {"model_id": model, "source": "none", "input_per_1m": 0, "output_per_1m": 0}),
                 in_models=model in models) for model in sorted(names)]


def credential_labels(refs):
    return {item["ref"]: item["provider"] + " · " + item["display_name"]
            for item in CREDENTIALS if item["ref"] in refs}


def key_rows():
    for key in LIVE_KEYS:
        refresh_key_quota(key)
    return [dict(key, route_names={route["id"]: route["name"] for route in ROUTES
                                  if route["id"] in key["route_bindings"]["route_ids"]},
                 credential_labels=credential_labels(key["route_bindings"]["credential_ids"] + key["route_bindings"].get("denied_credential_ids", []))) for key in KEYS]


def route_rows():
    return [dict(route, credential_labels=credential_labels(route["rule"].get("credential_ids", []) + route["rule"].get("denied_credential_ids", []))) for route in ROUTES]


def group_grants_access(group):
    return not group.get("disabled", False) and (bool(group["route_ids"]) or any(group["rule"].values()))


def rule_credential_refs(rule):
    return rule["credential_ids"] + rule["denied_credential_ids"]


def normalize_route_rule(rule):
    """Mirror billing.NormalizeRouteRule closely enough for UI checks."""
    if rule is None:
        rule = {}
    if not isinstance(rule, dict):
        raise ValueError("路由规则格式无效")
    unknown = set(rule) - set(ROUTE_RULE_FIELDS)
    if unknown:
        raise ValueError("未知字段：" + ", ".join(sorted(unknown)))
    result = {}
    for field in ROUTE_RULE_FIELDS:
        values = rule.get(field) or []
        if not isinstance(values, list):
            raise ValueError(field + " 必须是数组")
        items, seen = [], set()
        for value in values:
            if field.endswith("credential_providers"):
                if not isinstance(value, dict) or not isinstance(value.get("source"), str) or not isinstance(value.get("provider"), str):
                    raise ValueError("凭证类别无效")
                item = {"source": value["source"].strip(), "provider": value["provider"].strip().lower()}
                if item["source"] not in {"auth-files", "ai-providers"} or not item["provider"]:
                    raise ValueError("凭证类别无效")
                key = (item["source"], item["provider"])
            else:
                if not isinstance(value, str) or not value.strip():
                    raise ValueError("路由选项无效")
                item = value.strip()
                if field.endswith("credential_ids"):
                    item = item.lower()
                    if not item.startswith("sha256:") or len(item) != 71:
                        raise ValueError("上游凭证引用无效")
                key = item.lower()
            if key not in seen:
                seen.add(key)
                items.append(item)
        result[field] = items
    for field, name in (("models", "模型"), ("credential_ids", "凭证"), ("credential_providers", "凭证类别")):
        allowed = [json.dumps(item, sort_keys=True).lower() for item in result[field]]
        if any(json.dumps(item, sort_keys=True).lower() in allowed for item in result["denied_" + field]):
            raise ValueError(f"同一{name}不能同时加入黑白名单")
    return result


def group_row(group):
    return dict(group, scopes=[key["scope"] for key in KEYS if group["id"] in key.get("group_ids", [])],
                credential_labels=credential_labels(rule_credential_refs(group["rule"])))


def group_rows():
    return [group_row(group) for group in GROUPS]



TEAM_ACCOUNT_SETTINGS = {"require_turn_state": True, "accounts": {}}
TEAM_RISK = {"config": {"enabled": False, "mode": "observe", "blocked_keywords": [], "model_filter": {"mode": "all", "models": []}, "remember_hashes": True, "block_status": 403, "block_message": "Request blocked by configured risk policy", "retention_days": 30, "max_events": 500}, "status": {"observed": 3, "blocked": 0, "not_inspected": 2, "remembered_hashes": 1}, "events": [{"at": iso(NOW), "decision": "observed", "reason": "keyword", "model": "gpt-5.4", "caller_ref": "sha256:" + "a" * 64}, {"at": iso(NOW), "decision": "not_inspected", "reason": "payload_unavailable", "model": "gpt-5.4"}], "storage_error": ""}
TEAM_INTEGRATIONS = []
TEAM_CHANNELS = [{"name": "DeepSeek", "disabled": False, "api-key-entries": [{"api-key": "sk-dummy-deepseek"}], "extra-preserve": {"value": 42}}]
TEAM_DEVICE_LOGINS = {}
TEAM_NATIVE_CHANNELS = {"codex-api-key": [{"prefix": "other-codex", "api-key": "sk-dummy-codex", "extra-preserve": 123}], "claude-api-key": [{"prefix": "other-claude", "api-key": "sk-dummy-claude", "extra-preserve": 456}]}
TEAM_NATIVE_CHANNELS["codex-api-key"][0].update({"auth-index": "model-demo-native-codex", "models": [{"name": "gpt-6-astra", "alias": "demo-astra"}]})
MODEL_TESTS = {}
MODEL_TEST_PRESETS = json.loads((UI_PATH.parent / "model_test_presets.json").read_text(encoding="utf-8"))


def model_test_catalog(include_native=False):
    accounts = [{"auth_index": file["auth_index"], "credential_ref": dummy_state_credential_ref(file["credential_ref"]), "name": file["name"],
                 "provider": file["category"], "source": "auth-files", "disabled": file["disabled"],
                 "supported": file["category"] == "codex",
                 "reason": "Only Codex OAuth is supported for file-account tests" if file["category"] != "codex" else ""}
                for file in AUTH_FILES]
    for item in CREDENTIALS:
        if item["provider"] == "codex" and item["source"] == "auth-files" and not any(file["credential_ref"] == item["ref"] for file in AUTH_FILES):
            accounts.append({"auth_index": dummy_state_auth_index(item), "credential_ref": dummy_state_credential_ref(item["ref"]),
                             "name": item["display_name"], "provider": "codex", "source": "auth-files", "disabled": item["disabled"], "supported": True})
    if include_native:
        accounts.append({"auth_index": "model-demo-native-codex", "credential_ref": "sha256:" + "c" * 64,
                         "name": "Native Codex demo", "provider": "codex", "source": "ai-providers",
                         "supported": True, "disabled": False})
    accounts.append({"auth_index": "model-demo-disabled", "credential_ref": "sha256:" + "8" * 64,
                     "name": "Disabled account demo", "provider": "codex", "source": "auth-files",
                     "supported": True, "disabled": True})
    return {"accounts": accounts, "presets": MODEL_TEST_PRESETS, "usage_available": False,
            "limits": {"max_active": 4, "max_prompt_bytes": 8192, "lease_seconds": 90,
                       "send_within_seconds": 10, "max_response_bytes": 1048576}}


def prepare_dummy_model_test(body):
    account = next((item for item in model_test_catalog(True)["accounts"] if item["auth_index"] == body.get("auth_index")), None)
    preset = next((item for item in MODEL_TEST_PRESETS if item["id"] == body.get("preset")), None)
    prompt = body.get("prompt", "")
    if not account or not account["supported"] or not body.get("model") or not preset:
        return 400, {"error": {"message": "Choose a supported account, exact model and preset"}}
    if not prompt.strip() or len(prompt.encode()) > 8192 or "$TOKEN$" in prompt:
        return 400, {"error": {"message": "Invalid model test prompt"}}
    if preset["id"] != "free" and prompt != preset["prompt"]:
        return 400, {"error": {"message": "Use the custom preset after editing a test prompt"}}
    config = body.get("config") or {}
    if account["source"] == "ai-providers" and config.get("credential_ref"):
        account = dict(account, credential_ref=config["credential_ref"], provider=config.get("provider", account["provider"]), disabled=bool(config.get("disabled")))
    model = body["model"]
    for mapping in config.get("models", []):
        if model == mapping.get("alias"):
            model = mapping["name"]
            break
    proxy = config.get("proxy_url") or body.get("global_proxy_url") or "direct"
    source = "account" if config.get("proxy_url") else "global" if body.get("global_proxy_url") else "direct"
    if proxy != "direct" and not proxy.startswith(("http://", "https://", "socks5://", "socks5h://")):
        return 400, {"error": {"message": "Invalid proxy; no direct fallback is allowed"}}
    test_id = f"{time.time_ns():048x}"
    MODEL_TESTS[test_id] = {"preset": preset["id"], "account": account, "model": model, "requested_model": body["model"]}
    now = datetime.now(timezone.utc)
    return 200, {"test_id": test_id, "start_before": iso(now + timedelta(seconds=10)),
                 "expires_at": iso(now + timedelta(seconds=90)), "lease_expires_at": iso(now + timedelta(seconds=90)),
                 "account": account, "model": model, "preset": preset["id"], "usage_available": False,
                 "proxy": {"source": source, "endpoint": "direct" if proxy == "direct" else masked_dummy_proxy(proxy)},
                 "api_call": {"auth_index": account["auth_index"], "method": "POST", "proxy_url": proxy,
                              "url": "https://model-test.dummy.invalid/responses", "header": {"Authorization": "Bearer $TOKEN$"},
                              "data": json.dumps({"model": model, "input": prompt, "stream": False})}}


def model_test_exact_json(actual, expected):
    def unique_object(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError("duplicate JSON key")
            result[key] = value
        return result

    def reject_constant(value):
        raise ValueError("invalid JSON number")

    def parse(value):
        return json.loads(value, object_pairs_hook=unique_object, parse_int=Decimal,
                          parse_float=Decimal, parse_constant=reject_constant)

    def equal(left, right):
        if type(left) is not type(right):
            return False
        if isinstance(left, dict):
            return left.keys() == right.keys() and all(equal(left[key], right[key]) for key in left)
        if isinstance(left, list):
            return len(left) == len(right) and all(equal(a, b) for a, b in zip(left, right))
        return left == right

    try:
        return equal(parse(actual), parse(expected))
    except (ValueError, RecursionError, ArithmeticError):
        return False


def complete_dummy_model_test(body):
    lease = MODEL_TESTS.get(body.get("test_id"))
    if not lease:
        return 409, {"error": {"message": "Test expired or already completed"}}
    result = {"test_id": body["test_id"], "outcome": "failed", "output": "", "output_truncated": False,
              "assertions": [], "usage_available": False, "requested_model": lease["requested_model"],
              "upstream_model": lease["model"], "response_model": "", "response_model_status": "missing"}
    if body.get("transport_failed"):
        return 200, dict(result, lease_retained=True, reason="Management connection failed; the lease remains until timeout")
    del MODEL_TESTS[body["test_id"]]
    if body.get("not_started"):
        return 200, dict(result, reason="The prepared test was cancelled before sending")
    if not 200 <= body.get("status_code", 0) < 300:
        return 200, dict(result, reason="The model did not return a successful response")
    try:
        parsed = json.loads(body.get("body", ""))
        output = parsed.get("output_text", "")
        declared = parsed.get("model", "")
        if isinstance(declared, str) and declared and len(declared.encode()) <= 256 and not any(ch.isspace() or ord(ch) < 32 for ch in declared):
            result.update(response_model=declared, response_model_status="matched" if declared == lease["model"] else "different")
    except (ValueError, AttributeError):
        output = ""
    if not isinstance(output, str) or not output:
        return 200, dict(result, reason="No supported model text output")
    result.update(outcome="completed", output=output[:16384], output_truncated=len(output) > 16384)
    preset = next((item for item in MODEL_TEST_PRESETS if item["id"] == lease["preset"]), {})
    expected = preset.get("expected") if preset.get("assertion") == "Exact JSON object" else None
    if expected is not None:
        passed = not result["output_truncated"] and model_test_exact_json(output, expected)
        result["assertions"] = [{"name": "Exact JSON object", "passed": passed, "expected": expected}]
    return 200, result

def team_account_runtime():
    return {"accounts": [{"credential_ref": item["ref"], "auth_index": "dummy-index-" + item["ref"], "name": item["display_name"], "provider": item["provider"], "disabled": item["disabled"], "source": item["source"], "status_patch": {"name": "dummy-account-" + item["ref"], "auth_index": "dummy-index-" + item["ref"]} if item["source"] == "auth-files" else None, "concurrency_limit": TEAM_ACCOUNT_SETTINGS["accounts"].get(item["ref"], {}).get("concurrency_limit", 0), "current_concurrency": index % 3, "usage": {"requests": 80 + index, "successes": 76, "failures": 4 + index, "total_tokens": 82500, "amount_usd": 3.42, "last_used_at": iso(NOW)}} for index, item in enumerate(CREDENTIALS)], "settings": TEAM_ACCOUNT_SETTINGS, "usage_retention_days": 365, "host_schema": 6, "turn_state_host_supported": True}

def team_channels(account):
    descriptors = []
    if account["kind"].startswith("opencode-"):
        protocols = [("chat", "openai-compatibility", "name"), ("responses", "codex-api-key", "prefix"), ("anthropic", "claude-api-key", "prefix")]
    else:
        protocols = [("chat", "openai-compatibility", "name")]
    for protocol, resource, field in protocols:
        identity = account["channel_name"] + "-" + protocol
        models = [name for name in account["models"] if ("anthropic" if name.startswith("claude") else "responses" if name.startswith("gpt") else "chat") == protocol] if account["kind"].startswith("opencode-") else account["models"]
        config = {field: identity, "base-url": account["base_url"], "models": [{"name": name, "alias": ""} for name in models]}
        if resource == "openai-compatibility":
            config["api-key-entries"] = [{"api-key": "sk-dummy-integration-fixture"}]
        else:
            config["api-key"] = "sk-dummy-integration-fixture"
        descriptors.append({"resource": resource, "identity_field": field, "identity": identity, "protocol": protocol, "models": models, "config": config if models else None})
    account["channels"] = [{key: value for key, value in descriptor.items() if key != "config"} for descriptor in descriptors]
    account["client_models"] = [descriptor["identity"] + "/" + model if descriptor["resource"] != "openai-compatibility" else model for descriptor in descriptors for model in descriptor["models"]]
    return descriptors

def team_response(account):
    channels = team_channels(account)
    return {"account": dict(account), "channels": channels, "migration_id": account.get("pending_migration", "")}

def team_integration(body):
    identity = body.get("id") or "demo-integration-" + str(len(TEAM_INTEGRATIONS) + 1)
    account = next((item for item in TEAM_INTEGRATIONS if item["id"] == identity), None)
    if account is None:
        account = {"id": identity, "kind": body.get("kind", "cline-pass"), "channel_name": "team-" + identity, "base_url": "https://dummy.example/v1", "has_api_key": True, "has_auth_cookie": False, "has_refresh_token": body.get("kind") == "cline-pass", "models": ["gpt-5.4"]}
        TEAM_INTEGRATIONS.append(account)
    if body.get("id") and body.get("api_key"):
        account["pending_migration"] = "demo-migration-" + identity
    for key in ["name", "models", "workspace_id"]:
        if key in body:
            account[key] = body[key]
    if body.get("auth_cookie"):
        account["has_auth_cookie"] = True
    return team_response(account)

def payload_for(path, query):
    if path == f"{API_BASE}/model-tests":
        return model_test_catalog()
    if path == "/v0/management/proxy-url":
        return {"proxy-url": ""}
    if path == "/v0/management/gemini-api-key":
        return {"gemini-api-key": []}
    if path == f"{API_BASE}/account-runtime":
        return team_account_runtime()
    if path == f"{API_BASE}/risk-center":
        return TEAM_RISK
    if path == f"{API_BASE}/integrations":
        return {"accounts": TEAM_INTEGRATIONS, "providers": []}
    if path == "/v0/management/openai-compatibility":
        return {"openai-compatibility": TEAM_CHANNELS}
    if path in {"/v0/management/codex-api-key", "/v0/management/claude-api-key"}:
        resource = path.rsplit("/", 1)[-1]
        return {resource: TEAM_NATIVE_CHANNELS[resource]}

    if path == f"{API_BASE}/persistence":
        return {"detected": True, "container": True, "at_risk": True, "can_configure": False,
                "paths": [{"kind": "billing_database", "path": "/app/data/billing.db", "state": "container_layer"},
                          {"kind": "turn_state", "path": "/app/data/turn-state.json", "state": "container_layer"},
                          {"kind": "plugin_library", "path": "/app/plugins/key-billing.so", "state": "mounted"}]}
    if path == f"{API_BASE}/turn-state":
        return turn_state_view()
    if path == f"{API_BASE}/turn-state/runner":
        return dict(TURN_STATE_RUNNER)
    if path == f"{API_BASE}/turn-state/probe-progress":
        return {"active": bool(TURN_STATE_PROGRESS), "result": dict(TURN_STATE_PROGRESS)}
    if path == f"{API_BASE}/access-control":
        return {"access_control": ACCESS_CONTROL}
    if path == f"{API_BASE}/groups":
        return {"groups": group_rows()}
    if path == f"{API_BASE}/keys":
        refresh_route_counts()
        return {"keys": key_rows()}
    if path == f"{API_BASE}/plans":
        return {"plans": PLANS}
    if path == f"{API_BASE}/routes":
        refresh_route_counts()
        return {"routes": route_rows()}
    if path == f"{API_BASE}/credentials":
        return {"credentials": CREDENTIALS}
    if path == f"{API_BASE}/prices/reference/status":
        return price_status()
    if path == f"{API_BASE}/prices":
        return model_prices(query, include_custom=query.get("include_custom", ["true"])[0] == "true")
    if path == f"{API_BASE}/events/keys":
        scopes = {event["scope"] for event in filter_event_time(REQUEST_EVENTS, query)}
        return [{field: key[field] for field in ("scope", "preview", "label", "deleted_at") if field in key}
                for key in KEYS if key["scope"] in scopes]
    if path == f"{API_BASE}/events":
        return request_event_view(query)
    if path == f"{API_BASE}/errors":
        return error_view(query)
    if path == f"{API_BASE}/analysis":
        return analysis_view(query)
    if path == f"{API_BASE}/plugin-logs":
        counts = {level: sum(entry["level"] == level for entry in PLUGIN_LOGS)
                  for level in ("debug", "info", "error")}
        levels = query.get("level", ["all"])[0].split(",")
        before = int(query.get("before_id", ["0"])[0])
        limit = int(query.get("limit", ["100"])[0])
        entries = [entry for entry in PLUGIN_LOGS
                   if ("all" in levels or entry["level"] in levels)
                   and (not before or entry["id"] < before)]
        return {"entries": entries[:limit], "level_counts": counts,
                "next_before_id": entries[limit - 1]["id"] if len(entries) > limit else 0}
    if path == "/v0/management/auth-files":
        return {"files": [{**item, "type": item["category"], "id_token": {"chatgpt_account_id": "dummy-" + item["auth_index"]}} for item in AUTH_FILES]}
    if path == f"{API_BASE}/auth-files":
        return {"files": AUTH_FILES}
    if path == f"{API_BASE}/auth-files/quota":
        return auth_file_quota(query)
    if path == f"{API_BASE}/prices/reference":
        term = query.get("q", [""])[0].lower()
        return {"prices": [
            {**price, "provider_id": "demo", "model_id": model, "is_canonical": True}
            for model, price in REFERENCE_PRICES.items() if term in model.lower()
        ]}
    if path in {"/v0/management/config", "/v0/management/api-keys"}:
        config = {"api-keys": [f"sk-demo-{index:04d}" for index in range(1, len(LIVE_KEYS) + 1)]}
        if path == "/v0/management/config":
            config.update({
                "gemini-api-key": [],
                "interactions-api-key": [],
                "xai-api-key": [],
                "vertex-api-key": [],
                "codex-api-key": TEAM_NATIVE_CHANNELS["codex-api-key"],
                "claude-api-key": TEAM_NATIVE_CHANNELS["claude-api-key"],
                "openai-compatibility": TEAM_CHANNELS,
            })
        return config
    if path == "/v1/models":
        return {"data": [{"id": row["model_id"]} for row in PRICES if row.get("in_models")] + [
            {"id": "codex/deepseek-v4-flash-vision-exp"},
        ]}
    return None


class Handler(BaseHTTPRequestHandler):
    host_mode = "standalone"
    initial_theme = "auto"

    def send_response(self, code, message=None):
        time.sleep(random.uniform(0.4, 0.6))
        super().send_response(code, message)

    def send_html(self, body):
        encoded = body.encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Cache-Control", "no-store")
        self.send_header("Content-Length", str(len(encoded)))
        self.end_headers()
        self.wfile.write(encoded)

    def send_json(self, status, payload):
        if 200 <= status < 300 and getattr(self, "mutation_view", None) is not None:
            path = urlparse(self.path).path
            view = {}
            if path in {f"{API_BASE}/plans", f"{API_BASE}/routes", f"{API_BASE}/groups", f"{API_BASE}/access-control"} or path.startswith(f"{API_BASE}/keys/"):
                refresh_route_counts()
                view["keys"] = key_rows()
                view["groups"] = group_rows()
                view["access_control"] = ACCESS_CONTROL
                if path == f"{API_BASE}/plans":
                    view["plans"] = PLANS
                if path in {f"{API_BASE}/routes", f"{API_BASE}/keys/routes"}:
                    view["routes"] = route_rows()
            elif path in {f"{API_BASE}/prices", f"{API_BASE}/prices/reference/refresh"}:
                view["prices"] = model_prices({"model": self.mutation_view.get("models", [])}, include_custom=True)
                view["metadata"] = price_status()["metadata"]
            elif path == f"{API_BASE}/plugin-logs":
                view["logs_cleared"] = True
            payload = dict(payload, view=view)
        body = json.dumps(payload, ensure_ascii=False).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        self.mutation_view = None
        parsed = urlparse(self.path)
        if parsed.path == "/favicon.ico":
            self.send_response(204)
            self.end_headers()
            return
        if parsed.path == "/" and self.host_mode != "standalone":
            label = "CPAMC" if self.host_mode == "cpamc" else "CPAMP"
            body = (
                HOST_SHELL.replace("__HOST_MODE__", self.host_mode)
                .replace("__HOST_LABEL__", label)
                .replace("__INITIAL_THEME__", self.initial_theme)
            )
            self.send_html(body)
            return
        if parsed.path in ("/", "/ui"):
            body = UI_PATH.read_text(encoding="utf-8")
            catalogs = {language: json.loads((UI_PATH.parent / "locales" / f"{language}.json").read_text(encoding="utf-8"))
                        for language in ("en", "zh-CN")}
            script = "const BILLING_MESSAGES = " + json.dumps(catalogs).replace("<", "\\u003c") + ";\n"
            script += (UI_PATH.parent / "i18n.js").read_text(encoding="utf-8")
            body = body.replace("// BILLING_I18N", script)
            if self.host_mode != "standalone":
                body = body.replace(
                    "</head>",
                    '<script>localStorage.setItem("managementKey", JSON.stringify("dummy"));</script>\n</head>',
                    1,
                )
            self.send_html(body)
            return
        authorization = self.headers.get("Authorization", "")
        api_keys = [f"sk-demo-{index:04d}" for index in range(1, len(LIVE_KEYS) + 1)]
        if parsed.path == "/v1/models":
            if not authorization.startswith("Bearer ") or authorization[7:] not in api_keys:
                self.send_json(401, {"error": {"message": "API Key 无效"}})
                return
        resource_paths = {
            f"{RESOURCE_BASE}/profile",
            f"{RESOURCE_BASE}/subscription",
            f"{RESOURCE_BASE}/routing",
            f"{RESOURCE_BASE}/prices",
            f"{RESOURCE_BASE}/events",
            f"{RESOURCE_BASE}/errors",
            f"{RESOURCE_BASE}/analysis",
            f"{RESOURCE_BASE}/auth-files",
            f"{RESOURCE_BASE}/auth-files/quota",
        }
        if parsed.path in resource_paths:
            if not authorization.startswith("Bearer ") or authorization[7:] not in api_keys:
                self.send_json(401, {"error": {"message": "API Key 无效"}})
                return
            index = api_keys.index(authorization[7:])
            if parsed.path.endswith("/profile"):
                key = LIVE_KEYS[index]
                self.send_json(200, {"tracked": True, "identity": {"preview": key["preview"], "label": key["label"]}})
            elif parsed.path.endswith("/subscription"):
                key = LIVE_KEYS[index]
                refresh_key_quota(key)
                self.send_json(200, {"subscription": {"name": key["plan_name"], "unlimited": key["unlimited"], "blocked": key["blocked"], "partially_blocked": key.get("partially_blocked", False), "windows": key["windows"], "retry_at": key.get("retry_at")}, "concurrency": {"limit": key["concurrency_limit"], "current": key["current_concurrency"]}})
            elif parsed.path.endswith("/routing"):
                self.send_json(200, account_routing_view(index))
            elif parsed.path.endswith("/prices"):
                self.send_json(200, model_prices(parse_qs(parsed.query), include_custom=False))
            elif parsed.path.endswith("/analysis"):
                self.send_json(200, analysis_view(parse_qs(parsed.query), LIVE_KEYS[index]["scope"]))
            elif parsed.path.endswith("/errors"):
                self.send_json(200, error_view(parse_qs(parsed.query), LIVE_KEYS[index]["scope"]))
            elif parsed.path.endswith("/auth-files"):
                self.send_json(200, {"files": account_auth_files(index)})
            elif parsed.path.endswith("/auth-files/quota"):
                query = parse_qs(parsed.query)
                allowed = {
                    item["auth_index"] for item in account_auth_files(index)
                }
                auth_index = query.get("auth_index", [""])[0]
                payload = auth_file_quota(query) if auth_index in allowed else None
                if payload is None:
                    self.send_json(404, {"error": {"message": "认证文件不存在或不支持限额查询"}})
                else:
                    self.send_json(200, payload)
            elif parsed.path.endswith("/events"):
                self.send_json(200, request_event_view(parse_qs(parsed.query), LIVE_KEYS[index]["scope"]))
            return
        payload = payload_for(parsed.path, parse_qs(parsed.query))
        if payload is None:
            self.send_json(404, {"error": {"message": "dummy backend: route not found"}})
            return
        self.send_json(200, payload)

    def do_POST(self):
        self.handle_mutation()

    def do_PATCH(self):
        self.handle_mutation()

    def do_PUT(self):
        self.handle_mutation()

    def do_DELETE(self):
        self.handle_mutation()

    def handle_mutation(self):
        parsed = urlparse(self.path)
        self.mutation_view = None
        length = int(self.headers.get("Content-Length", "0"))
        request_body = b""
        if length:
            request_body = self.rfile.read(length)
        if parse_qs(parsed.query).get("view") == ["1"]:
            self.mutation_view = json.loads(request_body or b"{}")
            request_body = json.dumps(self.mutation_view.get("data") or {}).encode()
        route = self.command, parsed.path
        if self.command == "PUT" and parsed.path in {"/v0/management/openai-compatibility", "/v0/management/codex-api-key", "/v0/management/claude-api-key"}:
            body = json.loads(request_body or b"[]")
            if not isinstance(body, list):
                self.send_json(400, {"error": {"message": "Expected a raw channel array"}})
                return
            if parsed.path.endswith("/openai-compatibility"):
                TEAM_CHANNELS[:] = body
            else:
                TEAM_NATIVE_CHANNELS[parsed.path.rsplit("/", 1)[-1]][:] = body
            self.send_json(200, {"status": "ok"})
            return
        if route == ("PUT", f"{API_BASE}/account-runtime/settings"):
            body = json.loads(request_body or b"{}")
            if "accounts" in body:
                TEAM_ACCOUNT_SETTINGS["accounts"].update(body["accounts"])
            if "require_turn_state" in body:
                TEAM_ACCOUNT_SETTINGS["require_turn_state"] = body["require_turn_state"]
            self.send_json(200, TEAM_ACCOUNT_SETTINGS)
            return
        if route == ("PUT", f"{API_BASE}/risk-center/config"):
            TEAM_RISK["config"].update(json.loads(request_body or b"{}"))
            self.send_json(200, TEAM_RISK)
            return
        if self.command == "DELETE" and parsed.path in {f"{API_BASE}/risk-center/events", f"{API_BASE}/risk-center/hashes"}:
            if parsed.path.endswith("/events"):
                TEAM_RISK["events"] = []
            else:
                TEAM_RISK["status"]["remembered_hashes"] = 0
            self.send_json(200, TEAM_RISK)
            return
        if parsed.path.startswith(f"{API_BASE}/integrations"):
            body = json.loads(request_body or b"{}")
            account = next((item for item in TEAM_INTEGRATIONS if item["id"] == body.get("id")), None)
            if route == ("POST", f"{API_BASE}/integrations"):
                self.send_json(200, team_integration(body))
            elif route == ("POST", f"{API_BASE}/integrations/cline/device"):
                login_id = "demo-login-" + str(len(TEAM_DEVICE_LOGINS) + 1)
                TEAM_DEVICE_LOGINS[login_id] = {"name": body["name"], "kind": "cline-pass", "polls": 0}
                self.send_json(200, {"login_id": login_id, "user_code": "TEST-CODE", "verification_uri": "https://auth.cline.bot/device", "expires_at": iso(datetime.now(timezone.utc) + timedelta(minutes=10)), "interval": 5})
            elif route == ("POST", f"{API_BASE}/integrations/commit"):
                if not account or account.get("pending_migration") != body.get("migration_id"):
                    self.send_json(409, {"error": {"message": "Migration not found"}})
                else:
                    account.pop("pending_migration", None)
                    self.send_json(200, {"status": "complete"})
            elif route == ("POST", f"{API_BASE}/integrations/cline/cancel"):
                login = TEAM_DEVICE_LOGINS.get(body["login_id"], {})
                if login.get("account"):
                    self.send_json(200, {"status": "complete", **login["account"]})
                else:
                    login["cancelled"] = True
                    self.send_json(200, {"status": "cancelled"})
            elif route == ("POST", f"{API_BASE}/integrations/cline/poll"):
                login = TEAM_DEVICE_LOGINS[body["login_id"]]
                login["polls"] += 1
                if login["polls"] < 2:
                    self.send_json(200, {"status": "pending", "interval": 5})
                else:
                    login["account"] = team_integration(login)
                    self.send_json(200, {"status": "complete", **login["account"]})
            elif account and self.command == "DELETE":
                TEAM_INTEGRATIONS.remove(account)
                self.send_json(200, {"ok": True})
            elif account and parsed.path.endswith(("/channel", "/refresh")):
                if parsed.path.endswith("/refresh"):
                    account["pending_migration"] = "demo-migration-" + account["id"]
                self.send_json(200, team_response(account))
            elif account and parsed.path.endswith("/query"):
                self.send_json(200, {"account": account, "quota_supported": True, "models": ["gpt-5.4", "claude-opus-4.6"], "quota": {"plan": "Demo subscription", "fetched_at": iso(datetime.now(timezone.utc)), "quota": [quota_row("5-hour limit", 62, 18000, scope="account", window_seconds=18000), quota_row("Weekly limit", 43, 604800, scope="account", window_seconds=604800), {"label": "Credits", "remaining": 5.5, "currency": "USD"}]}})
            else:
                self.send_json(404, {"error": {"message": "Integration not found"}})
            return
        if route == ("POST", f"{API_BASE}/model-tests/prepare"):
            self.send_json(*prepare_dummy_model_test(json.loads(request_body or b"{}")))
        elif route == ("POST", f"{API_BASE}/model-tests/complete"):
            self.send_json(*complete_dummy_model_test(json.loads(request_body or b"{}")))
        elif route == ("PATCH", "/v0/management/auth-files/status"):
            body = json.loads(request_body or b"{}")
            target = next((item for item in CREDENTIALS if "dummy-account-" + item["ref"] == body.get("name") and "dummy-index-" + item["ref"] == body.get("auth_index")), None)
            if target is None or not isinstance(body.get("disabled"), bool):
                self.send_json(404, {"error": {"message": "Account identity changed"}})
                return
            target["disabled"] = body["disabled"]
            for file in AUTH_FILES:
                if file["credential_ref"] == target["ref"]:
                    file["disabled"] = body["disabled"]
            self.send_json(200, {"status": "ok", "disabled": body["disabled"]})
        elif route == ("PATCH", "/v0/management/auth-files/fields"):
            body = json.loads(request_body or b"{}")
            if not isinstance(body.get("name"), str) or not isinstance(body.get("websockets"), bool):
                self.send_json(400, {"error": {"message": "Invalid transport patch"}})
                return
            TURN_STATE_WEBSOCKETS[body["name"]] = body["websockets"]
            self.send_json(200, {"status": "ok"})
        elif route == ("POST", f"{API_BASE}/turn-state/proxies/read"):
            body = json.loads(request_body or b"{}")
            pool, offset = body.get("pool"), body.get("offset", 0)
            if pool not in {"static", "rotating"} or type(offset) is not int or offset < 0:
                self.send_json(400, {"error": {"message": "Invalid proxy page"}})
                return
            revision = turn_state_revision()
            if body.get("revision") and body["revision"] != revision:
                self.send_json(409, {"error": {"message": "Settings changed; reload saved proxies"}})
                return
            proxies = TURN_STATE_CONFIG["probe_proxies" if pool == "static" else "probe_proxies_rotating"]
            page, size = [], 0
            for proxy in proxies[offset:offset + 256]:
                encoded_size = len(json.dumps(proxy).encode()) + 1
                if page and size + encoded_size > 24 * 1024:
                    break
                page.append(proxy)
                size += encoded_size
            next_offset = offset + len(page)
            self.send_json(200, {"pool": pool, "revision": revision, "total": len(proxies), "offset": offset,
                                 "next_offset": next_offset, "done": next_offset == len(proxies), "proxies": page})
        elif route == ("POST", f"{API_BASE}/turn-state/proxies/test"):
            body = json.loads(request_body or b"{}")
            proxy = body.get("proxy", "")
            invalid = any(marker in proxy for marker in ("fail", "invalid", "timeout"))
            inconclusive = "inconclusive" in proxy
            status = "failed" if invalid else "inconclusive" if inconclusive else "reachable"
            reason_key = "proxy_tcp_timeout" if "timeout" in proxy else "proxy_tcp_failed" if invalid else "proxy_tcp_inconclusive" if inconclusive else "proxy_tcp_reachable"
            time.sleep(0.15)
            self.send_json(200, {"proxy": masked_dummy_proxy(proxy), "status": status,
                                 "gateway_ip": "" if invalid or inconclusive else "203.0.113.24", "latency_ms": 150,
                                 "reason": "", "reason_message": {"message_key": "backend.turn_state_" + reason_key}, "deletable": invalid})
        elif parsed.path == f"{API_BASE}/turn-state/config-upload":
            body = json.loads(request_body or b"{}")
            if self.command == "POST":
                upload_id = f"{time.time_ns():032x}"
                TURN_STATE_UPLOADS[upload_id] = {"size": body["size"], "data": b"",
                    "base": json.dumps(TURN_STATE_CONFIG, sort_keys=True)}
                self.send_json(200, {"id": upload_id, "received": 0, "chunk_bytes": 12288})
            elif self.command == "DELETE":
                TURN_STATE_UPLOADS.pop(body["id"], None)
                self.send_json(200, {"cancelled": True})
            elif self.command == "PATCH":
                upload = TURN_STATE_UPLOADS.get(body["id"])
                data = base64.b64decode(body["data"], validate=True)
                if not upload or body["offset"] != len(upload["data"]) or len(data) > 12288:
                    self.send_json(400, {"error": ui_message("backend.turn_state_upload_chunk_invalid")})
                    return
                upload["data"] += data
                self.send_json(200, {"id": body["id"], "received": len(upload["data"]), "chunk_bytes": 12288})
        elif route == ("POST", f"{API_BASE}/turn-state/config-upload/commit"):
            body = json.loads(request_body or b"{}")
            upload = TURN_STATE_UPLOADS.get(body["id"])
            if not upload or len(upload["data"]) != upload["size"]:
                self.send_json(400, {"error": ui_message("backend.turn_state_upload_incomplete")})
                return
            if upload["base"] != json.dumps(TURN_STATE_CONFIG, sort_keys=True):
                self.send_json(400, {"error": ui_message("backend.turn_state_upload_conflict")})
                return
            config = json.loads(upload["data"])
            if config.pop("expected_revision", turn_state_revision()) != turn_state_revision():
                self.send_json(409, {"error": {"message": "Settings changed since proxies were loaded; reload the saved proxies and retry"}})
                return
            if config.get("inject_mode") not in {"always", "replace-only"}:
                self.send_json(400, {"error": ui_message("backend.turn_state_invalid_inject_mode")})
                return
            if not valid_turn_state_pruning(config):
                self.send_json(400, {"error": {"message": "Invalid automatic proxy removal policy"}})
                return
            TURN_STATE_CONFIG.update(config)
            del TURN_STATE_UPLOADS[body["id"]]
            self.send_json(200, turn_state_view())
        elif route == ("PUT", f"{API_BASE}/turn-state"):
            body = json.loads(request_body or b"{}")
            if body.pop("expected_revision", turn_state_revision()) != turn_state_revision():
                self.send_json(409, {"error": {"message": "Settings changed since proxies were loaded; reload the saved proxies and retry"}})
                return
            if body.get("inject_mode") not in {"always", "replace-only"}:
                self.send_json(400, {"error": ui_message("backend.turn_state_invalid_inject_mode")})
                return
            if not valid_turn_state_pruning(body):
                self.send_json(400, {"error": {"message": "Invalid automatic proxy removal policy"}})
                return
            TURN_STATE_CONFIG.update(body)
            self.send_json(200, turn_state_view())
        elif route == ("POST", f"{API_BASE}/turn-state/templates/discard"):
            body = json.loads(request_body or b"{}")
            if body.get("confirm") is not True:
                self.send_json(400, {"error": "confirmation required"})
                return
            selected = next((item for item in TURN_STATE_TEMPLATES if item["account"] == body.get("account") and item["model"] == body.get("model")), None)
            if selected is None or dummy_template_fingerprint(selected) != body.get("fingerprint"):
                self.send_json(409, {"error": {"message": "The template changed or expired; refresh the table before discarding it"}})
                return
            TURN_STATE_TEMPLATES.remove(selected)
            self.send_json(200, turn_state_view())
        elif route == ("DELETE", f"{API_BASE}/turn-state/templates"):
            body = json.loads(request_body or b"{}")
            if body.get("account") and body.get("model"):
                TURN_STATE_TEMPLATES[:] = [item for item in TURN_STATE_TEMPLATES
                                         if item["account"] != body["account"] or item["model"] != body["model"]]
            else:
                TURN_STATE_TEMPLATES.clear()
            self.send_json(200, turn_state_view())
        elif route == ("POST", f"{API_BASE}/turn-state/cooldowns/clear"):
            body = json.loads(request_body or b"{}")
            if body.get("confirm") is not True:
                self.send_json(400, {"error": "confirmation required"})
                return
            self.send_json(200, dict(turn_state_view(), cleared=2))
        elif route == ("POST", f"{API_BASE}/turn-state/self-test"):
            body = json.loads(request_body or b"{}")
            if body.get("confirm") is not True or not body.get("account") or not body.get("model"):
                self.send_json(400, {"error": "scope and confirmation required"})
                return
            self.send_json(200, {"account": body["account"], "model": body["model"], "status": 200,
                                 "length": 312, "reached": True, "harvested": False})
        elif route == ("PUT", f"{API_BASE}/turn-state/runner"):
            body = json.loads(request_body or b"{}")
            if type(body.get("enabled")) is not bool:
                self.send_json(400, {"error": "enabled must be a boolean"})
                return
            if body["enabled"] and (not TURN_STATE_CONFIG["probe_accounts"] or not TURN_STATE_CONFIG["models"]):
                self.send_json(400, {"error": "save collection scope first"})
                return
            TURN_STATE_RUNNER["enabled"] = body["enabled"]
            TURN_STATE_RUNNER["revision"] += 1
            TURN_STATE_RUNNER["phase"] = ("draining" if TURN_STATE_RUNNER["in_flight"] and not body["enabled"] else
                                          "running" if TURN_STATE_RUNNER["in_flight"] else
                                          "offline" if not TURN_STATE_RUNNER["online"] else
                                          "waiting" if body["enabled"] else "stopped")
            self.send_json(200, dict(TURN_STATE_RUNNER))
        elif route == ("POST", f"{API_BASE}/turn-state/probe"):
            if TURN_STATE_RUNNER["enabled"] or TURN_STATE_RUNNER["in_flight"]:
                self.send_json(409, {"error": {"code": "runner_active", "message": "Stop the server collector before a manual probe"}})
                return
            body = json.loads(request_body or b"{}")
            accounts = [body["account"]] if body.get("account") else TURN_STATE_CONFIG["probe_accounts"]
            models = [body["model"]] if body.get("model") else TURN_STATE_CONFIG["models"]
            if not accounts or not models:
                self.send_json(400, {"error": ui_message("backend.turn_state_scope_required")})
                return
            now = datetime.now(timezone.utc)
            fresh = any(item["account"] == accounts[0] and item["model"] == models[0] for item in TURN_STATE_TEMPLATES)
            reason = ui_message("backend.turn_state_fresh" if fresh else "backend.turn_state_harvested")
            result = {"action": "fresh" if fresh else "harvested", "reason": reason["message"],
                      "reason_message": {"message_key": reason["message_key"]},
                      "account": accounts[0], "model": models[0], "next_check_at": iso(now + timedelta(seconds=60))}
            if not fresh:
                budget = turn_state_budget()
                if budget["exhausted"]:
                    reason = ui_message("backend.turn_state_hourly_exhausted")
                    self.send_json(200, {"action": "budget_wait", "reason": reason["message"],
                                        "reason_message": {"message_key": reason["message_key"]}, "next_check_at": budget["resumes_at"]})
                    return
                TURN_STATE_BUDGET_ATTEMPTS.append(time.time())
                TURN_STATE_PROBE_STATS["attempts"] += 1
                static, rotating = TURN_STATE_CONFIG["probe_proxies"], TURN_STATE_CONFIG["probe_proxies_rotating"]
                proxy = next(iter(static or rotating), "")
                result.update({"exit": masked_dummy_proxy(proxy), "proxy_index": 1, "proxy_total": len(static) + len(rotating) or 1,
                               "proxy_pool": "static" if static else "rotating" if rotating else "direct", "proxy_attempt": 1, "length": 292})
                TURN_STATE_PROGRESS.update(result)
                time.sleep(0.6)
                # Predictable failure endpoints are only dummy fixtures for the
                # automatic-removal UI; real transport classification is tested in Go.
                fixture_host = urlparse(proxy).hostname or ""
                failure = "failed" if "probe-fail" in fixture_host else "degraded" if "probe-degraded" in fixture_host else ""
                if failure:
                    TURN_STATE_PROBE_STATS[failure] += 1
                    result.update(action="error" if failure == "failed" else "degraded",
                                  length=0 if failure == "failed" else 312,
                                  reason="Dummy connection failure" if failure == "failed" else "Dummy degraded state",
                                  next_check_at=iso(now + timedelta(seconds=1)))
                    result.pop("reason_message", None)
                    if TURN_STATE_CONFIG["probe_drop_" + failure + "_proxies"]:
                        remaining = len(static) + len(rotating)
                        if remaining <= TURN_STATE_CONFIG["probe_min_proxies"]:
                            result["proxy_disposition"] = "retained_minimum"
                        else:
                            (static if static else rotating).remove(proxy)
                            result["proxy_disposition"] = "removed"
                            result["proxy_config_revision"] = turn_state_revision()
                            remaining -= 1
                        result["proxy_remaining"] = remaining
                else:
                    TURN_STATE_PROBE_STATS["harvested"] += 1
                    TURN_STATE_TEMPLATES.append({"account": accounts[0], "model": models[0], "length": 292,
                                                 "issued_at": iso(now), "expires_at": iso(now + timedelta(seconds=TURN_STATE_CONFIG["ttl_seconds"])),
                                                 "source": "probe", "exit": result["exit"], "harvested_at": iso(now)})
                    TURN_STATE_COUNTERS["learned"] += 1
                TURN_STATE_PROGRESS.clear()
            TURN_STATE_LAST.update(result)
            self.send_json(200, result)
        elif route == ("PUT", f"{API_BASE}/access-control"):
            body = json.loads(request_body or b"{}")
            if any(type(body.get(field)) is not bool for field in ("enabled", "deny_ungrouped")):
                self.send_json(400, {"error": {"message": "invalid access control settings"}})
                return
            ACCESS_CONTROL.update(body)
            self.send_json(200, {"access_control": ACCESS_CONTROL})
        elif route in {("POST", f"{API_BASE}/groups"), ("PATCH", f"{API_BASE}/groups")}:
            body = json.loads(request_body or b"{}")
            group_id = body.get("id") or "group-" + str(time.time_ns())
            group = next((item for item in GROUPS if item["id"] == group_id), None)
            if self.command == "POST":
                group = {"id": group_id, "disabled": False, "routing_mode": "ordinary", "route_ids": [], "rule": empty_rule()}
            if group is None:
                self.send_json(404, {"error": {"message": "group not found"}})
                return
            try:
                rule = normalize_route_rule(body["rule"]) if "rule" in body else group["rule"]
            except ValueError as error:
                self.send_json(400, {"error": {"code": "invalid", "message": str(error)}})
                return
            # Only newly added references must still exist, as on the plugin.
            known = {item["ref"] for item in CREDENTIALS} | set(rule_credential_refs(group["rule"]))
            missing = [ref for ref in rule_credential_refs(rule) if ref not in known]
            if missing:
                self.send_json(400, {"error": {"code": "invalid", "message": "上游凭证已不存在：" + ", ".join(missing)}})
                return
            if self.command == "POST":
                GROUPS.append(group)
            group.update({field: body[field] for field in ("name", "route_ids", "disabled", "routing_mode") if field in body}, rule=rule)
            if "scopes" in body:
                scopes = set(body["scopes"])
                for key in KEYS:
                    ids = [value for value in key.get("group_ids", []) if value != group_id]
                    if key["scope"] in scopes:
                        ids.append(group_id)
                    key["group_ids"] = ids
            self.send_json(200, {"group": group_row(group)})
        elif route == ("DELETE", f"{API_BASE}/groups"):
            group_id = parse_qs(parsed.query).get("id", [""])[0]
            GROUPS[:] = [group for group in GROUPS if group["id"] != group_id]
            for key in KEYS:
                key["group_ids"] = [value for value in key.get("group_ids", []) if value != group_id]
            self.send_json(200, {"deleted": group_id})
        elif route == ("PUT", f"{API_BASE}/keys/groups"):
            body = json.loads(request_body or b"{}")
            scopes = set(body.get("scopes", []))
            ids = body.get("group_ids", [])
            if any(group_id not in {group["id"] for group in GROUPS} for group_id in ids):
                self.send_json(400, {"error": {"message": "unknown group"}})
                return
            for key in KEYS:
                if key["scope"] in scopes:
                    key["group_ids"] = list(ids)
            self.send_json(200, {"updated": len(scopes)})
        elif route == ("POST", "/v0/management/api-call"):
            body = json.loads(request_body or b"{}")
            if body.get("url") == "https://model-test.dummy.invalid/responses":
                prompt = json.loads(body.get("data", "{}")).get("input", "")
                preset = next((item["id"] for item in MODEL_TEST_PRESETS if item["prompt"] == prompt), "free")
                entry = next((item for item in MODEL_TEST_PRESETS if item["id"] == preset), {})
                output = entry.get("expected") or {"code": "function firstUniqueChar(text) { return null; } // Demo response; review manually.",
                          "pelican": '<svg xmlns="http://www.w3.org/2000/svg" width="320" height="180" viewBox="0 0 320 180"><circle cx="90" cy="125" r="35" fill="none" stroke="black"/><circle cx="235" cy="125" r="35" fill="none" stroke="black"/><text x="20" y="30">Dummy pelican preview</text></svg>'}.get(preset, "Dummy model response for your custom prompt.")
                self.send_json(200, {"status_code": 200, "body": json.dumps({"output_text": output, "model": json.loads(body.get("data", "{}")).get("model", "")})})
                return
            auth_index = body.get("auth_index", "")
            auth_file = next((item for item in AUTH_FILES if item["auth_index"] == auth_index), None)
            quota = AUTH_FILE_QUOTAS.get(auth_index)
            if body.get("method") != "POST" or body.get("url") != "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume":
                self.send_json(400, {"error": {"message": "dummy backend: unsupported api-call"}})
            elif auth_file is None or quota is None or auth_file["category"] != "codex":
                self.send_json(404, {"error": {"message": "Codex 认证文件不存在"}})
            elif quota.get("rate_limit_reset_credits_available_count", 0) <= 0:
                self.send_json(200, {"status_code": 409, "body": '{"error":{"message":"No reset credits available"}}'})
            else:
                quota["rate_limit_reset_credits_available_count"] -= 1
                for row in quota["quota"]:
                    row["remaining_percent"] = 100
                self.send_json(200, {"status_code": 204, "body": ""})
        elif route == ("DELETE", f"{API_BASE}/plugin-logs"):
            cleared = len(PLUGIN_LOGS)
            PLUGIN_LOGS.clear()
            self.send_json(200, {"cleared": cleared})
        elif route == ("POST", f"{API_BASE}/prices/reference/refresh"):
            self.send_json(200, {"metadata": price_status()["metadata"], "changed": False})
        elif route == ("PUT", f"{API_BASE}/prices"):
            body = json.loads(request_body or b"{}")
            row = next((price for price in PRICES if price["model_id"] == body.get("model_id")), None)
            if row is None:
                row = {"in_models": False}
                PRICES.append(row)
            row.update(body)
            row["source"] = "custom"
            self.send_json(200, {"price": row})
        elif route == ("DELETE", f"{API_BASE}/prices"):
            model = parse_qs(parsed.query).get("model_id", [""])[0]
            row = next((price for price in PRICES if price["model_id"] == model), None)
            if row is None or row["source"] != "custom":
                self.send_json(404, {"error": {"message": "自定义价不存在"}})
                return
            if not row["in_models"]:
                PRICES.remove(row)
            else:
                row.update(REFERENCE_PRICES.get(model) or {
                    "source": "none", "input_per_1m": 0, "output_per_1m": 0,
                    "cache_read_per_1m": None, "cache_write_per_1m": None,
                    "long_context": None,
                })
            self.send_json(200, {"deleted": model})
        elif route == ("POST", f"{API_BASE}/keys/reset"):
            body = json.loads(request_body or b"{}")
            targets = [key for key in KEYS if not key.get("deleted_at") and key["plan_id"]
                       and (body.get("mode") == "global" or key["scope"] in body.get("scopes", []))]
            counts = {"keys": 0, "windows": 0}
            for key in targets:
                count = len(QUOTA_CYCLES.get(key["scope"], {}))
                counts["keys"] += bool(count)
                counts["windows"] += count
                QUOTA_CYCLES.pop(key["scope"], None)
                refresh_key_quota(key)
            self.send_json(200, counts)
        elif route == ("POST", f"{API_BASE}/keys/concurrency"):
            body = json.loads(request_body or b"{}")
            for key in KEYS:
                if key["scope"] == body.get("scope"):
                    key["concurrency_limit"] = body.get("concurrency_limit", 0)
                    break
            self.send_json(200, {"ok": True})
        elif route == ("POST", f"{API_BASE}/keys/sync"):
            self.send_json(200, {"added": 0, "deleted": 0})
        elif route == ("POST", f"{API_BASE}/credentials/sync"):
            body = json.loads(request_body or b"{}")
            CREDENTIALS[:] = [
                item for item in CREDENTIALS
                if item["ref"] not in SYNCED_CREDENTIAL_REFS
            ]
            SYNCED_CREDENTIAL_REFS.clear()
            for item in body.get("credentials", []):
                ref = item["ref"]
                SYNCED_CREDENTIAL_REFS.add(ref)
                CREDENTIALS.append({
                    "ref": ref,
                    "source": "ai-providers",
                    "provider": item["provider"],
                    "display_name": item["display_name"],
                    "status": "disabled" if item.get("disabled") else "active",
                    "disabled": bool(item.get("disabled")),
                    "unavailable": False,
                })
            self.send_json(200, {"credentials": CREDENTIALS})
        elif route == ("DELETE", f"{API_BASE}/routes"):
            route_id = parse_qs(parsed.query).get("id", [""])[0]
            if any(route_id in group["route_ids"] for group in GROUPS):
                self.send_json(409, {"error": {"message": "请先从分组中解除此路由规则"}})
                return
            affected = 0
            unrestricted = 0
            deleted = 0
            ROUTES[:] = [item for item in ROUTES if item["id"] != route_id]
            for key in KEYS:
                route_ids = key["route_bindings"]["route_ids"]
                if route_id in route_ids:
                    key["route_bindings"]["route_ids"] = [item for item in route_ids if item != route_id]
                    affected += 1
                    deleted += bool(key.get("deleted_at"))
                    unrestricted += not key.get("deleted_at") and not any(key["route_bindings"].values())
            self.send_json(200, {"deleted": route_id, "affected_keys": affected, "deleted_keys": deleted, "fully_unrestricted_keys": unrestricted})
        elif route == ("POST", f"{API_BASE}/routes"):
            body = json.loads(request_body or b"{}")
            route_id = f"route-dummy-{len(ROUTES)}"
            scopes = set(body.get("scopes", []))
            stored = {"id": route_id, "name": body.get("name", "新路由"), "rule": body.get("rule", {})}
            ROUTES.append(stored)
            for key in KEYS:
                if key["scope"] in scopes:
                    key["route_bindings"]["route_ids"].append(route_id)
            self.send_json(201, {"route": stored})
        elif route == ("PATCH", f"{API_BASE}/routes"):
            body = json.loads(request_body or b"{}")
            route_id = body.get("id", "")
            stored = next((item for item in ROUTES if item["id"] == route_id), None)
            if stored is None:
                self.send_json(404, {"error": {"message": "dummy backend: route not found"}})
                return
            stored.update({key: body[key] for key in ("name", "rule") if key in body})
            if "scopes" in body:
                scopes = set(body["scopes"])
                for key in KEYS:
                    route_ids = [item for item in key["route_bindings"]["route_ids"] if item != route_id]
                    if key["scope"] in scopes:
                        route_ids.append(route_id)
                    key["route_bindings"]["route_ids"] = route_ids
            refresh_route_counts()
            self.send_json(200, {"route": stored})
        elif route in {("POST", f"{API_BASE}/plans"), ("PATCH", f"{API_BASE}/plans")}:
            body = json.loads(request_body or b"{}")
            plan_id = body.get("id") or "plan-" + str(time.time_ns())
            stored = next((item for item in PLANS if item["id"] == plan_id), None)
            if self.command == "POST":
                stored = {"id": plan_id}
                PLANS.append(stored)
            if stored is None:
                self.send_json(404, {"error": {"message": "dummy backend: plan not found"}})
                return
            stored.update({key: body[key] for key in ("name", "windows") if key in body})
            for index, window in enumerate(stored["windows"]):
                window.setdefault("id", str(time.time_ns()) + "-" + str(index))
            stored["windows"].sort(key=lambda window: window["period_seconds"])
            if "scopes" in body:
                scopes = set(body["scopes"])
                for key in KEYS:
                    if key["scope"] in scopes and key["plan_id"] != plan_id:
                        key["plan_id"] = plan_id
                        QUOTA_CYCLES.pop(key["scope"], None)
                    elif key["scope"] not in scopes and key["plan_id"] == plan_id:
                        key["plan_id"] = ""
                        QUOTA_CYCLES.pop(key["scope"], None)
            for key in KEYS:
                refresh_key_quota(key)
            self.send_json(201 if self.command == "POST" else 200, {"plan": stored})
        elif route == ("PUT", f"{API_BASE}/keys/routes"):
            body = json.loads(request_body or b"{}")
            bindings = body.get("bindings", {})
            for key in KEYS:
                if key["scope"] == body.get("scope"):
                    key["route_bindings"] = {
                        "configured": True,
                        "route_ids": bindings.get("route_ids", []),
                        "models": bindings.get("models", []),
                        "credential_ids": bindings.get("credential_ids", []),
                        "credential_providers": bindings.get("credential_providers", []),
                        "denied_models": bindings.get("denied_models", []),
                        "denied_credential_ids": bindings.get("denied_credential_ids", []),
                        "denied_credential_providers": bindings.get("denied_credential_providers", []),
                    }
                    break
            refresh_route_counts()
            self.send_json(200, {"ok": True})
        elif route == ("DELETE", f"{API_BASE}/plans"):
            plan_id = parse_qs(parsed.query).get("id", [""])[0]
            PLANS[:] = [plan for plan in PLANS if plan["id"] != plan_id]
            for key in KEYS:
                if key["plan_id"] == plan_id:
                    key["plan_id"] = ""
                    refresh_key_quota(key)
            self.send_json(200, {"deleted": plan_id})
        elif route in {("POST", f"{API_BASE}/keys/bind"), ("POST", f"{API_BASE}/keys/unbind")}:
            body = json.loads(request_body or b"{}")
            for key in KEYS:
                if key["scope"] == body.get("scope"):
                    plan_id = body.get("plan_id", "")
                    if key["plan_id"] != plan_id:
                        key["plan_id"] = plan_id
                        QUOTA_CYCLES.pop(key["scope"], None)
                        refresh_key_quota(key)
            self.send_json(200, {"ok": True})
        elif route == ("POST", f"{API_BASE}/keys/label"):
            body = json.loads(request_body or b"{}")
            for key in KEYS:
                if key["scope"] == body.get("scope"):
                    key["label"] = body.get("label", "").strip()
                    break
            self.send_json(200, {"ok": True})
        else:
            self.send_json(404, {"error": {"message": "dummy backend: route not found"}})

    def log_message(self, message, *args):
        print(f"{self.address_string()} - {message % args}")


def main():
    parser = argparse.ArgumentParser(description="Serve the billing UI with deterministic dummy data.")
    parser.add_argument("--port", type=int, default=8765)
    parser.add_argument(
        "--host",
        choices=("standalone", "cpamc", "cpamp"),
        default="standalone",
        help="Wrap the plugin UI in a lightweight CPAMC or CPAMP host shell.",
    )
    parser.add_argument(
        "--theme",
        choices=("auto", "light", "white", "dark"),
        default="auto",
        help="Initial host theme; the preview shell can switch themes after startup.",
    )
    args = parser.parse_args()
    seed_paginated_history()
    Handler.host_mode = args.host
    Handler.initial_theme = args.theme
    server = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    entry_path = "/ui" if args.host == "standalone" else "/"
    print(
        f"Frontend dummy backend ({args.host}): http://127.0.0.1:{server.server_port}{entry_path}",
        flush=True,
    )
    if args.host != "standalone":
        print(f"Direct plugin document: http://127.0.0.1:{server.server_port}/ui", flush=True)
    print(f"API Key account page: http://127.0.0.1:{server.server_port}/ui#account", flush=True)
    print("Data is reset on every restart. Press Ctrl-C to stop.", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
