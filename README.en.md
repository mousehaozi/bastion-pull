<h1 align="center">bastion-pull</h1>

<p align="center"><b>A tiny pull-based deployer: polls GitHub Releases, replaces files, restarts your service</b></p>

<p align="center">
  <a href="https://github.com/mousehaozi/bastion-pull/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/mousehaozi/bastion-pull/ci.yml?branch=main&style=flat-square&logo=githubactions&logoColor=white&label=CI" alt="CI"></a>
  <a href="https://github.com/mousehaozi/bastion-pull/releases/latest"><img src="https://img.shields.io/github/v/release/mousehaozi/bastion-pull?style=flat-square&logo=github&include_prereleases&sort=date" alt="Release"></a>
  <a href="https://github.com/mousehaozi/bastion-pull/releases"><img src="https://img.shields.io/github/downloads/mousehaozi/bastion-pull/total?style=flat-square&logo=github" alt="Downloads"></a>
  <img src="https://img.shields.io/github/go-mod/go-version/mousehaozi/bastion-pull?style=flat-square&logo=go&logoColor=white" alt="Go version">
  <img src="https://img.shields.io/badge/platform-linux-informational?style=flat-square&logo=linux&logoColor=white" alt="Platform">
  <img src="https://img.shields.io/badge/dependencies-none-brightgreen?style=flat-square" alt="No dependencies">
  <a href="https://github.com/mousehaozi/bastion-pull/stargazers"><img src="https://img.shields.io/github/stars/mousehaozi/bastion-pull?style=flat-square&logo=github" alt="Stars"></a>
</p>

<p align="center"><a href="README.md">简体中文</a> · English</p>

---

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
- Optional update notifications through [PushPlus](https://www.pushplus.plus) to one or more groups and/or friends

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
| `projects[].assets[].name` | Release asset file name or glob (`*`, `?`, `[a-z]`). Several matches in one release is an error; no match just means this release does not update it. |
| `extract_to` | Directory to unpack `.tar.gz` / `.tgz` / `.zip` into. The directory is **replaced as a whole** (old files disappear). Its parent must be writable. |
| `copy_to` | Destination file path (parent directories are created). |
| `projects[].restart` | Shell command run once after all assets of the project were replaced. A non-zero exit counts as failure and is retried next tick. |

| `projects[].assets[].restart` | Shell command run only when **this** asset was updated (once per distinct command), before the project-level `restart`. Use it to restart just the service that changed. |

**Matching drives everything.** A release only needs to carry the assets that changed (e.g. your Actions workflow only builds the admin app this time). For each release, bastion-pull matches its assets against the configured `assets[].name`; every match runs its action (unpack / copy, then its own `restart`), and configured assets the release does not carry are simply skipped. The project-level `restart` runs only if at least one asset matched. A release matching nothing is recorded and ignored. If several releases appeared since the last check, they are merged (the newest one carrying an asset wins), so quick successive releases never lose an update.

Exactly one of `extract_to` / `copy_to` is required per asset. Unknown keys are rejected so typos do not go unnoticed.

Command line:

```
bastion-pull [-c config.yaml] [-once] [-notify-test] [-notify-latest] [-version]
```

`-once` checks every project once and exits (exit code 1 if something failed), handy for testing or cron.

### Update notifications (PushPlus)

After a project has been deployed successfully (files replaced and restart command finished), bastion-pull can send a short HTML message: project, version, time, server, a brief changelog and the updated files. The HTML template is compiled into the binary.

```yaml
notify:
  pushplus:
    token: your_pushplus_token          # or env PUSHPLUS_TOKEN
    # channel: wechat                   # optional, default wechat
    targets:                            # one or more; none = only the token owner
      - name: dev-group
        topic: your_group_code          # send to a group (group code)
      - name: ops
        to: friend_token_1,friend_token_2   # send to friends (comma separated or a YAML list)
      - name: me                        # neither topic nor to = yourself
```

- Get the token on the PushPlus site (*Send message → token*). Group codes are created under *Group message*; friend tokens come from *Friends*.
- Each target is one request. A target uses **either** `topic` **or** `to` (PushPlus ignores `to` when a group is given), so list a group and some friends as separate targets. At most 100 friend tokens per target (PushPlus limits non-members to 10).
- Set `notify: false` on a project to silence it. An empty `token` disables notifications (a warning is logged) instead of failing.
- The "changelog" is taken from the GitHub Release notes (the first 8 lines, markdown stripped). In your release workflow, put the commit messages into the notes, for example:

  ```yaml
  - run: |
      jq -r '.commits[]?.message | split("\n")[0] | "- " + .' "$GITHUB_EVENT_PATH" > notes.md
      [ -s notes.md ] || echo "- manual build" > notes.md
      gh release create "$TAG" web.tar.gz app.jar --notes-file notes.md
  ```

- A failed notification is logged (`notification failed`) and retried 3 times, but never fails or repeats the deployment. Failed deployments do not send a message.
- Test your settings at any time, without deploying anything and while the service is running (without `-c`, `/etc/bastion-pull/config.yaml` is used):

  ```bash
  sudo bastion-pull -notify-test      # a dummy message
  sudo bastion-pull -notify-latest    # the real message for each project's latest release, titled "【测试】"
  ```

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
