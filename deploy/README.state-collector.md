# State 服务器采集器

`cpa-state-collector` 是独立进程，负责定期唤醒 CPA 插件中的探测接口。页面只负责配置、启停和查看结果，关浏览器、离开页面或本机休眠不会停止服务器采集。CPA 和采集器都必须在服务器上保持运行。

需要同时使用包含 `/turn-state/runner` 接口的新版 `cpa-team-manager` 插件。**v0.0.9 没有这个接口，单独安装采集器不能把旧版变成后台采集。** 先备份并更新插件、重启 CPA，再安装采集器。插件仍不创建后台 Go 协程，不需要修改 CPA，也不需要 `curl` 执行探测。

采集器只访问你配置的 CPA 管理地址，使用管理密码认证。上游账号凭证和代理仍由插件管理，不复制给采集器。采集消耗账号额度和代理流量；安装进程本身不会自动开启采集，需在 State 页面点「开始」。

## Linux：systemd

使用 systemd 247 或更新版本（例如 Debian 11+、Ubuntu 22.04+）。从与插件同次发布的 `cpa-state-collector_<版本>_linux_<架构>.tar.gz` 中解压可执行文件和服务文件；源码构建方法见下文。这里的命令在服务器执行。

```sh
sudo install -m 0755 cpa-state-collector /usr/local/bin/cpa-state-collector
sudo install -d -m 0700 /etc/cpa-state-collector
sudo install -m 0644 cpa-state-collector.service /etc/systemd/system/cpa-state-collector.service
sudo install -m 0600 collector.env.example /etc/cpa-state-collector/collector.env
```

用 `sudoedit /etc/cpa-state-collector/collector.env` 设置实际 CPA 管理地址，例如：

```text
CPA_URL=http://127.0.0.1:8317
```

用 `sudoedit /etc/cpa-state-collector/management-key` 写入管理密码，文件只放密码本身，允许末尾换行。不要填写下游业务 API Key、OAuth access token 或 bcrypt 哈希。随后限制文件权限：

```sh
sudo chmod 0600 /etc/cpa-state-collector/management-key
sudo systemctl daemon-reload
sudo systemctl enable --now cpa-state-collector
sudo systemctl status cpa-state-collector --no-pager
```

服务通过 systemd `LoadCredential` 读取密码，不把密码写进命令行或日志。更换密码文件后执行 `sudo systemctl restart cpa-state-collector`，让 systemd 重新加载凭证。查看进程日志：

```sh
sudo journalctl -u cpa-state-collector -n 50 --no-pager
```

打开新版插件的 State 页面，确认采集器在线，保存账号、模型、代理和注入策略，再点击「开始」。之后可以关闭页面。开启状态保存在插件旁的 `<state_file>.turn-state-runner.json`，CPA 或采集器重启后会按这个状态继续。

## Docker：独立容器

请先切换到本次发布的源码标签 `v0.1.1`，确保构建内容与插件一致。源码仓库中的 `compose.state-collector.yaml` 是 Linux 主机网络示例。如果 CPA 端口发布在宿主的 8317，可直接使用 `http://127.0.0.1:8317`。采集器和 CPA 在不同容器网络时，`127.0.0.1` 不会指向 CPA；应加入 CPA 的现有 Docker 网络，移除 `network_mode: host`，使用对应服务名（例如 `http://cpa:8317`）。

容器以 UID/GID 65532 运行。准备一个位于仓库外的密码文件，例如 `/etc/cpa-state-collector/container-management-key`，使用编辑器写入管理密码，然后：

```sh
sudo chown 65532:65532 /etc/cpa-state-collector/container-management-key
sudo chmod 0400 /etc/cpa-state-collector/container-management-key
export CPA_MANAGEMENT_KEY_FILE=/etc/cpa-state-collector/container-management-key
export CPA_URL=http://127.0.0.1:8317
export CPA_COLLECTOR_VERSION=0.1.1
docker compose -f deploy/compose.state-collector.yaml up -d --build
docker compose -f deploy/compose.state-collector.yaml logs --tail=50 state-collector
```

`CPA_MANAGEMENT_KEY_FILE` 指向宿主上的文件，Compose 将它只读挂载到容器。不要把真实密码文件放进 Git 仓库。镜像包含 TLS 根证书；管理地址使用 HTTPS 时会正常验证证书，不跳过验证。示例配置使用重启策略，Docker 启动后会恢复采集器；是否执行探测仍以 State 页保存的开启状态为准。

## 源码构建与手动运行

Go 1.24+，不需要 C 编译器：

```sh
CGO_ENABLED=0 go build -trimpath -o cpa-state-collector ./cmd/cpa-state-collector
./cpa-state-collector --help
./cpa-state-collector --url http://127.0.0.1:8317 --management-key-file /安全路径/management-key --check
./cpa-state-collector --url http://127.0.0.1:8317 --management-key-file /安全路径/management-key
```

`--check` 只检查兼容接口和管理认证，不启动探测。前台运行会随终端进程结束而退出；长期运行请交给 systemd、容器或操作系统进程管理器。可使用 `CPA_URL`、`CPA_MANAGEMENT_KEY` 环境变量，但推荐密码文件；不要把管理密码作为命令行参数。手动进程会在后续请求重新读取密码文件，systemd 的 `LoadCredential` 副本则需要重启服务才更新。

## 启停与故障处理

- **页面开始**：保存服务器端的采集意图，由采集器执行。若采集器不在线，会显示等待连接；页面本身不兜底发探测请求。
- **页面停止**：服务器不再启动后续探测。已发出的一次请求会结束或超时，期间显示收尾中。停止进程与停止采集不是同一操作：单独关闭进程保留开启意图，进程恢复后可继续。
- **刷新或离开页面**：只停止页面状态刷新，不停止采集。重新打开会读取服务器最近日志，不依赖原浏览器的本地存储。
- **重复进程**：同一 CPA 插件只允许一个有效租约持有者调度。第二个进程等待，不会并发采集。持有者退出后，其他实例在租约过期后接管；正常部署仍建议只启动一个。
- **网络断开、CPA 重启或认证失败**：采集器保留进程并退避重试。修复 CPA 地址、管理密码或服务后可恢复，不因临时失败清掉页面的开启状态。不要因为看到采集器进程存活就认为探测成功，应检查页面心跳、最近结果及桶有效期。
- **旧页面或手动探测**：自动采集开启或仍有自动请求在途时，旧的手动探测入口会拒绝并提示停止自动采集，避免两个调度器同时消耗账号额度。
- **日志与保存**：开启意图持久化；最近采集日志是服务器内存中的有限列表，CPA 重启会清空该列表，已保存的模板与冷却仍保留。备份时保存数据库、State 基线、运行快照及 runner 控制文件。

自动采集使用现有按账号/模型隔离、临期续采优先、静态与轮换池预算。它不会解除上游额度、429 或网络限制，也不承诺任何账号都能取得有效模板。

## English quick reference

Install both the updated plugin and the standalone collector; v0.0.9 lacks the runner endpoints. The collector wakes the plugin through the authenticated CPA management API. It owns the timer outside the plugin process, while the plugin owns the persisted start/stop setting, single-instance lease, next due time, and bounded recent events. The browser only controls and observes this server state.

Build with `CGO_ENABLED=0 go build -o cpa-state-collector ./cmd/cpa-state-collector`, then run with `--url` and `--management-key-file`. `--check` verifies the API without probing. `CPA_URL` and `CPA_MANAGEMENT_KEY` are supported environment alternatives. Never use an upstream OAuth token or a downstream API key in place of the management password.

Use the included systemd unit for boot-time startup, or build the Docker Compose example from tag v0.1.1 with CPA_COLLECTOR_VERSION=0.1.1 for a sidecar container. systemd 247+ is required for `LoadCredential`; restart the unit after rotating its password file. The container runs as UID 65532, so its mounted password file must be readable by that UID. Set the CPA origin to an address reachable from the collector, and preserve normal HTTPS verification.

Starting the service alone does not enable collection. Click Start on the State page; afterward closing the browser does not stop it. Stop prevents new probes and lets one in-flight probe finish. Network/authentication failures retry, duplicate collectors wait for the lease, and persisted intent survives process restarts. The collector is a separate install from the plugin-store library.
