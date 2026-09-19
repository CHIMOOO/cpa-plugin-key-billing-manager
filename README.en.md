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
<img src="images/example.png" alt="API Key Team Manager dashboard" width="100%" />

## Features

- Spending, token, and request quotas with independent or shared reset schedules.
- A dedicated **Subscription Plans** tab with separate quota pools for one model, several models, or a model family; each key keeps independent balances.
- Long-context pricing tiers and per-key concurrency limits.
- Routing rules and direct model/credential permissions.
- A dedicated **API Key Groups** tab, multiple groups per key, per-group enable switches, and bulk membership editing.
- Optional rejection of all ungrouped API keys.
- Account integrations for OpenCode Go / Zen, CommandCode, and Cline Pass, with supported subscription quota queries and publishing into CPA providers.
- Per-account usage and concurrency controls, plus exact credential-linked quota chips in groups.
- Local content risk rules with observe/pre-block modes, model filters, and hash memory.
- A dedicated **Codex Turn State** tab with template collection, configurable injection modes, and static/rotating proxy pools.
- Reference prices from [models.dev](https://models.dev/) and custom overrides.
- English and Simplified Chinese. Standalone pages remember the selected language; embedded pages follow the management host.

## How it works

The plugin checks quotas, concurrency, and access rules before upstream execution. CLIProxyAPI supplies usage through `usage.handle`; this is the sole source for billing, latency, and failure records. Request completion releases concurrency slots.

Turn State collection reads response headers without reconstructing usage from response bodies. Plugin work is synchronous within host calls. The plugin creates no background goroutines, timers, or refresh tasks; the management page triggers successive bounded probes.

## Requirements

- CLIProxyAPI **7.2.143 or later**, built with plugin support.
- Codex Turn State HTTP/SSE injection requires **7.3.4 or the equivalent request-header forwarding fix**. The 7.2.143 HTTP executor drops the header even if plugin counters report injection.
- Active Turn State probing uses Go's built-in HTTP client; server-side `curl` is not required.

## Installation

Run the installer from your CLIProxyAPI directory.

On macOS or Linux:

```sh
curl -LsSf https://raw.githubusercontent.com/CHIMOOO/cpa-plugin-key-billing-manager/main/install.sh | sh
```

On Windows, stop CLIProxyAPI first and run in PowerShell:

```powershell
irm https://raw.githubusercontent.com/CHIMOOO/cpa-plugin-key-billing-manager/main/install.ps1 | iex
```

These scripts install this fork's release into `plugins/`. Restart CLIProxyAPI after installation or an upgrade. Alternatively, download this fork's [release archive](https://github.com/CHIMOOO/cpa-plugin-key-billing-manager/releases/latest), or add its `registry.json` to the CPA plugin store.

```text
plugins/cpa-team-manager.so       # Linux
plugins/cpa-team-manager.dylib    # macOS
plugins/cpa-team-manager.dll      # Windows
```

Unreleased workspace changes require a local build; the registry installs published release assets.

The store display name is **API Key 团队管理** (API Key Team Manager). Search terms include `api-key`, `team`, `billing`, `quota`, `subscription`, `routing`, `codex`, and `turn-state`. Starting with v0.0.8, the plugin ID, configuration key, library filename, and routes use the independent ID `cpa-team-manager` to avoid colliding with the original project.

### Upgrading this project's v0.0.7 or earlier

1. Stop CPA. Back up its configuration, the existing `state_file` database, and the adjacent `.turn-state.json` file.
2. Move this project's old `cpa-key-billing.so`, `.dylib`, or `.dll` out of the plugin loading directory. Do not enable both copies of the billing and scheduling hooks.
3. Rename this project's key under `plugins.configs` to `cpa-team-manager` and **keep the full original `state_file` path**, for example `plugins/cpa-key-billing-state-v1.db`. The new ID defaults to a new database and does not automatically take over old data.
4. Install the new library, start CPA, and check the existing groups, subscriptions, and buckets at the new URL below. Credential fingerprints remain compatible, preserving existing bindings.

New installations can use the configuration below. This project and the original plugin must use separate data files; never point two active plugins at the same database. To roll back, stop CPA and restore the backup and old plugin configuration.

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

Back up the database before upgrading, together with the adjacent `<state_file>.turn-state.json`. v0.0.7 upgrades supported databases to schema v19 to preserve the new scoped-quota semantics. Do not open the migrated database with an older plugin; restore the pre-upgrade backup when rolling back. Legacy JSON/SQLite files from upstream v0.8.4 or earlier require a new state file.

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

Selections cycle through unselected, allowed, and denied. Permissions from all enabled groups and the key itself are combined: allowlists are unioned, denylists are unioned, and denials win. An empty model allowlist does not restrict models. **For a managed key, an empty credential allowlist grants no upstream access.** Empty groups grant no access. Saving a key's direct permissions and then clearing every selection still means no upstream access.

Selecting a credential category authorizes current and future credentials in that category. Select individual files instead when only specific accounts should be allowed. Searches and category filters do not clear hidden selections; bulk selection affects only the currently displayed individual credentials.

**Enable access control** is on by default. Turning it off suspends group, model, and credential restrictions while preserving CPA authentication, billing, quotas, and concurrency limits. **Reject all ungrouped API keys** is off by default; turning it on rejects both existing and future ungrouped keys while access control is enabled.

Disabling a group preserves its members and rules but excludes its grants and denials. A key belonging only to disabled groups is denied. When it also belongs to enabled groups, those groups continue to apply. Existing groups default to enabled after migration.

Scheduling merges group permissions before selecting from CPA's current candidates for the requested model. A disabled account in one group cannot hide an available authorized account in another. Reenabled accounts follow the host's current state. Group grants do not enable accounts or bypass zero weights, model cooldowns, or denials. If no authorized candidate is available, the request fails instead of using an unauthorized account.

The plugin also validates the final credential chosen by CPA. Host priority settings can narrow the candidate set; when a host scheduling mode bypasses the plugin's selection, the final check rejects unauthorized credentials rather than choosing another itself. Category permissions require an identifiable category; use explicit credentials if the host cannot supply that information.

The former X-Forwarded-For interception feature and management endpoints have been removed. Historical database settings remain for compatibility and no longer affect requests.

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

Refresh quotas on demand. Only returned windows, percentages, or balances are displayed; missing 5-hour/weekly limits are omitted, and failures never imply a full balance. Group chips associate quotas through exact credential references, never name/model heuristics. Unverified associations show no quota. Upstream subscription allowances are separate from this plugin's downstream quota plans.

**Accounts** aggregates the last 365 days of `usage.handle` records by the host's exact account index, including requests, failures, classified tokens, cost, and current concurrency. Missing historical token classifications remain marked incomplete. Some CPA versions omit configured API keys from auth-file inventory: limits can still be set before use, but usage identity requires the first real request, potentially again after restart. Unknown usage is not displayed as zero.

Per-account concurrency limits range from 0–1000; 0 is unlimited. Both upstream-account and downstream-key limits apply. Streaming requests hold their slot until completion, disconnection, or a retry switches accounts. Back up `<state_file>.account-runtime.json` with the database.

## Risk center

Local keyword checks are disabled by default and can be scoped to models. Observe mode records matches; pre-block mode refuses matches before upstream execution. Pre-block also refuses uninspectable payloads, including missing, unsupported, or oversized input. Inspection accepts up to 1 MiB JSON and 64 KiB recognized text; it does not inspect image/audio content or fetch external links.

Optional hash memory recognizes the same normalized text after an earlier match. Events store timestamps, models, rule references, and caller references, never prompts, excerpts, or credentials. Defaults retain 30 days and up to 500 events; counters describe retained events. Hash memory can be cleared separately. Back up `<state_file>.risk-control.json`. This is local rule enforcement, without external AI moderation or a guarantee against upstream account restrictions.

## Codex Turn State

Configure the module in its own **Codex Turn State** tab. It is adapted from [arden-aaai/cpa-plugin-codex-turn-state](https://github.com/arden-aaai/cpa-plugin-codex-turn-state), MIT License, Copyright © 2026 boooot. The account/model template algorithm, injection modes, and proxy retry policy are adapted to this plugin's synchronous execution and separate state store. See [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) for the reference revision and full license.

The module is disabled by default. Saved settings apply immediately; replacing the plugin library still requires a CPA restart.

**Enrolling an account for collection enables business protection by default.** Business requests require a valid template for the selected account and actual upstream model. Missing/expired buckets, disabled injection, dry-run, and headers that `replace-only` cannot replace cause rejection. Other accounts follow their own rules. Protection is a separate immediately saved switch; disabling it requires confirmation. `always` supports clients without a template header but still requires a valid bucket.

Protected accounts require a verified CPA 7.3.4-compatible host with header forwarding. Plugin schema 6 is a minimum marker, not proof that every fork contains the fix. Unknown selected-account metadata or an older host fails closed. Protected WebSocket traffic is refused because reused sessions cannot reliably inject per-turn updates; use HTTP/SSE.

| Setting | Behavior |
| --- | --- |
| `replace-only` (default) | Inject only when the incoming header matches the replacement length, default 312 |
| `always` | Inject a matching valid template, including when the request has no Turn State header |
| `dry_run` | Record decisions without modifying request headers |
| Response learning | Save valid templates from successful HTTP/SSE response headers |
| Default probe models | `gpt-6-astra` and `gpt-5.6-sol`; the exact former default list is corrected, while custom or cleared lists are preserved |
| Template / replacement lengths | Default 292 / 312 |
| TTL | Default 3600 seconds from the token's embedded issuance time; receiving the same token does not extend its lifetime |

Templates are isolated by CPA's selected account and actual upstream model. `always` does not create templates or share them between accounts/models. `pass` can mean the request already carries the current template; inspect its reason. Header-length classification follows the upstream project's heuristic and is not an independent measurement of answer quality.

Select accounts and models, save, then start probing with this page open. **Disabled Codex OAuth accounts are selectable for collection** and are labeled as disabled. Probing does not enable them for business traffic or change CPA's global proxy. Deleted or unsupported accounts are skipped; stale saved selections remain removable. Probing requires a valid OAuth token; expired credentials need a CPA refresh or a new login.

Each static proxy URL represents one exit and cools down for 55 minutes after failure. A rotating URL can be tried up to 10 times before a 10-minute cooldown. Successful collection schedules renewal from the token's actual issuance time and TTL. Set the renewal lead to an integer such as 10 or 20 minutes, strictly shorter than the TTL. The default `renew_before_minutes=0` preserves the automatic lead: 5 minutes, or one quarter of a shorter TTL. Changing it reschedules successful exits without resetting failure cooldowns. Existing cooldowns from older versions lack a success marker and retain their original deadline once after upgrading.

The account × model matrix displays remaining validity in minutes, missing selected buckets, and still-valid templates outside the selected scope. Account and proxy controls are expanded by default. Saved HTTP, HTTPS, SOCKS5, and SOCKS5H URLs, including credentials, load into editable textareas. Only changed pools are submitted; emptying a loaded textarea and saving clears that pool. Status responses still contain counts only. A separate management-only endpoint reads complete proxy URLs in bounded pages with caching disabled.

Account/model selections, proxies, injection, and renewal settings share dirty-state feedback and **Save all settings** controls at the top and in each section. Failed saves preserve drafts. Bucket readiness displays proxy IP/host and port plainly while hiding authentication; a rotating gateway address is not proof of the actual egress IP.

The dashboard separates probe results, bucket readiness, and business decisions: replacement, insertion, pass-through, skip, observation, and errors. Counters begin when the plugin loads and survive page reloads. Each bucket offers targeted collection, a CPA self-test, template clearing, and cooldown clearing. The self-test pins the account/model and sends a minimal request without a template to check CPA's request path; it never harvests a template or proves injection effectiveness or answer quality.

Clearing cooldowns requires confirmation and removes failure waits while preserving valid templates and normal renewal schedules. The next probe may immediately spend account quota or proxy traffic and trigger throttling again; clearing does not remove upstream limits. A targeted reset also clears the account-wide rejection pause, as explained in the confirmation.

Test each pool to see progress, masked proxy addresses, and sampled exit IPs when available. Tests use no account credentials or account quota and check connectivity to the Codex API. Only connection or proxy-authentication failures qualify for removal; upstream 403/429 and inconclusive results are retained. Removing failed proxies edits the draft; click Save to apply it. Live collection progress and logs show the actual selected proxy position/total and masked address, with retry counts for rotating proxies. Successful templates retain their masked collection address. A proxy endpoint is not necessarily the actual exit IP; a rotating proxy's diagnostic IP describes that sample only, not a later collection request.

Each pool supports up to 20,000 proxies, with a 16 MiB limit for the complete configuration. The management page automatically uploads large settings in chunks of about 16 KiB per request to avoid typical ingress `413 Request Entity Too Large` limits. Settings take effect atomically after the complete upload passes validation. Interrupted uploads preserve the previous configuration and the editor draft for retry. Changes made by another page after proxies were loaded or during upload cause a conflict; reload the saved proxy lists using the retry button, edit, and save again. Incomplete uploads stay in memory and are discarded on subsequent calls after 15 minutes of inactivity; they are never written to the state file.

Active probing uses Go's built-in HTTP client to send a direct upstream request using the selected account, with a 25-second total timeout. Each probe opens a new connection and closes the response and connection after reading the headers, so rotating proxies can assign a new exit on every attempt. **It consumes upstream quota and is not billed to a downstream CPA API key.** Continuous work remains browser-driven: a reload restores the task; leaving the page pauses it, with resumption on return. Tabs on the same origin and management identity coordinate ownership. Stop clears the saved intent, while the current request finishes or times out. With all pages closed, no background server job runs. Configuration changes and template clearing wait for an in-flight probe to finish.

HTTP/SSE injection needs CPA 7.3.4 or the equivalent forwarding fix. WebSocket headers apply only to a new handshake, not each message over a reused connection. WebSocket handshake responses are not part of HTTP/SSE response learning; active probing can collect templates first.

Settings, proxies, and templates live in `<state_file>.turn-state.json` next to the billing database. The file is restricted to the current system user. Management APIs never return raw templates. The management-only proxy reader returns complete proxy URLs, including passwords, for the editable textareas; ordinary status and collection logs remain redacted. Enable either this integration or the standalone Turn State plugin to avoid competing header rewrites.

## Rejection responses

| Condition | HTTP status | `type` | `code` |
| --- | --- | --- | --- |
| Concurrency limit reached | `429` | `rate_limit_error` | `rate_limit_exceeded` |
| Subscription quota exhausted | `429` | `rate_limit_error` | `rate_limit_exceeded` |
| Model access denied | `403` | `permission_error` | `insufficient_quota` |
| Ungrouped-key policy, only disabled/empty groups, or unauthorized final credential | `403` | `permission_error` | `access_denied` |
| No available authorized credential | `503` | `server_error` | `internal_server_error` |
| Bound routing rule missing or invalid | `503` | `server_error` | `routing_configuration_error` |

For `/v1/responses` WebSocket requests, CPA closes the connection on plugin rejection instead of returning these HTTP response bodies.

## Local development

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

## Acknowledgments

- [haowang02/cpa-plugin-key-billing](https://github.com/haowang02/cpa-plugin-key-billing), the original fork source. Upstream v1.3.12 localization ([594c3b1](https://github.com/haowang02/cpa-plugin-key-billing/commit/594c3b1922edb6ce38058b4b1dc2707023ed0cf3)) is adapted while preserving this fork's group permissions and Turn State integration.
- [cpa-plugin-codex-turn-state](https://github.com/arden-aaai/cpa-plugin-codex-turn-state), the source of the integrated template collection and injection feature.
- [LINUX DO](https://linux.do/) community.
