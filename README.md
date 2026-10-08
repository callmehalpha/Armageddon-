# Armageddon

**Your development environment survives the machine.**

Armageddon is a self-hosted workspace server for developers. Your code, your Git history and your *uncommitted* work live on a server you own. You work there from a browser, an IDE or SSH, and your laptops keep live copies. If a laptop is lost, stolen or dies, you lose nothing: open a browser anywhere and carry on.

![Workspace with browser terminal](docs/screenshots/2-workspace.png)

> **Status: v0.1, feature-complete and hardened (M9).** Everything described here runs and is tested. It has not yet been used day to day by people other than its authors, so expect rough edges. See [limitations](#limitations).

## The problem

Git protects what you commit. It does not protect what you are working on right now:

- the half-finished feature you have not committed yet;
- what you staged, your stashes, your local branches;
- the `.env` files and local setup that make the project actually run.

All of that lives on one laptop. When the laptop dies, it goes with it, and getting a new machine back to "I can work on this project" takes hours. Cloud IDEs solve part of this, but your code then lives on someone else's computer, and you work only in their browser editor.

## The solution

Armageddon keeps a complete, live copy of every project, uncommitted work included, on **your own server**, and treats your laptops as copies of it rather than the other way round.

- **Work on the server.** Each project is a *workspace* with a working tree on your server. Open it from the browser terminal, the browser IDE (VS Code in the browser), or SSH from your own editor. Each workspace runs as its own Linux user, isolated from the others and from the server.
- **Uncommitted work is saved continuously.** A couple of seconds after any file changes, Armageddon captures a *checkpoint*: every file, the staged changes and the current commit. Checkpoints are kept apart from your Git history, which stays clean.
- **Laptops keep live replicas.** `armageddon clone` gives you an exact copy, uncommitted work and `.env` included, and keeps it current. Want to work offline or with local tools? `armageddon work local` moves the workspace to your laptop, and `work remote` hands it back.
- **Nothing is ever overwritten silently.** Exactly one place writes at a time. When changes collide, the losing side is set aside in a *quarantine* for you to review, never dropped.
- **Every replica is a backup.** If the server itself is lost, a laptop's replica can rebuild the workspace on a new one, history and uncommitted work included.
- **Run the app too.** Node.js and PHP projects are detected and their dev server runs on the workspace behind an authenticated URL; Docker Compose services (Postgres, Redis…) run alongside.

[How it works](docs/concepts.md) explains workspaces, seats, checkpoints, the lease and quarantine in more depth.

## How to set it up

You need a Linux server (a small VPS is enough: Ubuntu 24.04, Debian 12 or Fedora 40, 1 GB RAM, 10 GB disk) with git 2.39 or newer, and Go 1.26 to build. Laptops can run Linux or macOS.

### 1. Install the server

There is no published release yet, so build from source:

```sh
git clone https://github.com/callmehalpha/Armageddon-.git && cd Armageddon-
go build -o armageddon ./cmd/armageddon
sudo install -m 0755 armageddon /usr/local/bin/

# The server runs as an unprivileged user; a small root helper is the only
# part allowed to create workspace users.
sudo useradd --system --user-group --no-create-home --shell /usr/sbin/nologin armageddon
sudo install -d -o armageddon -g armageddon -m 0755 /var/lib/armageddon
sudo -u armageddon armageddon server init --data /var/lib/armageddon \
     --domain dev.example.com --acme-email you@example.com   # automatic HTTPS
     # or: --ip-only for a self-signed certificate on a bare IP
sudo armageddon helper --data /var/lib/armageddon &
sudo -u armageddon armageddon server run --data /var/lib/armageddon
```

The server prints a one-time setup link. Open it to create the first admin.

Once releases are published, one command does all of this, including systemd services, and verifies the release signature:

```sh
curl -fsSL https://github.com/callmehalpha/Armageddon-/releases/latest/download/install.sh | sudo sh
```

[Operating a server](docs/operations.md) covers TLS, updates and rollback, backups, `doctor` and Docker Compose deployment.

### 2. Create a workspace

In the browser, create a workspace, either empty or cloned from a Git URL, and open its terminal. Edit, `git commit` and `git push` as usual.

### 3. Connect a laptop

```sh
armageddon login https://dev.example.com      # approve this device in the browser
armageddon workspaces                         # what you can access
armageddon clone my-project                   # exact replica, uncommitted work included
armageddon agent install                      # keep replicas current from now on
armageddon status                             # replica health, and who holds the workspace
```

To write on the laptop instead of the server:

```sh
armageddon work local      # the laptop becomes the writer; the server follows
armageddon work remote     # hand it back
```

### 4. Run the app on the server

```sh
armageddon runtime install     # toolchain and dependencies, as the workspace user
armageddon runtime start       # the dev server; prints its authenticated URL
armageddon compose up          # the workspace's compose.yaml
armageddon ports               # every listening port, with its URL
```

### A note on `.env` files

`.env` files are copied to your server, your replicas and your server backups, on purpose: a workspace you cannot run is not one you can continue elsewhere. They never enter Git history and are never pushed to your Git remote. Keep secrets that must not leave one machine outside the working tree. [More on this](docs/concepts.md#your-env-files-are-copied-to-your-server-and-replicas).

When something goes wrong, see [troubleshooting](docs/troubleshooting.md).

## Limitations

The full list, with workarounds and what is planned, is in [docs/LIMITATIONS.md](docs/LIMITATIONS.md). The ones most likely to matter:

- **One writer at a time.** A workspace is written either on the server or on one laptop, never two at once. Use separate workspaces for separate people working in parallel.
- **Size.** Comfortable up to about 50,000 tracked files and 5 GB per repository. Files over 50 MiB are not synced unless you raise the limit (`.armageddon/sync.yaml`).
- **Trusted members only.** The browser IDE and app previews are served from the Armageddon origin, and workspaces share the host network. Share a server only with people you trust (B13, B14, B21).
- **Not supported:** Windows laptops, Git submodules, Git LFS content (pointers sync, content does not).
- **Runtimes:** Node.js and PHP only. The dev server does not survive a server restart (Compose services do).
- **No releases yet.** Build from source until the first signed release is published.

## How to contribute

Contributions are welcome: bug reports, fixes, documentation and tests.

1. **Talk first for big changes.** Open an issue describing the problem before a large pull request. The design contract in [`docs/design/v0.1-architecture.md`](docs/design/v0.1-architecture.md) is the reference for how things must behave; changes to guarantees go there first.
2. **Set up.** Go 1.26 and git 2.39 or newer. Most of the code builds and unit-tests on Linux and macOS; the end-to-end tests need Linux and root, so use a throwaway VM.
3. **Before opening a pull request**, run:

   ```sh
   gofmt -l .                # must print nothing
   go vet ./...
   go test ./...
   ```

   and, on a Linux VM, the end-to-end suite for what you touched:

   ```sh
   sudo mkdir -p /srv && sudo chmod 755 /srv
   sudo install -m 0755 armageddon /usr/local/bin/armageddon
   sudo test/e2e/mvp.sh /usr/local/bin/armageddon        # the core scenario
   sudo test/e2e/write.sh /usr/local/bin/armageddon      # local write mode
   sudo test/e2e/disaster.sh /usr/local/bin/armageddon   # failure scenarios F1–F18
   sudo test/integration/escape.sh /usr/local/bin/armageddon   # privilege boundary
   ```

   CI runs all of these on every pull request. The [disaster suite](docs/disaster-suite.md) maps each failure scenario to its test.
4. **Keep the guarantees.** Two rules are never traded away: an acknowledged checkpoint is never lost, and nothing a user wrote is overwritten without being set aside. A change touching capture, apply or the lease needs a test that would catch a violation.
5. **Security issues:** please report them privately to the maintainer rather than in a public issue. The last review is in [`docs/security-review-v0.1.md`](docs/security-review-v0.1.md).

### Code layout

```
cmd/armageddon/          the single binary: server, helper, agent and CLI
internal/server/         HTTP API, lease authority, Git hosting, workspaces, terminal, web UI
internal/helper/         the root helper: a small, allowlisted API over a Unix socket
internal/agent/          device pairing, replicas, local write mode, quarantine
internal/treesync/       checkpoints: capture, transfer and verified apply
internal/runtimes/       Node.js and PHP runtimes, run as the workspace user
internal/store/          SQLite persistence and migrations
internal/doctor/         health checks and safe repairs
internal/lifecycle/      update and rollback
test/e2e/, test/integration/   end-to-end, disaster and escape tests
docs/                    concepts, operations, troubleshooting, design and reviews
```

## License

MIT. See [LICENSE](LICENSE).
