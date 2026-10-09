# bastion-pull

[English](#english) · [中文](#中文)

---

## English

**bastion-pull** is a tiny pull-based deployer. It runs on your server, polls GitHub Releases, and when a new release appears it downloads the assets, replaces the files on disk, and runs your restart command.

It is made for servers that sit behind a bastion host / firewall: it only makes **outbound** HTTPS requests to GitHub. No inbound port, no webhook, no self-hosted runner, no SSH credentials in CI.

Typical flow: *GitHub Actions builds and publishes a Release → bastion-pull notices → replaces files → restarts the service.*

### Features

- Watches many repositories with one process and one config file
- Polls with `ETag` conditional requests: a check with no change returns `304`, which does **not** count against the GitHub API rate limit, so polling every minute is fine
- Resumable downloads with retries; size and SHA-256 are verified before anything is replaced
- Replaces files atomically (single file: write + rename; directory: unpack to a staging dir, then swap)
- Remembers the deployed release, so restarting bastion-pull never redeploys
- If anything fails (download, replace, restart), nothing is marked as deployed and the next tick retries
- Static binary, no dependencies. Linux builds for amd64, arm64, armv7, 386, riscv64, loong64, mips64le
- Honors `HTTPS_PROXY` / `NO_PROXY`

### Quick install (recommended)

On the server, as root:

```bash
curl -fsSL https://raw.githubusercontent.com/mousehaozi/bastion-pull/main/install.sh | sudo bash
```

The script:

1. detects the CPU architecture (amd64, arm64, armv7, 386, riscv64, loong64, mips64le),
2. downloads the matching binary from the latest release and verifies its SHA-256,
3. installs it to `/usr/local/bin/bastion-pull`, so the `bastion-pull` command works globally,
4. creates the default config `/etc/bastion-pull/config.yaml` (mode 600) if it does not exist yet; an existing config is never overwritten,
5. registers the `bastion-pull` systemd service, **enabled at boot** and with `Restart=always`, so it starts after a server reboot and restarts after a crash,
6. prints where the config file is and what to do next.

After it finishes you **must fill in the config file**, then start the service:

```bash
sudo vi /etc/bastion-pull/config.yaml                       # token, repo, assets, restart command
sudo bastion-pull -c /etc/bastion-pull/config.yaml -once    # optional: test once in the foreground
sudo systemctl start bastion-pull
journalctl -u bastion-pull -f
```

The service is not started on a fresh install because the default config is only a template. Running the script again later **upgrades** the binary, keeps your config, and restarts the service.

Options and environment variables:

| | |
|---|---|
| `--version <tag>` | Install a specific release instead of the latest |
| `--file <archive.tar.gz>` | Install from a local archive (offline; if `checksums.txt` sits next to it, it is verified) |
| `--uninstall` | Remove the binary and the service, keep `/etc/bastion-pull` |
| `GH_PROXY=https://ghfast.top/` | Prefix for GitHub URLs, for servers with a poor connection to GitHub |

If `raw.githubusercontent.com` is blocked (`Connection reset by peer`), fetch the script through a mirror, or skip it and use the offline method below:

```bash
curl -fsSL https://ghfast.top/https://raw.githubusercontent.com/mousehaozi/bastion-pull/main/install.sh | sudo env GH_PROXY=https://ghfast.top/ bash
```

If the server cannot reach GitHub, download `bastion-pull_<version>_linux_<arch>.tar.gz` and `checksums.txt` on another machine, copy them over, and run `sudo ./install.sh --file bastion-pull_<version>_linux_<arch>.tar.gz` (`install.sh` is also inside the archive).

### Manual install

1. Download the archive for your CPU from the [Releases](../../releases) page and verify it against `checksums.txt`:

   ```bash
   uname -m        # x86_64 → linux_amd64, aarch64 → linux_arm64, armv7l → linux_armv7
   sha256sum -c checksums.txt --ignore-missing
   tar xzf bastion-pull_*_linux_amd64.tar.gz
   sudo install -m 755 bastion-pull_*_linux_amd64/bastion-pull /usr/local/bin/bastion-pull
   bastion-pull -version
   ```

2. Create a GitHub token (see [Token](#token)) and the config file:

   ```bash
   sudo mkdir -p /etc/bastion-pull
   sudo cp config.example.yaml /etc/bastion-pull/config.yaml
   sudo chmod 600 /etc/bastion-pull/config.yaml
   sudo vi /etc/bastion-pull/config.yaml
   ```

3. Try it once in the foreground:

   ```bash
   sudo bastion-pull -c /etc/bastion-pull/config.yaml -once
   ```

4. Run it as a service:

   ```bash
   sudo cp deploy/bastion-pull.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now bastion-pull
   journalctl -u bastion-pull -f
   ```

Run it as the user that is allowed to write the target paths and run the restart command (usually `root`). When it runs as root, replaced files keep the owner of the files they replace.

### Token

Only needed for private repositories, but strongly recommended anyway (unauthenticated requests have a very low rate limit).

GitHub → Settings → Developer settings → Personal access tokens → **Fine-grained tokens** → Generate:

- Repository access: *Only select repositories* → the repos you deploy
- Permissions → Repository permissions → **Contents: Read-only** (that is all)
- Set an expiry you can live with and remember to renew it. A `401` in the log means the token has expired.

Put it in the config (`token:`) or in the `GITHUB_TOKEN` environment variable.

### Configuration

```yaml
token: github_pat_xxx          # or env GITHUB_TOKEN
interval: 60s                  # default 1m, minimum 10s
# state_file: /var/lib/bastion-pull/state.json   # default: state.json next to the config
# api_url: https://api.github.com                # only change for GitHub Enterprise

projects:
  - name: studio                       # optional, for logs
    repo: owner/name
    assets:
      - name: web.tar.gz               # unpack into a directory
        extract_to: /var/www/site
      - name: app-*.jar                # glob allowed, must match exactly one asset
        copy_to: /opt/app/app.jar      # copy a single file
        mode: "0644"                   # optional, default: keep the existing file's mode
    restart: systemctl restart app     # optional, run with `sh -c` after all assets are replaced
```

| Key | Meaning |
|---|---|
| `token` | GitHub token. Falls back to `$GITHUB_TOKEN`. |
| `interval` | Time between checks. |
| `state_file` | JSON file recording the deployed release of every repo. A `downloads/` folder for partial downloads is created beside it. |
| `projects[].repo` | `owner/name`. Each repo may appear once. |
| `projects[].assets[].name` | Release asset file name or glob (`*`, `?`, `[a-z]`). Zero or several matches is an error. |
| `extract_to` | Directory to unpack `.tar.gz` / `.tgz` / `.zip` into. The directory is **replaced as a whole** (old files disappear). Its parent must be writable. |
| `copy_to` | Destination file path (parent directories are created). |
| `projects[].restart` | Shell command run once after all assets of the project were replaced. A non-zero exit counts as failure and is retried next tick. |

Exactly one of `extract_to` / `copy_to` is required per asset. Unknown keys are rejected so typos do not go unnoticed.

Command line:

```
bastion-pull [-c config.yaml] [-once] [-version]
```

`-once` checks every project once and exits (exit code 1 if something failed), handy for testing or cron.

### Behavior notes

- **First run:** with no state recorded, the current latest release is deployed immediately.
- **What counts as "latest":** GitHub's *latest release*, i.e. the newest published release that is neither a draft nor a pre-release.
- **Rollback:** there is none, by design. To roll back, publish a new release (or re-run an old build so it becomes the newest release).
- **Directory swap:** replacing a directory is two quick renames, so there is a few-millisecond window where the path does not exist. `extract_to` cannot be a mount point or a symlink target on another filesystem.
- **Several instances:** a lock file (`<state_file>.lock`) prevents two instances sharing one state file.
- **Logs:** printed to stdout/stderr, so `journalctl -u bastion-pull` shows them.

### Publishing from GitHub Actions

Attach your build outputs to a GitHub Release, for example:

```yaml
- run: gh release create "build-$GITHUB_RUN_NUMBER" web.tar.gz app.jar --generate-notes
  env:
    GH_TOKEN: ${{ github.token }}
```

Build a directory archive from its **contents**, so files land directly in `extract_to`: `tar -czf web.tar.gz -C dist .`

### Troubleshooting

| Log message | Cause |
|---|---|
| `404: no published release, or the token cannot see this repository` | No release yet, only drafts/pre-releases exist, wrong repo name, or the token has no access to the repo. |
| `401: token is invalid or expired` | Renew the token. |
| `no asset matches "x" (available: …)` | The release does not contain the asset named in the config. |
| `download … failed after 8 attempts` | Network to GitHub is bad. Partial files are kept and resumed on the next tick. Try setting `HTTPS_PROXY`. |
| `restart command: exit status N` | Your restart command failed; the whole project is retried next tick. |

### Build from source

```bash
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.version=dev" -o bastion-pull .
```

Every push to `main` is tested, cross-compiled and published as a release named `build-<time>-<sha>` (the 10 newest are kept). Pushing a tag such as `v1.0.0` publishes a release with that name.

---

## 中文

**bastion-pull** 是一个很小的「拉取式」部署工具。它运行在你的服务器上，定时检查 GitHub Release，发现新版本就下载资产、替换磁盘上的文件，再执行你配置的重启命令。

适合部署在堡垒机 / 防火墙后面的服务器：它只会主动向 GitHub 发起**出站** HTTPS 请求，不需要开放入站端口，不需要 Webhook，不需要 self-hosted runner，CI 里也不用保存服务器的 SSH 凭证。

典型流程：*GitHub Actions 构建并发布 Release → bastion-pull 发现新版本 → 替换文件 → 重启服务。*

### 特性

- 一个进程、一份配置，同时监控多个仓库
- 使用 `ETag` 条件请求轮询：没有更新时 GitHub 返回 `304`，**不消耗** API 限额，所以每分钟检查一次完全没问题
- 下载支持断点续传和重试；替换前校验文件大小和 SHA-256
- 原子替换（单文件：写临时文件再 rename；目录：先解压到临时目录再整体切换）
- 记录已部署的版本，bastion-pull 自身重启不会重复部署
- 任何一步失败（下载、替换、重启）都不会记为已部署，下个周期自动重试
- 静态二进制，无依赖。提供 amd64、arm64、armv7、386、riscv64、loong64、mips64le 的 Linux 版本
- 支持 `HTTPS_PROXY` / `NO_PROXY` 环境变量

### 一键安装（推荐）

在服务器上用 root 执行：

```bash
curl -fsSL https://raw.githubusercontent.com/mousehaozi/bastion-pull/main/install.sh | sudo bash
```

脚本会依次完成：

1. 检测 CPU 架构（amd64、arm64、armv7、386、riscv64、loong64、mips64le）；
2. 从最新 Release 下载对应架构的二进制，并校验 SHA-256；
3. 安装到 `/usr/local/bin/bastion-pull`，之后可在任意位置直接使用 `bastion-pull` 命令；
4. 如果 `/etc/bastion-pull/config.yaml` 不存在，则创建默认配置（权限 600）；已有配置绝不会被覆盖；
5. 注册 `bastion-pull` 的 systemd 服务，**设置开机自启**并配置 `Restart=always`：服务器重启后自动启动，进程异常退出后自动重启；
6. 输出配置文件的位置和下一步要做的事。

脚本执行完后，你**必须先填写配置文件**，再启动服务：

```bash
sudo vi /etc/bastion-pull/config.yaml                       # 填写 token、仓库、资产路径、重启命令
sudo bastion-pull -c /etc/bastion-pull/config.yaml -once    # 可选：前台试跑一次
sudo systemctl start bastion-pull
journalctl -u bastion-pull -f
```

首次安装不会自动启动服务，因为默认配置只是模板。以后再次运行该脚本即为**升级**：替换程序、保留你的配置、并自动重启服务。

参数与环境变量：

| | |
|---|---|
| `--version <tag>` | 安装指定版本，默认最新 |
| `--file <archive.tar.gz>` | 使用本地压缩包安装（离线；压缩包旁有 `checksums.txt` 时会校验） |
| `--uninstall` | 卸载程序和服务，保留 `/etc/bastion-pull` |
| `GH_PROXY=https://ghfast.top/` | GitHub 地址前缀，用于访问 GitHub 较慢的服务器 |

如果 `raw.githubusercontent.com` 被屏蔽（`Connection reset by peer`），可以通过镜像获取脚本，或直接用下面的离线方式：

```bash
curl -fsSL https://ghfast.top/https://raw.githubusercontent.com/mousehaozi/bastion-pull/main/install.sh | sudo env GH_PROXY=https://ghfast.top/ bash
```

如果服务器连不上 GitHub，可以在别的机器上下载 `bastion-pull_<版本>_linux_<架构>.tar.gz` 和 `checksums.txt`，拷贝到服务器后执行 `sudo ./install.sh --file bastion-pull_<版本>_linux_<架构>.tar.gz`（压缩包里也带有 `install.sh`）。

### 手动安装

1. 到 [Releases](../../releases) 页面下载对应 CPU 架构的压缩包，并用 `checksums.txt` 校验：

   ```bash
   uname -m        # x86_64 → linux_amd64，aarch64 → linux_arm64，armv7l → linux_armv7
   sha256sum -c checksums.txt --ignore-missing
   tar xzf bastion-pull_*_linux_amd64.tar.gz
   sudo install -m 755 bastion-pull_*_linux_amd64/bastion-pull /usr/local/bin/bastion-pull
   bastion-pull -version
   ```

2. 创建 GitHub 令牌（见下文[令牌](#令牌)）并写配置文件：

   ```bash
   sudo mkdir -p /etc/bastion-pull
   sudo cp config.example.yaml /etc/bastion-pull/config.yaml
   sudo chmod 600 /etc/bastion-pull/config.yaml
   sudo vi /etc/bastion-pull/config.yaml
   ```

3. 先前台试跑一次：

   ```bash
   sudo bastion-pull -c /etc/bastion-pull/config.yaml -once
   ```

4. 注册为系统服务：

   ```bash
   sudo cp deploy/bastion-pull.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now bastion-pull
   journalctl -u bastion-pull -f
   ```

请用有权限写入目标路径、执行重启命令的用户运行（通常是 `root`）。以 root 运行时，被替换的文件会保持原来的属主。

### 令牌

私有仓库必须配置；公开仓库也强烈建议配置（未认证请求的限额非常低）。

GitHub → Settings → Developer settings → Personal access tokens → **Fine-grained tokens** → Generate：

- Repository access：选 *Only select repositories*，只勾选要部署的仓库
- Permissions → Repository permissions → **Contents: Read-only**（只需要这一项）
- 设置一个你能接受的有效期，并记得到期续期。日志里出现 `401` 说明令牌过期了。

令牌可以写在配置的 `token:` 里，也可以用环境变量 `GITHUB_TOKEN`。

### 配置

```yaml
token: github_pat_xxx          # 或环境变量 GITHUB_TOKEN
interval: 60s                  # 默认 1m，最小 10s
# state_file: /var/lib/bastion-pull/state.json   # 默认：配置文件同目录的 state.json
# api_url: https://api.github.com                # 仅 GitHub Enterprise 需要修改

projects:
  - name: studio                       # 可选，仅用于日志
    repo: owner/name
    assets:
      - name: web.tar.gz               # 解压到目录
        extract_to: /var/www/site
      - name: app-*.jar                # 支持通配符，必须恰好匹配一个资产
        copy_to: /opt/app/app.jar      # 复制单个文件
        mode: "0644"                   # 可选，默认沿用原文件权限
    restart: systemctl restart app     # 可选，所有资产替换完后用 `sh -c` 执行
```

| 配置项 | 说明 |
|---|---|
| `token` | GitHub 令牌，未填时读取 `$GITHUB_TOKEN`。 |
| `interval` | 检查间隔。 |
| `state_file` | 记录各仓库已部署版本的 JSON 文件；未下载完的文件放在它旁边的 `downloads/` 目录。 |
| `projects[].repo` | `owner/name`，同一个仓库只能出现一次。 |
| `projects[].assets[].name` | Release 资产文件名或通配符（`*`、`?`、`[a-z]`）。匹配 0 个或多个都会报错。 |
| `extract_to` | 把 `.tar.gz` / `.tgz` / `.zip` 解压到的目录。目录会被**整体替换**（旧文件会消失），其上级目录需要可写。 |
| `copy_to` | 目标文件路径（自动创建上级目录）。 |
| `projects[].restart` | 该项目所有资产替换完成后执行一次的 shell 命令。退出码非 0 视为失败，下个周期重试。 |

每个资产必须且只能配置 `extract_to` 或 `copy_to` 其中之一。出现未知配置项会直接报错，避免拼写错误被忽略。

命令行：

```
bastion-pull [-c config.yaml] [-once] [-version]
```

`-once` 表示每个项目只检查一次就退出（有失败时退出码为 1），方便测试或配合 cron。

### 行为说明

- **首次运行：** 没有记录时，会立即部署当前最新的 Release。
- **「最新」的定义：** GitHub 的 *latest release*，即最近发布的、非草稿也非预发布的 Release。
- **回滚：** 按设计没有回滚功能。需要回滚时，重新发布一个新的 Release（或重跑旧的构建，让它成为最新的 Release）。
- **目录切换：** 替换目录是两次快速的 rename，期间有几毫秒该路径不存在。`extract_to` 不能是挂载点。
- **多实例：** 通过锁文件（`<state_file>.lock`）防止两个实例共用同一个状态文件。
- **日志：** 输出到标准输出/错误，用 `journalctl -u bastion-pull` 查看。

### 在 GitHub Actions 中发布

把构建产物上传到 GitHub Release，例如：

```yaml
- run: gh release create "build-$GITHUB_RUN_NUMBER" web.tar.gz app.jar --generate-notes
  env:
    GH_TOKEN: ${{ github.token }}
```

目录类产物要打包目录的**内容**，这样文件会直接落在 `extract_to` 下：`tar -czf web.tar.gz -C dist .`

### 常见问题

| 日志信息 | 原因 |
|---|---|
| `404: no published release, or the token cannot see this repository` | 还没有发布过 Release、只有草稿/预发布、仓库名写错，或令牌没有该仓库的权限。 |
| `401: token is invalid or expired` | 令牌无效或已过期，请更新。 |
| `no asset matches "x" (available: …)` | Release 里没有配置中指定的资产。 |
| `download … failed after 8 attempts` | 到 GitHub 的网络很差。未下完的文件会保留，下个周期继续下载；可以尝试设置 `HTTPS_PROXY`。 |
| `restart command: exit status N` | 重启命令执行失败，整个项目会在下个周期重试。 |

### 从源码构建

```bash
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.version=dev" -o bastion-pull .
```

每次 push 到 `main` 都会自动测试、交叉编译，并发布名为 `build-<时间>-<sha>` 的 Release（保留最近 10 个）。推送 `v1.0.0` 这样的标签则会以该标签名发布。
