<div align="center">
  <h1>API Key Team Manager</h1>
  <p><strong>API key billing, subscription quotas, group access, and Codex Turn State for <a href="https://github.com/router-for-me/CLIProxyAPI">CLIProxyAPI</a>.</strong></p>
  <p>
    <a href="https://github.com/CHIMOOO/cpa-plugin-key-billing-manager/releases/latest"><img src="https://img.shields.io/github/v/release/CHIMOOO/cpa-plugin-key-billing-manager?label=release" alt="Latest release"></a>
    <a href="https://github.com/CHIMOOO/cpa-plugin-key-billing-manager/actions/workflows/check.yml"><img src="https://github.com/CHIMOOO/cpa-plugin-key-billing-manager/actions/workflows/check.yml/badge.svg" alt="CI status"></a>
    <a href="./LICENSE"><img src="https://img.shields.io/badge/license-MIT-green" alt="MIT License"></a>
  </p>
  <p><strong>English</strong> · <a href="./README.md">简体中文</a></p>
</div>

**Plugin ID: `cpa-team-manager` · Store name: API Key 团队管理 (API Key Team Manager) · Releases: [CHIMOOO/cpa-plugin-key-billing-manager](https://github.com/CHIMOOO/cpa-plugin-key-billing-manager)**

This independent fork of [haowang02/cpa-plugin-key-billing](https://github.com/haowang02/cpa-plugin-key-billing) extends downstream billing, subscriptions, and routing with team groups, account integrations and concurrency, upstream quota queries, local risk rules, and Codex Turn State. The original MIT copyright notice is retained. This project's `v0.0.x` releases and the original project's versions are separate release lines.

[Install](#installation) · [Migrate older releases](#migrating-this-projects-v007-or-earlier) · [Account integrations](#account-integrations-and-concurrency) · [Codex Turn State](#codex-turn-state) · [Changelog](Changelog.md)

## Feature guide

| Page | Capabilities |
| --- | --- |
| API Key / API Key Groups | Per-key concurrency and model/account permissions; multiple groups per key; ordinary/exclusive/common pools; group and account switches |
| Subscription Plans | Spending, token, and request limits; all-model, exact-model, or model-family pools; independent or shared reset schedules |
| Account Integrations | OpenCode Go / Zen, CommandCode, and Cline Pass; publish supported models and query upstream-provided subscription quotas |
| Accounts | Per-account requests, successes/failures, tokens, cost, current concurrency, and concurrency limits |
| Codex Turn State | Account/model-specific collection, renewal, injection, and valid-bucket protection; static/rotating proxies, ten concurrent connectivity checks, optional automatic removal |
| Model Tests | Concurrent diagnostics across accounts with repeated runs; per-account pass counts, declared-model consistency and timing; proxied accounts marked in the list, animated SVG previews, and no execution of generated code |
| Risk Center | Local keyword/model rules, observe/pre-block modes, and hash memory without persisting prompt text |
| Traffic Capture | Watch several upstream accounts at once, pausing each on its own, with live headers, bodies, responses and the upstream response model; memory only, credentials and body text masked by default, downloadable as text |
| Analysis / Request Events / Error Events | Host-reported usage, cost, latency, and failures; billing is never inferred from raw response bodies |
| Settings | Routing, prices, long-context tiers, logs, and persistence risk diagnostics |

English and Simplified Chinese are supported. Standalone pages have a language selector; embedded pages follow CPAMC / CPAMP. API key holders can use a separate page to view their own subscriptions and usage.

## Installation

### Requirements

- CLIProxyAPI (CPA) **7.2.143 or later**, built with plugin support. No-plugin builds cannot load this library.
- **For Codex Turn State, use CPA 7.3.4 or an equivalent request-header forwarding fix, with HTTP/SSE.** The 7.2.143 Codex executor drops the header, so plugin injection counters alone cannot prove delivery upstream. Disable upstream WebSocket for protected accounts; downstream WebSocket clients can then use CPA's HTTP/SSE bridge.
- Release packages cover Windows, macOS, and Linux on amd64 / arm64. Probing uses Go's built-in HTTP client; no server-side `curl` installation is required.
- Users of this fork's v0.0.7 or earlier should follow the old-ID migration below before installing, so a new default database is not mistaken for lost data.

### Recommended: add this project's third-party registry

Add the **raw registry URL**, not the repository homepage:

```text
https://raw.githubusercontent.com/CHIMOOO/cpa-plugin-key-billing-manager/main/registry.json
```

1. Sign in to your management panel with the management key and open its configuration editor. Current panels expose the source input here:

   | Panel | Registry input |
   | --- | --- |
   | CLI Proxy API Management Center (CPAMC) | Configuration → Visual → Advanced & Experimental → Plugins → Third-party plugin sources (`plugins.store-sources`) |
   | CPA Manager Plus (CPAMP) | Configuration / Configuration file → Visual → System configuration → Plugin store sources; one URL per line |

2. Append the URL without removing existing sources, enable the plugin system, and save the configuration.
3. Open **Plugin Store**, refresh, and search for `cpa-team-manager` or **API Key 团队管理**. Verify author **CHIMOOO** and this repository, choose a published release, and install it.
4. Under **Plugins / Installed plugins**, enable this plugin and configure `state_file` as described below. Both the global and per-plugin switches must be enabled. Restart the actual CPA service or container when prompted; refreshing the browser does not apply a library upgrade.
5. Open the plugin's **API Key 团队管理** menu/resource and check its footer version. A direct resource URL is provided below if the menu is unavailable.

The input locations are verified against the current [CPAMC configuration source](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/main/src/features/config/components/sections/SectionAdvanced.tsx) and [CPAMP configuration source](https://github.com/seakee/CPA-Manager-Plus/blob/main/apps/web/src/components/config/VisualConfigEditor.tsx). Names can vary between panel versions. If your panel lacks the input, merge this into the existing `plugins` section of `config.yaml`, retaining other settings:

```yaml
plugins:
  enabled: true
  store-sources:
    - https://raw.githubusercontent.com/CHIMOOO/cpa-plugin-key-billing-manager/main/registry.json
```

The host retains its built-in official source. Adding this registry does not depend on official-store acceptance. `registry.json` describes published releases; it neither builds workspace changes nor loads source files. See the local build instructions below for unreleased changes and the [CPAMP plugin manual](https://github.com/seakee/CPA-Manager-Plus/blob/main/apps/docs/en/manual/plugins.md) for restart/resource-path behavior.

### Manual installation

Stop CPA and run the installer from its working directory. Linux / macOS:

```sh
curl -LsSf https://raw.githubusercontent.com/CHIMOOO/cpa-plugin-key-billing-manager/main/install.sh | sh
```

Windows PowerShell:

```powershell
irm https://raw.githubusercontent.com/CHIMOOO/cpa-plugin-key-billing-manager/main/install.ps1 | iex
```

The scripts install this repository's published library into the current directory's `plugins/`. Alternatively, download the matching OS/architecture package from [Releases](https://github.com/CHIMOOO/cpa-plugin-key-billing-manager/releases/latest), verify it against that release's `checksums.txt`, and extract the library:

```text
plugins/cpa-team-manager.so       # Linux
plugins/cpa-team-manager.dylib    # macOS
plugins/cpa-team-manager.dll      # Windows
```

If you use a custom `plugins.dir`, put the library in that directory. Save the configuration and start CPA; library upgrades also require a restart. Avoid duplicate copies of the same plugin ID in the root and platform subdirectories.

### Migrating this project's v0.0.7 or earlier

Starting with **v0.0.8**, this fork changed its plugin ID, configuration key, library name, and resource routes from `cpa-key-billing` to `cpa-team-manager` to distinguish it from the original project. These steps concern older releases of this fork; they do not imply arbitrary upstream releases are interchangeable.

1. **Stop CPA and back up** `config.yaml`, the original `state_file` database, and its adjacent `.turn-state.json` plus `.turn-state.json.runtime.json` if present. Include `.account-runtime.json` and `.risk-control.json` if present.
2. Move this fork's old `cpa-key-billing.so`, `.dylib`, or `.dll` out of the loading directory. Do not load both copies of the billing and scheduling hooks.
3. Rename this project's key under `plugins.configs` to `cpa-team-manager` and **retain the exact original `state_file` path**, for example `plugins/cpa-key-billing-state-v1.db`. Do not rename the database or its adjacent state files. The new ID defaults to a new path and does not automatically adopt old data.
4. Install the new library, start CPA, and verify existing groups, subscriptions, usage, and buckets at the new URL. Credential fingerprints remain compatible, preserving bindings.

Run one plugin responsible for these billing, scheduling, and restriction hooks in a CPA instance. Never let different plugins share an active database. To roll back, stop CPA and restore the pre-upgrade database, state files, and matching old configuration before starting the old library.

## Configuration

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cpa-team-manager:
      enabled: true
      debug: false
      codex_fast_mode_billing: false # Charge 2.5 times for Codex priority requests
      state_file: "plugins/cpa-team-manager-state-v1.db"
```

When `codex_fast_mode_billing` is enabled, Codex upstream requests with `service_tier=priority` are billed at **2.5 times** the standard cost.

Compatibility is determined by the data format, not by confusing upstream release numbers with this fork's versions. Current code uses SQLite schema v20 for group pool roles. Supported, structurally valid v10–v19 databases migrate with their data intact; existing groups default to ordinary. Earlier JSON files and unknown/incompatible SQLite formats are not promised automatic migration: retain a backup and use a new `state_file` on an unsupported-format error rather than forcing its schema version. Older plugins must not open a migrated database; restore the backup when rolling back.

Stop CPA before backing up these files:

| File | Data |
| --- | --- |
| SQLite database at `state_file` | Keys, groups, plans, billing, and usage history |
| `<state_file>.turn-state.json` | Proxies, collection settings, and the runtime baseline |
| `<state_file>.turn-state.json.runtime.json` | Latest durable templates and cooldowns; back up together with the baseline |
| `<state_file>.turn-state-runner.json` | Persisted collection start/stop intent for process restart recovery |
| `<state_file>.account-runtime.json` | Per-account concurrency and bucket-protection settings |
| `<state_file>.risk-control.json` | Risk rules, events, and hash memory |
| CPA configuration and `auth-dir` | Provider channels, integration credentials, and pending credential migrations |

## Access

Open the plugin in the management panel or visit:

```text
http(s)://<CLIProxyAPI address>/v0/resource/plugins/cpa-team-manager/ui
```

API key holders can view their own subscription and usage at:

```text
http(s)://<CLIProxyAPI address>/v0/resource/plugins/cpa-team-manager/ui#account
```

### Persistence diagnostics

On detectable Linux container deployments, **Settings** checks the loaded library, billing database, and plugin data files against their filesystem mounts. Only risks such as container writable layers, memory filesystems, and missing loaded libraries are displayed, with affected paths. Healthy external mounts are hidden, and no other page displays this module. Recreating a container may lose its writable layer; an ordinary CPA process restart is different. Losing the plugin or its data may disable group and quota enforcement.

Back up the files, mount plugin/data directories persistently, and retain plugin loading settings in deployment configuration. CPA provides no safe callback to change container mounts, so the plugin explains the required action instead of offering an unreliable automatic fix. An observed external mount does not prove autoload, future deployments, or backups are correct. Undetectable deployments show no inferred conclusion.

## Billing and quotas

- Keys without a subscription plan have usage recorded without subscription quota limits.
- Plans can contain multiple quota windows, each limiting spending, tokens, requests, or a combination.
- A pool can cover all models, one or several exact model IDs, or OpenAI, Claude, xAI, Gemini, DeepSeek, or Qwen model families. Models selected in one pool share its balance. Create separate pools for independent limits, even with the same reset period: for example, `gpt-6-astra` $100/day and `gpt-5.5` $200/day.
- Families match the client-visible model name, such as GPT/o for OpenAI or Grok for xAI, not the CPA compatibility provider. Use exact full IDs for arbitrary custom aliases. Unmatched models remain subject to all-model pools and routing rules; other scoped pools do not limit them.
- All matching pools accumulate and enforce their limits. An all-model $500 pool can coexist with a Claude $200 pool without creating duplicate billing records. Exhausting the Claude pool does not block other model families with available quota. Changing a pool's scope resets its usage; changing its limit preserves usage.
- Plans containing scoped pools require an explicit model. Dynamic `auto` requests, including thinking suffixes, can have different model identities at CPA admission and usage time; they receive `503 quota_model_unresolved` to prevent unaccounted usage. Plans containing only all-model pools still support `auto`.
- Each key is billed separately. Independent cycles begin with the first admitted request; shared cycles follow the plan's schedule.
- Manual resets preserve shared reset times. Independent cycles restart on the next admitted request.
- Custom prices override models.dev reference prices. If neither exists, requests are allowed with zero monetary cost; token and request quotas still apply.
- Request events are retained for 365 days, including failed and zero-usage records.

## Groups and routing

Use **API Key Groups** to manage groups, membership, access control, and the ungrouped-key policy. Groups can bind routing rules and/or directly select credentials and models. A key may belong to multiple groups and also have its own routing rules or direct permissions.

Click a model or credential row to cycle through unselected, allowed, and denied; Enter and Space work as well. No status dropdown is needed. Advanced dynamic category rules and API-key assignments are expanded by default. Without an enabled exclusive group, permissions from all enabled groups and the key itself are combined: allowlists are unioned, denylists are unioned, and denials win. An empty model allowlist does not restrict models. **For a managed key, an empty credential allowlist grants no upstream access.** Empty groups grant no access. In ordinary mode, an empty combined credential allowlist denies access. In exclusive mode, a key rule left completely empty (for example after Clear) inherits the groups; a direct rule that still lists anything constrains them.

Selecting a credential category authorizes current and future credentials in that category. Select individual files instead when only specific accounts should be allowed. Searches and category filters do not clear hidden selections; bulk selection affects only the currently displayed individual credentials.

**Enable access control** is on by default. Turning it off suspends group, model, and credential restrictions while preserving CPA authentication, billing, quotas, and concurrency limits. **Reject all ungrouped API keys** is off by default; turning it on rejects both existing and future ungrouped keys while access control is enabled.

The group editor offers three pool roles:

| Role | Effect on bound keys |
| --- | --- |
| Ordinary (default) | Participates unless the key has an enabled exclusive group |
| Exclusive | Only this group and the key's explicitly bound enabled common groups provide the pool |
| Common | Remains eligible alongside an exclusive group; never grants access to unbound keys |

For a key bound to group 1 and group 2, exclusive + ordinary uses only group 1; exclusive + common can use both pools. Without an exclusive group, the original union behavior remains. A key may have only one enabled exclusive group; conflicting creation, editing, enabling, or bulk binding is rejected atomically. Direct key credential/model allowlists narrow the exclusive/common union by intersection, and denials still win; direct rules cannot expand that pool. A key with untouched direct routing inherits the selected groups. Once direct routing is configured with any entry, its credential grants form the required intersection; model-only or deny-only direct configurations grant no credentials. A completely empty direct configuration (Clear) inherits the groups again; to block a key, disable it or add a denial instead. Turning access control off also pauses these restrictions.

Disabling a group preserves its members and rules but excludes its grants and denials. A key belonging only to disabled groups is denied. When it also belongs to enabled groups, those groups continue to apply. Existing groups default to enabled after migration.

Scheduling merges group permissions before selecting from CPA's current candidates for the requested model. A disabled account in one group cannot hide an available authorized account in another. Reenabled accounts follow the host's current state. Group grants do not enable accounts or bypass zero weights, model cooldowns, or denials. If no authorized candidate is available, the request fails instead of using an unauthorized account.

The plugin also validates the final credential chosen by CPA. Host priority settings can narrow the candidate set; when a host scheduling mode bypasses the plugin's selection, the final check rejects unauthorized credentials rather than choosing another itself. Category permissions require an identifiable category; use explicit credentials if the host cannot supply that information.

The former X-Forwarded-For interception feature and management endpoints have been removed. Historical database settings remain for compatibility and no longer affect requests.

Group account pools use compact cards with no upstream quota query or display. Account switches update CPA globally across all bound groups; new requests exclude disabled accounts while in-flight responses may finish. The UI verifies the saved state using exact identity and auth index. Physical files and verified native API keys use the host account-status endpoint. Native keys add/remove the exact excluded-models wildcard while preserving other exclusions. Zero weight is displayed separately; enabling does not change weight. OpenAI compatibility and virtual children without a safe per-account operation show a disabled control and direct users to the original management page; the plugin never silently disables their entire provider.

## Account integrations and concurrency

**Account Integrations** supports OpenCode Go / Zen, CommandCode API keys, and Cline Pass. Cline supports device authorization and existing credentials; the page drives login and refresh. Secrets are stored in CPA-managed auth files, not the plugin database. Persist and back up CPA's `auth-dir` and provider configuration.

Publish selected models into the account's CPA channels. OpenCode models are split by verified protocol into Chat Completions, Responses, and Anthropic providers. Native providers use unique client model prefixes, which the page can copy. OpenCode Google endpoints cannot currently be expressed through the host's provider configuration; those models and unknown protocols are visibly excluded. Publishing/deleting preserves unrelated channels. Resources update separately, with partial failures reported for retry.

Key replacement and manual Cline refresh first prepare exact group, route, and key credential references and merge old/new credentials into one concurrency pool. Old references retire only after all channels are updated and confirmed. Use **Connect / Repair channels** after an interruption; retrying resumes the pending migration without rotating again. Pending records live in CPA's auth directory. Cancelling device login stops subsequent channel writes; if the account was already saved, the page reports that outcome and lets you delete it.

If the upstream rotates a token but CPA auth storage cannot save it, the new token is retained only in process memory. Restore storage and repair the channel before restarting; a process exit in this state may require signing in again.

| Integration | Quota support |
| --- | --- |
| OpenCode Go | Upstream subscription windows using the workspace ID and dashboard auth cookie |
| OpenCode Zen | Model access; prepaid balance is currently unavailable |
| CommandCode | Organization subscription limits and credits from the official CLI endpoints |
| Cline Pass | Official subscription usage limits; only subscription-eligible models are published |

Refresh quotas on demand. Only returned windows, percentages, or balances are displayed; missing 5-hour/weekly limits are omitted, and failures never imply a full balance. Group pools no longer query or display upstream quota; use Account Integrations or Auth Files for those queries. Upstream subscription allowances are separate from this plugin's downstream quota plans.

**Accounts** aggregates the last 365 days of `usage.handle` records by the host's exact account index, including requests, failures, classified tokens, cost, and current concurrency. Missing historical token classifications remain marked incomplete. Some CPA versions omit configured API keys from auth-file inventory: limits can still be set before use, but usage identity requires the first real request, potentially again after restart. Unknown usage is not displayed as zero.

Per-account concurrency limits range from 0–1000; 0 is unlimited. Both upstream-account and downstream-key limits apply. Streaming requests hold their slot until completion, disconnection, or a retry switches accounts. Back up `<state_file>.account-runtime.json` with the database.

## Model tests

**Model Tests** compares a selected account/model's response to a single prompt. It sends a **diagnostic request through CPA's management API**, not the complete downstream API-key path, so it does not replace business tests of key groups, routing, and subscription limits. It consumes upstream quota. This path emits no `usage.handle` record, creates no downstream usage/billing entry, and does not present response-reported token counts, cost, or TTFT as authoritative telemetry.

Model tests require a **CPA 7.3.4-compatible host with schema 6**. Supported accounts are exactly identifiable **Codex OAuth auth files** and CPA-native **OpenAI-compatible, Codex / Responses, Claude, and Gemini API-key channels**. Configured accounts are matched against a freshly read native management configuration, their account index, and an exact fingerprint; older hosts may omit them from the auth-file inventory, so that inventory is not the only source. Other OAuth types such as Claude or Antigravity, disabled accounts, missing/duplicate indexes, and unsupported custom headers or protocols are refused. Channels published by Account Integrations must satisfy the same requirements; credential-storage helper records cannot execute requests.

The page runs top to bottom: test setup, test accounts, then the comparison.

1. **Setup:** enter the actual upstream model ID, choose a preset, and set **Runs per account** (1–100) and **Concurrent requests** (1–8). Configured channels provide known models and aliases; every run rereads the configuration and resolves an unambiguous alias. Editing a preset turns it into a custom test without the original check. Prompts are limited to 8192 UTF-8 bytes.
2. **Accounts:** filter by provider, search, and select up to 50 accounts and at most 1000 requests in total (accounts × runs). **Accounts with their own proxy show a "Proxy host:port" tag**, and a note above the list says whether other accounts use the global proxy or connect directly.
3. **Execution:** different accounts run in parallel up to the concurrency limit; **each account sends one request at a time and repeats its runs in order**, which respects its own concurrency limit and shows consecutive behavior. Failures are not retried. Stopping or leaving the tab starts no new requests and lets running ones finish. The plugin keeps at most eight tests in flight.
4. **Comparison:** a summary has one row per account with a strip of per-run results (click one to open that run), the pass count, each declared model with its count, and the average time. Below it, every run is listed with its status, declared model, time and the last line of the answer; filter by account, failures or model mismatches, and click a row for the full answer, checks, model details, proxy route and prompt.
5. Results stay only in the page's memory and are replaced by the next test. A passed check only shows that this answer met the task; it does not verify overall intelligence or actual model identity.

Presets: the **candy guarantee puzzle** (the original from [codex-candy-eval](https://github.com/haowang02/codex-candy-eval/blob/main/codex_candy_eval.py)) is checked automatically: the last line must give the final answer 21. **Pelican riding a bicycle** (after [Simon Willison](https://simonwillison.net/2024/Oct/25/pelicans-on-a-bicycle/)) asks for an animated SVG, reviewed by hand; the page plays it as an isolated image with scripts, styles and external resources removed, and can replay it. A custom prompt is compared by hand.

**The network route is explicit: account proxy → global proxy → explicit direct connection.** An account explicitly configured for direct access overrides the global proxy. Every run rereads the current settings and displays the selected source and redacted endpoint. Unverified global settings, invalid proxies, or connection failures never silently fall back to direct access.

The plugin applies local risk rules, valid-State checks, and per-account concurrency to the diagnostic. Preparation has a short send deadline; expired preparations require a new test rather than using a nearly expired template. Normal completion releases the slot. If the management connection fails, the upstream may still be running, so the slot remains conservatively reserved until the bounded lease expires and a later host call cleans it up. No background test loop is created.

Ordinary status APIs do not return raw State values. Preparing a single administrator test may temporarily return the State header required by that request. The page uses it for that call without saving it, complete proxy credentials, or API keys in test history. Response bodies are used only to extract/display model text, never to create business billing or failure telemetry.

## Risk center

Local keyword checks are disabled by default and can be scoped to models. Observe mode records matches; pre-block mode refuses matches before upstream execution. Pre-block also refuses uninspectable payloads, including missing, unsupported, or oversized input. Inspection accepts up to 1 MiB JSON and 64 KiB recognized text; it does not inspect image/audio content or fetch external links.

Optional hash memory recognizes the same normalized text after an earlier match. Events store timestamps, models, rule references, and caller references, never prompts, excerpts, or credentials. Defaults retain 30 days and up to 500 events; counters describe retained events. Hash memory can be cleared separately. Back up `<state_file>.risk-control.json`. This is local rule enforcement, without external AI moderation or a guarantee against upstream account restrictions.

## Traffic capture

Traffic capture shows what upstream accounts actually receive. Search the account list and tick one or more accounts to start listening; each account can be paused, resumed or stopped on its own. The page polls every 1.5 seconds and lists the requests scheduled to those accounts with the account, the requested model and the model declared in the upstream response. Open one to see its path, headers, body, and the response headers and body; streamed responses grow chunk by chunk. The request, the upstream body and the response can each be copied or downloaded as `.txt`, and a full record downloads all of them together. Requests retried on another account are marked; when that account is watched too, the retry becomes an entry of its own.

- **Memory only:** nothing is written to disk or logs. Up to 100 requests are kept, each body is truncated at 2 MiB, and the oldest entries are evicted beyond 48 MiB in total. A CPA restart or Clear removes them.
- **Stops by itself:** capture continues while the page stays open and stops about 45 seconds after you leave it. The plugin runs no background timers.
- **Redaction:** values of `Authorization`, `Cookie`, `X-Api-Key`, and headers whose names contain key, token, secret, password, or signature become `[redacted]`. On the page, request and response bodies keep only their protocol structure (types, roles, models, IDs and similar) by default, and user input, output, prompts and tool text show as `***`; downloads are masked the same way. Plugin memory still holds the original text, so enable capture only while troubleshooting.
- **Scope:** entries show what the plugin hooks see after the account is chosen: the client path, headers, and body, plus the body sent upstream after protocol conversion when response hooks are registered. Upstream credentials and the final HTTP message built inside CPA's executor do not pass through the plugin.
- **Responses need response hooks:** request-only capture adds no hooks. CPA reads hook declarations only when it loads or reconfigures the plugin, so switching "Register response hooks to capture responses" makes the page save the plugin configuration unchanged, which has CPA re-register the plugin (the same as saving it in the management center). If that does not take effect, use "Reload plugin to apply" or restart CPA. While it is on, every streamed chunk then passes through the plugin, which returns after one quick check when nothing is being captured. When Codex Turn State is on, the response hooks are already registered. The switch is stored in `<state_file>.traffic-capture.json`.

## Codex Turn State

The **Force Astra** switch on the State page adapts `ccodex-rotate`'s forced-model feature. Save to route explicit GPT/Codex text model requests through Codex accounts to `gpt-6-astra`, preserving reasoning effort. The setting survives restarts; disabling it restores normal host routing. It works independently of State injection and observation mode. To inject State as well, collect an Astra template for each account in use. Custom aliases, other providers, image/audio requests, collection and quality probes retain their original routes. Requires CLIProxyAPI v7.3.4-compatible provider model routing; billing follows host usage records.

Automatic collection requires both the updated plugin and the standalone [server collector](deploy/README.state-collector.md), managed by systemd or Docker. v0.0.9 uses browser scheduling and does not gain background collection by installing only the collector. Upgrading the plugin without installing the collector shows an offline state; the browser does not silently fall back to probing.

Configure the module in its own **Codex Turn State** tab. It is adapted from [arden-aaai/cpa-plugin-codex-turn-state](https://github.com/arden-aaai/cpa-plugin-codex-turn-state), MIT License, Copyright © 2026 boooot. The account/model template algorithm, injection modes, and proxy retry policy are adapted to this plugin's synchronous execution and separate state store. See [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) for the reference revision and full license.

The module is disabled by default. Saved settings apply immediately; replacing the plugin library still requires a CPA restart.

**Enrolling an account for collection enables business protection by default.** Business requests for the selected models require a valid template for the selected account and actual upstream model; other models on the account skip State. Missing/expired buckets, disabled injection, dry-run, and headers that `replace-only` cannot replace cause rejection. Other accounts follow their own rules. Protection is a separate immediately saved switch; disabling it requires confirmation. `always` supports clients without a template header but still requires a valid bucket.

Protected accounts require a verified CPA 7.3.4-compatible host with header forwarding. Host schema 6 is a minimum marker, not proof that every fork contains the fix. Unknown selected-account metadata or an older host fails closed. Protected accounts must use upstream HTTP/SSE; downstream WebSocket clients may remain connected to CPA. Saving collection settings or starting probing uses CPA's existing per-account management PATCH to turn off upstream WebSocket and reads back the result before continuing. It preserves account enablement, OAuth credentials and global proxies; removing the account from collection does not re-enable upstream WebSocket.

CPA bridges each downstream WS turn to HTTP/SSE, allowing the current State header to be injected per turn. The plugin verifies exact runtime identity and upstream mode through a local host callback on protected WS requests; unknown identity or an upstream WS mode change fails closed. Existing connection-bound continuations may need one reconnect, and concurrent external account edits cannot be made atomic by this plugin. A real CPA v7.3.8 test completed two turns over one downstream WS using HTTP upstream and a renewed State header on the second turn.

If transport setup fails, State settings are not applied and the draft is retained. Accounts already changed remain on HTTP; retry completes the rest.

| Setting | Behavior |
| --- | --- |
| `replace-only` (default) | Inject only when the incoming header matches the replacement length, default 312 |
| `always` | Inject a matching valid template, including when the request has no Turn State header |
| `dry_run` | Record decisions without modifying request headers |
| Response learning | Save valid templates from successful HTTP/SSE response headers |
| Default probe models | `gpt-6-astra` and `gpt-5.6-sol`; the exact former default list is corrected, while custom or cleared lists are preserved |
| Template / replacement lengths | Default 292 / 312 |
| TTL | Default 3600 seconds from the token's embedded issuance time; receiving the same token does not extend its lifetime |
| Cookies | Upstream cookies are kept per account in memory only and refreshed by every upstream response of a selected account, including probes and 312 responses. For 240 seconds by default, requests for the selected models carry them, with or without a template. After the first listed model collects a 292, collection probes it again after 30 seconds by default to keep the cookies fresh; a refresh without a 292 keeps the template and retries on the next exit after the same interval, and never removes a proxy for a degraded state |

### First-time setup

1. Select the Codex OAuth accounts and exact models you will use. Credentials, model access, and upstream quota must be valid; State does not replenish exhausted quota.
2. Enter one complete proxy URL per line in the correct static or rotating pool. Connectivity testing runs up to 10 requests concurrently. Reachability alone does not mean a valid bucket has been collected.
3. Enable injection and turn off observation. Choose `always` for clients without a State header; `replace-only` requires a replaceable header. Use a CPA 7.3.4-compatible host and HTTP/SSE. Default bucket protection refuses business traffic through unready accounts.
4. Install the standalone collector on the CPA server using the linked deployment guide. Save all settings, verify the collector is online, then click Start. Closing or signing out of the management page does not stop collection. Network and authentication failures retry; repair the collector management credential or CPA service to resume. Stop explicitly clears the persisted collection intent.
5. Confirm the intended account/model bucket has positive remaining minutes and a successful collection log. Send business requests through this CPA and inspect insertion/replacement decisions. The matching template must belong to the same account/model; this is not a guarantee of answer quality.

A degraded length of 312 means no valid template was saved. The runner tries other eligible exits or waits for cooldowns. Fresh/cooling results mean waiting, not completion. The server collector continues when the page is closed, the collection tab is hidden, or the user device sleeps. If the page reports the collector offline, check its process, CPA connection and management credential. The browser does not run probes as a fallback.

### Automatic proxy removal

**Discard abnormal IPs** and **Discard degraded IPs** default to off. The first removes confirmed proxy connection, timeout, or proxy-authentication failures; the second removes entries that returned a degraded State length. Account 401/403/429 responses and missing credentials never qualify as broken proxies.

Enabling either option reveals the minimum retained proxy count: default 10, range 1–40000, counted across both pools. At the floor, probing continues without removal; the feature never empties the pools and silently switches to direct access. Removal targets the exact URL in its original pool, preserving other sessions on the same gateway. For rotating proxies it removes that URL entry, not a permanent egress-IP identity.

Automatic removals are saved immediately and logged with the remaining count. Failed writes retain the proxy and probing continues. Editors follow the updated pools unless there are unsaved edits, which are preserved with a reload notice. Manual **Remove failed proxies** after connectivity testing only edits the draft and still requires Save. Copy the list first if you need a recovery copy.

### Accounts and templates

Templates are isolated by CPA's selected account and actual upstream model. `always` does not create templates or share them between accounts/models. `pass` can mean the request already carries the current template; inspect its reason. Header-length classification follows the upstream project's heuristic and is not an independent measurement of answer quality.

Select accounts and models, save, verify the server collector is online, then click Start. Collection continues when this page closes. **Disabled Codex OAuth accounts are selectable for collection** and are labeled as disabled. Probing does not enable them for business traffic or change CPA's global proxy. Deleted or unsupported accounts are skipped; stale saved selections remain removable. Probing requires a valid OAuth token; expired credentials need a CPA refresh or a new login.

Early renewal is measured from the current template's actual expiry: an 11:00 expiry with a 20-minute lead enters the renewal queue at 10:40. Business requests keep using the unexpired old template until a newer valid template has been saved and atomically replaces it. Failed, degraded, or older responses neither clear the current template early nor extend its expiry.

The table separates remaining validity from time until the renewal window and marks renewal in progress while the old template remains usable. Queueing, proxy failures, account cooldowns and process downtime can still exceed the lead; 10–20 minutes is a practical starting point for larger or unreliable pools. A short TTL's automatic lead may be smaller than the 90-second lease takeover interval. Expired templates are never treated as valid to conceal a gap.
Each static proxy URL represents one exit and cools down for 55 minutes after failure. Each account/model rotating pool shares at most 10 attempts per 10-minute window across all URLs. Due valid buckets receive renewal priority with fair rotation; after at most three renewal attempts a missing bucket gets a turn. Adding URLs cannot multiply the failure retry budget or let one failing bucket starve the rest. Success restarts the budget at the next scheduled renewal. Successful collection schedules renewal from the token's actual issuance time and TTL. Set the renewal lead to an integer such as 10 or 20 minutes, strictly shorter than the TTL. The default `renew_before_minutes=0` preserves the automatic lead: 5 minutes, or one quarter of a shorter TTL. Changing it reschedules successful exits without resetting failure cooldowns. Existing cooldowns from older versions lack a success marker and retain their original deadline once after upgrading.

### Matrix, logs, and self-tests

The account × model matrix displays remaining validity in minutes, missing selected buckets, and still-valid templates outside the selected scope. Account and proxy controls are expanded by default. Saved HTTP, HTTPS, SOCKS5, and SOCKS5H URLs, including credentials, load into editable textareas. Only changed pools are submitted; emptying a loaded textarea and saving clears that pool. Status responses contain counts and a configuration revision, not the complete proxy lists. A separate management-only endpoint reads complete proxy URLs in bounded pages with caching disabled.

Account/model selections, proxies, injection, and renewal settings share dirty-state feedback and **Save all settings** controls at the top and in each section. Failed saves preserve drafts. Bucket readiness displays proxy IP/host and port plainly while hiding authentication; a rotating gateway address is not proof of the actual egress IP.

The dashboard separates probe results, bucket readiness, and business decisions: replacement, insertion, pass-through, skip, observation, and errors. Counters begin when the plugin loads and survive page reloads. After automatic collection is stopped and any in-flight probe finishes, each bucket offers targeted collection and a CPA self-test. Template and cooldown controls remain available as shown by the page. The self-test pins the account/model and sends a minimal request without a template to check CPA's request path; it never harvests a template or proves injection effectiveness or answer quality.

Clearing cooldowns requires confirmation and removes failure waits while preserving valid templates and normal renewal schedules. The next probe may immediately spend account quota or proxy traffic and trigger throttling again; clearing does not remove upstream limits. A targeted reset also clears the account-wide rejection pause, as explained in the confirmation.

### Connectivity and large proxy lists

Test each pool to see progress, masked proxy addresses, and sampled exit IPs when available. Tests use no account credentials or account quota and check connectivity to the Codex API. Only connection or proxy-authentication failures qualify for removal; upstream 403/429 and inconclusive results are retained. Removing failed proxies edits the draft; click Save to apply it. Live collection progress and logs show the actual selected proxy position/total and masked address, with retry counts for rotating proxies. Successful templates retain their masked collection address. A proxy endpoint is not necessarily the actual exit IP; a rotating proxy's diagnostic IP describes that sample only, not a later collection request.

Each pool supports up to 20,000 proxies, with a 16 MiB limit for the complete configuration. The management page automatically uploads large settings in chunks of about 16 KiB per request to avoid typical ingress `413 Request Entity Too Large` limits. Settings take effect atomically after the complete upload passes validation. Interrupted uploads preserve the previous configuration and the editor draft for retry. Changes made by another page after proxies were loaded or during upload cause a conflict; reload the saved proxy lists using the retry button, edit, and save again. Incomplete uploads stay in memory and are discarded on subsequent calls after 15 minutes of inactivity; they are never written to the state file.

### Execution and storage boundaries

Active probing uses Go's built-in HTTP client to send a direct upstream request using the selected account, with a 25-second total timeout. Each probe opens a new connection and closes the response and connection after reading the headers, so rotating proxies can assign a new exit on every attempt. **It consumes upstream quota and is not billed to a downstream CPA API key.** The standalone collector owns periodic scheduling and calls the plugin synchronously. Browser reloads, navigation, sign-out, or closing the browser do not stop collection. Stop persists a disabled intent and lets the current probe drain. Duplicate processes respect one lease and the in-flight gate. Manual/legacy-browser probes are refused while automatic collection is enabled. Configuration changes and template clearing coordinate with in-flight probes.

HTTP/SSE injection needs CPA 7.3.4 or the equivalent forwarding fix. Upstream WebSocket headers apply only to a new handshake. Selected accounts disable that transport, so downstream WS clients are bridged to a fresh HTTP request per turn. WebSocket handshake responses are not part of HTTP/SSE response learning; active probing can collect templates first.

Collection intent persists in `<state_file>.turn-state-runner.json`. The lease and latest 100 events are process-local and reset on CPA restart without deleting templates. Settings, proxies, and a baseline snapshot live in `<state_file>.turn-state.json`; newer durable templates and cooldowns live in the smaller `<state_file>.turn-state.json.runtime.json`. The runtime snapshot applies only to its matching baseline. Stop CPA and back up/restore both files together. Probes persist immediately. Business-response learning first updates an in-memory cache, then persists on management/collector synchronization or a probe commit; restarting before that may lose new cache entries. Failed persistence retains the cache and is reported on the State page. Disk writes and large-pool scans do not hold the business mutex; newer hosts negotiate the stream schema to omit unused chunk history. The file is restricted to the current system user. Ordinary status APIs do not return raw templates. The management-only proxy reader returns complete proxy URLs, including passwords, for the editable textareas; ordinary status and collection logs remain redacted. Enable either this integration or the standalone Turn State plugin to avoid competing header rewrites.

## Rejection responses

The `type` / `code` columns describe OpenAI-compatible responses. Native Anthropic requests use their corresponding error envelope.

| Condition | HTTP status | `type` | `code` |
| --- | --- | --- | --- |
| Concurrency limit reached | `429` | `rate_limit_error` | `rate_limit_exceeded` |
| Subscription quota exhausted | `429` | `rate_limit_error` | `rate_limit_exceeded` |
| Model access denied | `403` | `permission_error` | `insufficient_quota` |
| Ungrouped-key policy, only disabled/empty groups, or unauthorized final credential | `403` | `permission_error` | `access_denied` |
| No available authorized credential | `503` | `server_error` | `internal_server_error` |
| Bound routing rule missing or invalid | `503` | `server_error` | `routing_configuration_error` |
| Per-account concurrency limit reached | `429` | `rate_limit_error` | `rate_limit_exceeded` |
| Protected account has no injectable valid template | `503` | `cpa_key_billing_error` | `turn_state_required` |
| Incompatible State host / unsupported WebSocket session | `503` | `cpa_key_billing_error` | `turn_state_host_unsupported` / `turn_state_websocket_unsupported` |
| Scoped quota cannot resolve a dynamic model | `503` | `cpa_key_billing_error` | `quota_model_unresolved` |
| Risk rule rejection | Default `403`; configurable 4xx | `permission_error` | `content_policy_violation` |

Some error types retain `cpa_key_billing_error` for client compatibility; this does not indicate the old plugin ID is loaded.

For `/v1/responses` WebSocket requests, CPA closes the connection on plugin rejection instead of returning these HTTP response bodies.

## Implementation boundaries and development

CPA's `usage.handle` is the sole source of usage, billing, latency, and upstream failure details. Business response bodies are not parsed to reconstruct usage. Admission checks enforce request policies; scheduling and final-account checks enforce credential access, per-account concurrency, and valid-template protection. Completion hooks release concurrency and perform lifecycle bookkeeping.

Work completes synchronously inside host calls. The plugin does not own background goroutines, timers, or refreshers. The separate collector process schedules State probes on the server. The management page controls this service and drives only page-local quota queries and authorization polling; closing it does not stop the collector. Saved billing, access, and State policies still apply to normal business requests in CPA.

### Local build and debugging

Use Go 1.24+ and a C compiler. On Windows, use MinGW-w64 GCC:

```powershell
$env:CGO_ENABLED = "1"
New-Item -ItemType Directory -Force dist | Out-Null
go build -buildvcs=false -tags cshared -buildmode=c-shared -o dist/cpa-team-manager.dll ./cmd/cpa-key-billing
```

On Linux:

```sh
mkdir -p dist
CGO_ENABLED=1 go build -buildvcs=false -tags cshared -buildmode=c-shared -o dist/cpa-team-manager.so ./cmd/cpa-key-billing
```

Stop CPA, replace the library in its `plugins/` directory, and restart. Do not keep duplicate libraries with the same plugin ID. The UI is embedded in the library, so UI edits also require a rebuild and restart. Use an isolated configuration, database, and dummy credentials for testing. See [AGENTS.md](AGENTS.md) for repository checks.

## License and origins

This project uses the [MIT License](LICENSE) and retains the original **Hao Wang** copyright and license notice. Third-party reference revisions and licenses are documented in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

- [haowang02/cpa-plugin-key-billing](https://github.com/haowang02/cpa-plugin-key-billing), the original fork source. Upstream v1.3.12 localization ([594c3b1](https://github.com/haowang02/cpa-plugin-key-billing/commit/594c3b1922edb6ce38058b4b1dc2707023ed0cf3)) is adapted while preserving this fork's group permissions and Turn State integration.
- [cpa-plugin-codex-turn-state](https://github.com/arden-aaai/cpa-plugin-codex-turn-state), the source of the integrated template collection and injection feature.
- [LINUX DO](https://linux.do/) community.
