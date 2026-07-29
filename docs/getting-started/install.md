# Installation

!!! info "Design preview"
    Chmura is a design in progress. There is no released build yet — the commands
    on this page describe the intended experience and are how we validate the
    model. They do not run today.

Chmura ships as a small set of single-purpose tools. Which ones you install
depends on what you want to do.

| Tool | You need it to… | Who runs it |
| --- | --- | --- |
| `chmura` | deploy and operate applications | everyone |
| `chmura-dev` | run a local installation for development | app developers |
| `chmura-server` | host an installation's control plane | operators |
| `chmura-agent` | join a machine to a cluster as a node | operators |

Most people only ever install `chmura` (and `chmura-dev` for local work). You
only need `chmura-server` and `chmura-agent` if you are *hosting* an
installation — see [Architecture](../architecture.md).

## Install the CLI

=== "Homebrew (macOS, Linux)"

    ```bash
    brew install chmura
    ```

=== "Shell installer"

    ```bash
    curl -fsSL https://chmura.example/install.sh | sh
    ```

=== "From a release"

    Download the binary for your platform from the releases page, make it
    executable, and put it on your `PATH`.

!!! info "Distribution is not decided yet"
    Package names and the installer host above are placeholders. No custom domain
    is required to ship an installer — a static `install.sh` served from the
    project's GitHub Pages works today (`curl -fsSL <pages-url>/install.sh | sh`).
    A dedicated domain is a branding choice for later, not a prerequisite.

`chmura-dev` is installed the same way (`brew install chmura-dev`, or bundled
with the shell installer).

## Verify

```bash
chmura version
```

## Shell completion

```bash
chmura completion zsh   # or bash, fish
```

Follow the printed instructions to load it in your shell.

## Next

- **[Quickstart](quickstart.md)** — deploy an app locally in about a minute, with
  no remote installation required.
- **[First steps](first-steps.md)** — connect to a real installation and deploy
  to staging and production.
