# BeeVM: step-by-step build guide

This guide lets anyone rebuild BeeVM from nothing, in the same order we built it. Every command is here, with what it does, why we do it, and the problems we hit (or that people commonly hit).

It grows with the project: each finished milestone adds a section.

| Covered so far | Status |
|---|---|
| Part 0: what we are building | — |
| Part 1: prepare Windows and WSL2 | Done |
| Part 2: get the code, understand the layout | Done |
| Part 3: Milestone 1, a hand-booted MicroVM | Done |
| Part 4: Milestone 2, boot and stop a VM from Go | Done |
| Part 5: publishing to GitHub | Done |
| Appendix: cheat sheet, versions, numbers, troubleshooting | — |

---

## Part 0: what we are building

BeeVM is a Function-as-a-Service (FaaS) runtime, like a tiny AWS Lambda. A user uploads a function; BeeVM runs it inside its own **Firecracker MicroVM** (a very small virtual machine) so different users cannot affect each other. The grading targets are: cold start under 50 ms (via snapshots), tenant isolation, and VM density per machine.

On this laptop everything runs in layers:

```mermaid
flowchart TB
    W["Windows 11 (your laptop)"] --> WSL["WSL2: Ubuntu 24.04<br/>(itself a lightweight VM)"]
    WSL --> KVM["KVM in the WSL2 Linux kernel<br/>/dev/kvm"]
    KVM --> FC["firecracker process<br/>(one per MicroVM)"]
    FC --> G["Guest MicroVM<br/>kernel 6.1 + Alpine rootfs + our init"]
    WSL --> GO["beevmd (our Go program)<br/>controls firecracker"]
    GO -. "HTTP over a Unix socket" .-> FC
```

**Why the layers matter:** a MicroVM inside WSL2 is a VM inside a VM ("nested virtualization"). It works, but it is slower than on a real Linux server. That is why our boot numbers here (about 1.7 s) are only a development baseline.

---

## Part 1: prepare Windows and WSL2

### 1.1 Check that the CPU supports virtualization

**What:** Intel VT-x or AMD-V must be turned on in the BIOS/UEFI.
**Why:** KVM (and so Firecracker) needs hardware virtualization.

In Windows: Task Manager → Performance → CPU → "Virtualization: Enabled". If it says Disabled, turn it on in the BIOS setup.

### 1.2 Install WSL2 with Ubuntu 24.04

In **Windows PowerShell**:

```powershell
wsl --install -d Ubuntu-24.04
wsl -l -v
```

`wsl -l -v` must show `VERSION 2`. WSL1 has no real Linux kernel and no KVM.

### 1.3 Check that KVM works inside WSL2

In the **WSL (Ubuntu) terminal**:

```bash
uname -m                                          # must print x86_64
ls -l /dev/kvm                                    # must exist: crw-rw---- root kvm
cat /sys/module/kvm_intel/parameters/nested       # Intel: Y means nested virtualization is on
cat /sys/module/kvm_amd/parameters/nested         # AMD: 1 means on
```

On our laptop (i7-13620H, Windows 11) all of this worked without changing anything.

**Common problems**

| Symptom | Cause | Fix |
|---|---|---|
| `/dev/kvm: No such file` | Nested virtualization off for WSL2 | Create `C:\Users\<you>\.wslconfig` with `[wsl2]` and `nestedVirtualization=true`, then `wsl --shutdown` in PowerShell |
| Still no `/dev/kvm` | Virtualization off in BIOS, or an old Windows build | Enable VT-x/AMD-V in BIOS; update Windows 11 |
| `nested` file missing | Wrong CPU vendor file (Intel vs AMD) | Read the other one |

---

## Part 2: get the code and understand the layout

### 2.1 Clone the repository

In the **WSL terminal** (choose your own Windows folder):

```bash
cd /mnt/c/Users/<you>/Desktop
git clone https://github.com/om8P/BeeVM.git
cd BeeVM
```

`/mnt/c/...` is your Windows `C:` drive seen from Linux. Keeping the code there means you can edit it with Windows tools (VS Code, Claude) while building it in WSL.

> The course case-study `.docx` is **not** in the repo (it is course material). Get it from the course page if you need it.

### 2.2 Two places on disk, on purpose

```mermaid
flowchart TB
    subgraph WIN["Windows drive /mnt/c/.../BeeVM: easy to edit, slow from Linux"]
      SRC["source code: scripts/, guest/, internal/, cmd/<br/>docs/, Makefile, go.mod"]
    end
    subgraph LIN["WSL Linux disk ~/.beevm = BEEVM_HOME: fast"]
      direction LR
      BIN["bin/ firecracker, jailer"]
      IMG["images/ vmlinux, rootfs.ext4"]
      CACHE["cache/ downloads"]
      RUN["run/ per-boot folders<br/>run/vms/&lt;id&gt;/ per Go-booted VM"]
    end
    SRC -- "make ... reads/writes" --> LIN
```

**Why:** reading files on `/mnt/c` from Linux goes through a slow translation layer. VM disk images and (later) snapshots need fast disk access, so they live in `~/.beevm`. To put them elsewhere: `export BEEVM_HOME=/some/linux/path` before running `make`.

### 2.3 Line endings (a problem we hit)

Windows Git had `core.autocrlf=true`, which turns line endings into Windows-style CRLF on checkout. Bash then fails with errors like `$'\r': command not found`.

**Fix (already in the repo):** `.gitattributes` contains `* text=auto eol=lf`, which forces LF for every text file regardless of the Git settings. Check with:

```bash
git ls-files --eol | grep -E "\.sh|Makefile"     # should show w/lf
```

If a script was already checked out with CRLF: `sed -i 's/\r$//' path/to/script.sh`.

---

## Part 3: Milestone 1, a hand-booted MicroVM

**Goal:** prove KVM and Firecracker work, by booting a VM with the plain `firecracker` command and a JSON config file.

### 3.1 One-time setup: `make setup`

In the **WSL terminal**, in the repo folder:

```bash
make setup
```

It asks for your **sudo password** once. It runs `scripts/setup-wsl.sh`, which does these steps, each skipped if already done (so it is safe to run again):

```mermaid
flowchart TD
    A["1. apt install jq fakeroot curl e2fsprogs"] --> B["2. usermod -aG kvm $USER<br/>(permission for /dev/kvm)"]
    B --> C["3. Go 1.27.3 → /usr/local/go<br/>PATH added to ~/.profile"]
    C --> D["4. Firecracker 1.17.0 + jailer<br/>→ ~/.beevm/bin (checksum checked)"]
    D --> E["5a. guest/kernel/fetch-kernel.sh<br/>vmlinux 6.1.155 → ~/.beevm/images"]
    E --> F["5b. guest/rootfs/build-rootfs.sh<br/>Alpine 3.24.3 → rootfs.ext4 (64 MB)"]
    F --> G["6. print versions; remind to re-login"]
```

All versions and SHA-256 checksums are pinned in `scripts/common.sh`. Every download is checked against its checksum and refused if it does not match.

**Then, once, in Windows PowerShell:**

```powershell
wsl --shutdown
```

and open the WSL terminal again. **Why:** adding a user to a group only takes effect at the next login. Check with `id` (must list `kvm`).

**What each piece is:**

- **`firecracker`**: the virtual machine monitor (VMM). It uses KVM to run a MicroVM with only a few emulated devices (serial port, disk, network, vsock), which is why it starts fast.
- **`jailer`**: Firecracker's sandboxing helper. Installed now, used in milestone 9.
- **`vmlinux`**: the guest Linux kernel as an uncompressed file. Firecracker loads it straight into guest memory; there is no BIOS or bootloader.
- **`rootfs.ext4`**: the guest's disk (its `/`), containing Alpine Linux plus our init script.

### 3.2 How the rootfs is built (no sudo, no Docker)

```bash
make rootfs        # or: bash guest/rootfs/build-rootfs.sh
```

1. Download `alpine-minirootfs-3.24.3-x86_64.tar.gz` (about 3.6 MB).
2. Inside **one `fakeroot` session**: unpack it, copy `guest/rootfs/init` to `/sbin/beevm-init`, then run `mkfs.ext4 -d <folder> rootfs.ext4 64M`.

- `mkfs.ext4 -d` creates a filesystem image and fills it from a folder **without mounting it**, so no root rights are needed.
- `fakeroot` makes the files *look* owned by root inside that session, so they are root-owned inside the image (the guest expects that).

The image is rebuilt automatically when `guest/rootfs/init` changes.

### 3.3 The guest's init script (PID 1)

`guest/rootfs/init` is the first program the guest kernel runs. It mounts `/proc`, `/sys`, `/dev`, prints a banner, then acts on the `beevm.mode=` value in the kernel command line:

| Mode | Used by | Behaviour |
|---|---|---|
| `selftest` | `make boot-test` | prints `BEEVM_BOOT_OK`, then `reboot -f` (VM exits) |
| `idle` | `beevmd boot`, Go tests (milestone 2) | prints `BEEVM_READY`, waits until stopped |
| none | `make boot` | gives you a shell; `reboot -f` exits |

### 3.4 Boot a VM: `make boot-test` and `make boot`

```bash
make boot-test     # automatic: boots, checks, prints the time
make boot          # interactive: you get a shell inside the VM
```

`scripts/boot-vm.sh` makes a run folder `~/.beevm/run/<timestamp>/`, copies `rootfs.ext4` into it (the clean original is never changed), writes `vm-config.json`, and runs `firecracker --no-api --config-file vm-config.json`.

The kernel arguments it passes:

| Argument | Meaning |
|---|---|
| `console=ttyS0` | kernel messages go to the serial port, which Firecracker connects to your terminal |
| `reboot=k` | a guest reboot happens through the keyboard controller, which Firecracker treats as "exit" |
| `panic=1` | if the kernel crashes, reboot after 1 s instead of hanging |
| `pci=off` | Firecracker has no PCI bus, so don't look for one |
| `init=/sbin/beevm-init` | run our script as PID 1 |
| `beevm.mode=...` | our own flag, read by the init script |

What happens during `make boot-test`:

```mermaid
sequenceDiagram
    participant You
    participant S as boot-vm.sh
    participant FC as firecracker
    participant K as KVM (/dev/kvm)
    participant G as guest kernel
    participant I as init (PID 1)
    You->>S: make boot-test
    S->>S: copy rootfs, write vm-config.json
    S->>FC: firecracker --no-api --config-file ...
    FC->>K: create VM, 1 vCPU, 128 MiB
    FC->>G: load vmlinux into guest memory, start CPU
    G->>G: boot (~1.3 s under WSL2), mount rootfs.ext4
    G->>I: run /sbin/beevm-init
    I-->>FC: banner + BEEVM_BOOT_OK (serial console)
    I->>G: reboot -f
    G-->>FC: keyboard-controller reset (reboot=k)
    FC-->>S: process exits (code 0)
    S-->>You: PASS: ... in ~1700 ms
```

In `make boot`, check that you are really in a different system: `uname -r` shows `6.1.155` inside the VM but `6.6...` in WSL.

**Result:** `make boot-test` passes in 1.7 to 2.0 s. `make clean-images && make setup && make boot-test` rebuilds everything from scratch and passes again.

### 3.5 Problems we hit in milestone 1

| Problem | What happened | Fix |
|---|---|---|
| No kernel for Firecracker 1.17 | Firecracker's CI storage had no `vmlinux-6.1` under `v1.17` or `v1.16` | Took `vmlinux-6.1.155` from the `v1.15` folder. The kernel does not depend on the Firecracker version |
| HTTPS certificate error listing the kernel bucket | `https://spec.ccfc.min.s3.amazonaws.com` fails, since bucket names with dots break the S3 certificate | Use the path-style URL `https://s3.amazonaws.com/spec.ccfc.min/...` |
| `sudo` needs a password | Automation tools cannot type it | You run `make setup` yourself; everything after it needs no sudo |
| `Permission denied` on `/dev/kvm` after setup | The group change is not active until next login | `wsl --shutdown` in PowerShell, reopen WSL |
| Shell scripts fail with `\r` errors | CRLF line endings from Windows Git | `.gitattributes` (Part 2.3) |
| Commands run from Windows into WSL lose `$VARS` and quotes | Each layer (Git Bash, `wsl.exe`, bash) re-parses the text; Git Bash also rewrites `/mnt/...` paths | Put commands in a script file and run `wsl.exe -- bash file.sh`, set `MSYS_NO_PATHCONV=1`, or just use `make` targets. Typing directly in the WSL terminal avoids all of this |
| Slow Alpine download | Mirror speed (about 130 KB/s once) | Only happens once; files are cached in `~/.beevm/cache` |

**Other common problems**

- `make: command not found` or strange errors: you ran it in PowerShell. Run it in the WSL terminal.
- `firecracker not found`: `make setup` did not finish. Run it again; finished steps are skipped.
- Boot hangs or FAIL: read the log path printed by the script (`~/.beevm/run/<...>/console.log`). The last lines usually say what went wrong.
- `ls: cannot access /dev/kvm` after a Windows update: check Part 1.3 again.

---

## Part 4: Milestone 2, boot and stop a VM from Go

**Goal:** replace the shell script with Go code that the rest of BeeVM can call: create, start, stop, kill and clean up a VM, leaving nothing behind.

### 4.1 Go module and the Firecracker SDK

Already done in the repo; for a fresh clone you only need:

```bash
make build         # downloads Go dependencies the first time, builds bin/beevmd
```

For the record, the module was created with:

```bash
go mod init github.com/om8P/BeeVM
go get github.com/firecracker-microvm/firecracker-go-sdk@6fb280e993d4
go mod tidy
```

- A **Go module** is a project with its own name (the module path) and a list of pinned dependencies (`go.mod` and checksums in `go.sum`).
- **firecracker-go-sdk** starts the `firecracker` process and talks to its API for us.
- **Why a commit instead of a release?** The SDK's last release (v1.0.0) is from 2022. Its main branch (December 2025) has updated, security-fixed dependencies. It is tested against Firecracker 1.4.1, but worked with our 1.17.0 without changes. It requires Go 1.24 or newer.

If `go` is "not found" in a new terminal: `source ~/.profile`, or use `make` (it calls `/usr/local/go/bin/go` directly).

### 4.2 The API socket: how Go controls Firecracker

In milestone 1 we used `--no-api` and a JSON file. Now Firecracker runs with `--api-sock <file>`: it listens on a **Unix domain socket** (a file that acts like a local network port) and is configured with HTTP requests:

```mermaid
sequenceDiagram
    participant B as beevmd / test
    participant V as internal/vmm
    participant SDK as firecracker-go-sdk
    participant FC as firecracker (API socket)
    participant G as guest
    B->>V: New(cfg): make folder, copy rootfs
    B->>V: Start(ctx)
    V->>SDK: NewMachine + Start
    SDK->>FC: start process with --api-sock firecracker.sock
    SDK->>FC: PUT /machine-config (1 vCPU, 128 MiB)
    SDK->>FC: PUT /boot-source (vmlinux, kernel args)
    SDK->>FC: PUT /drives/... (rootfs.ext4)
    SDK->>FC: PUT /actions InstanceStart
    FC->>G: boot
    G-->>FC: serial output → console.log
    B->>V: WaitForConsole("BEEVM_READY")
    V-->>B: ready after ~1.65 s
    B->>V: Stop(ctx)
    V->>SDK: Shutdown
    SDK->>FC: PUT /actions SendCtrlAltDel
    FC->>G: Ctrl-Alt-Del → kernel restarts → VM exits
    V-->>B: graceful, ~55-90 ms
    B->>V: Cleanup: delete the VM folder
```

### 4.3 The VM lifecycle in `internal/vmm`

```mermaid
stateDiagram-v2
    [*] --> Created: New(cfg)<br/>folder + rootfs copy
    Created --> Running: Start(ctx)
    Created --> Removed: Cleanup()
    Running --> Exited: Stop(ctx)<br/>Ctrl-Alt-Del, wait
    Running --> Exited: Kill()<br/>SIGKILL at once
    Running --> Exited: Stop timed out → Kill()
    Exited --> Removed: Cleanup()<br/>delete folder (or keep console.log)
    Removed --> [*]
```

Files:

| File | What it holds |
|---|---|
| `internal/vmm/config.go` | `Config` (paths, vCPUs, memory, extra kernel args), defaults, `Validate()`, the per-VM paths |
| `internal/vmm/vm.go` | `New`, `Start`, `WaitForConsole`, `Stop`, `Kill`, `Cleanup`, `PID`, `Exited` |
| `internal/vmm/vm_test.go` | 9 unit tests that need no KVM |
| `internal/vmm/vm_kvm_test.go` | 3 tests that boot real VMs (`//go:build kvm`) |
| `cmd/beevmd/main.go` | the `beevmd boot` command |

Each VM gets its own folder `~/.beevm/run/vms/<id>/` with `rootfs.ext4`, `firecracker.sock` and `console.log`. The library never reads environment variables; the caller passes all paths. Only `beevmd` reads `BEEVM_HOME`.

### 4.4 Run it

```bash
make test          # unit tests, no KVM needed
make test-kvm      # boots real VMs: graceful stop, kill, cancelled start
make vet           # Go's static checks
make build
./bin/beevmd boot                      # boot, show console, stop, clean up
./bin/beevmd boot --hold 30s           # keep it running 30 s (Ctrl-C kills it)
./bin/beevmd boot --keep-logs          # keep console.log afterwards
./bin/beevmd boot --mem 256 --vcpus 2  # different size
```

**Check that nothing is left behind** (after any of the above, including Ctrl-C):

```bash
pgrep -a firecracker || echo "no firecracker processes"
ls ~/.beevm/run/vms/
```

**Result:** all tests pass; ready in about 1.65 s, graceful stop in 55 to 90 ms; Ctrl-C exits with code 130; no leftovers in any case we tried.

**Build tags:** `//go:build kvm` at the top of `vm_kvm_test.go` means it is only compiled with `go test -tags kvm`. So `make test` passes on machines without KVM, and `make test-kvm` runs the real thing.

### 4.5 Problems we hit in milestone 2

| Problem | Why | What we did |
|---|---|---|
| The VM would die when a start timeout expired | The SDK ties the firecracker process to the context used to start it | The VM gets its own long-lived context; the caller's timeout only limits how long `Start` waits |
| Calling `Shutdown` twice would panic | The SDK closes an internal channel in `Shutdown` | `Stop` wraps it in `sync.Once` |
| Ctrl-C in `beevmd` went straight to firecracker | The SDK forwards signals to firecracker by default | Set `ForwardSignals` to an empty list; `beevmd` decides (it kills the VM and cleans up) |
| `StopVMM` could hang | It sends SIGTERM and waits | `Kill` sends SIGKILL to the process directly and waits at most 5 s |
| Init's SIGINT handler never runs on stop | By default the kernel restarts immediately on Ctrl-Alt-Del, without telling PID 1 | Fine: the VM still exits cleanly. The handler stays as a safety net |
| The kernel takes ~1.3 s to boot | Partly WSL2 nesting; the log also shows a ~460 ms gap around `clk: Disabling unused clocks` | Noted for tuning in milestone 6 |
| `go mod tidy` requires Go 1.24 | The SDK's main branch needs it | Fine with our Go 1.27.3; docs updated to "Go 1.24+" |

**Other common problems**

- `go: command not found`: `source ~/.profile` or open a new WSL terminal.
- KVM tests print `SKIP`: `/dev/kvm` or the images are missing. Run `make setup` and check `id` for `kvm`.
- `address already in use` for the socket: a folder from a crashed run is left over. Delete `~/.beevm/run/vms/<id>`.
- First `make build` is slow: Go is downloading dependencies; later builds are cached.

---

## Part 5: publishing to GitHub

What we did, so you can do the same for your own fork or a new repo.

```bash
gh auth login                                   # once: log the GitHub CLI in
gh repo create om8P/beevm --public --source=. --remote=origin --push
gh repo rename BeeVM --repo om8P/beevm --yes
git remote set-url origin https://github.com/om8P/BeeVM.git
```

- `gh repo create ... --source=. --push` creates the repo on GitHub, adds it as `origin` and pushes the current branch.
- After renaming the repo we also renamed the Go module to `github.com/om8P/BeeVM` (in `go.mod` and the import in `cmd/beevmd/main.go`) so they match.

### 5.1 Removing a file from history before going public

The first commit contained the course case-study `.docx`. Deleting it in a new commit would still leave it in the history, visible to anyone. So, before the first push, we rewrote the local history:

```bash
git branch backup-before-rewrite        # safety copy of the old history (stays local)
git filter-branch -f --index-filter \
  'git rm -q --cached --ignore-unmatch "Advanced Cloud System Software Design_ 30 Case Studies.docx"' -- main
rm -rf .git/refs/original
echo "Advanced Cloud System Software Design_ 30 Case Studies.docx" >> .gitignore
```

```mermaid
flowchart LR
    subgraph Before["Before (local only)"]
      a1["A: docs + .docx"] --> b1["B"] --> c1["C: M1"] --> d1["D: M2"]
    end
    subgraph After["After (pushed)"]
      a2["A': docs, no .docx"] --> b2["B'"] --> c2["C': M1"] --> d2["D': M2"] --> e2["stop tracking .docx"]
    end
    Before -- "git filter-branch" --> After
```

**Things to know:**

- Every rewritten commit gets a **new hash**. Only do this before anyone else has the old history.
- The rewrite **deleted the file from the working folder** (it was no longer tracked). We restored it with `git show backup-before-rewrite:"<file>" > "<file>"`.
- Commit author emails are public on GitHub. To hide yours, use GitHub's no-reply address: `git config user.email "<id>+<username>@users.noreply.github.com"`.

Current history on GitHub:

| Commit | What |
|---|---|
| `918d884` | planning docs and project setup |
| `e6fdccb` | 13-milestone session guide |
| `e6dad4a` | Milestone 1 |
| `bd71997` | Milestone 2 |
| `717a840` | stop tracking the course document |
| `6364ecd` | rename Go module to `.../BeeVM` |

---

## Appendix A: command cheat sheet

All in the **WSL terminal**, in the repo folder, unless marked PowerShell.

| Command | What it does |
|---|---|
| `make help` | list targets |
| `make setup` | one-time install (sudo); safe to re-run |
| `wsl --shutdown` (PowerShell) | restart WSL so the `kvm` group takes effect |
| `make images` / `make kernel` / `make rootfs` | (re)build guest images only |
| `make boot-test` | milestone 1 automatic boot check |
| `make boot` | milestone 1 interactive VM (`reboot -f` to exit) |
| `make build` | build `bin/beevmd` |
| `./bin/beevmd boot [--hold D] [--keep-logs] [--mem N] [--vcpus N]` | milestone 2 Go boot |
| `make test` / `make test-kvm` / `make vet` | Go tests and checks |
| `make clean-images` | delete `~/.beevm/images` and `~/.beevm/run` |
| `pgrep -a firecracker` | find leftover VMs |
| `id` | check you are in the `kvm` group |

## Appendix B: pinned versions

| Component | Version | Where pinned |
|---|---|---|
| Firecracker + jailer | 1.17.0 | `scripts/common.sh` |
| Go | 1.27.3 (module needs ≥ 1.24) | `scripts/common.sh`, `go.mod` |
| Guest kernel | vmlinux 6.1.155 (Firecracker CI, v1.15 folder) | `scripts/common.sh` |
| Alpine minirootfs | 3.24.3 | `scripts/common.sh` |
| firecracker-go-sdk | main @ `6fb280e993d4` (Dec 2025) | `go.mod` |
| Host | Windows 11, WSL2 Ubuntu 24.04.3, WSL kernel 6.6.87 | — |

## Appendix C: measured numbers (WSL2, i7-13620H, 1 vCPU, 128 MiB)

| Measurement | Value | Milestone |
|---|---|---|
| Cold boot, shell script, process start to exit | 1.7 to 2.0 s | 1 |
| Guest kernel reaches init | ~1.3 s | 1 |
| Cold boot via Go, Start to `BEEVM_READY` | 1.58 to 1.71 s | 2 |
| Graceful stop (Ctrl-Alt-Del) | 55 to 91 ms | 2 |
| `make setup` re-run when all done | 0.6 s | 1 |
| Target cold start (snapshot restore) | < 50 ms | 6 (to do) |

## Appendix D: general troubleshooting

1. **Where am I?** Most problems come from running a command in the wrong place. Prompt `PS C:\...>` is PowerShell; `user@host:~$` is WSL.
2. **Read the console log.** Every boot writes one: `~/.beevm/run/<timestamp>/console.log` (script) or `~/.beevm/run/vms/<id>/console.log` (Go, with `--keep-logs`).
3. **Check KVM access:** `ls -l /dev/kvm` and `id`.
4. **Start clean:** `make clean-images && make setup`.
5. **Leftover processes:** `pgrep -a firecracker`, then `kill <pid>`.
