# airgapbridge

Cross-platform single-binary CLI enabling Frontend developers (**Next.js** and **Vite**) to develop locally and share pre-PR builds with QA against a protected SIT environment (**Microsoft Entra ID / Azure AD SSO** + **VPN-gated Backend APIs**) without copying auth tokens manually.

Built with **pure Go standard library** (zero external dependencies).

---

## How It Works

1. **`airgapbridge dev`**:
   - Auto-starts your local dev server (`bun run dev`, `yarn dev`, `pnpm dev`, or `npm run dev`) if not already listening on `localPort`.
   - Starts a local HTTP/HTTPS forward-proxy (`127.0.0.1:8899`) with selective `CONNECT` TLS termination **only** for your SIT domain (`sitUrl`).
   - Launches an isolated Chrome/Edge profile (`--user-data-dir` in OS temp folder) pointing to `sitUrl`.
   - Traffic to `sitUrl` (excluding `bypassPaths`) is routed to `http://127.0.0.1:<localPort>` with HMR WebSocket (`Upgrade`) support; Entra ID (`login.microsoftonline.com`), `bypassPaths`, and VPN-gated backend APIs pass through untouched.
2. **`airgapbridge qa`**:
   - Builds your production bundle (`buildCommand`, e.g. `pnpm build` / `npm run build`), serves the build output (`qaCommand`, e.g. `pnpm start` on port `3000` for Next.js or `pnpm preview` on port `4173` for Vite), opens a `cloudflared` tunnel (`https://*.trycloudflare.com`), copies the tunnel URL to your clipboard, and generates `airgapbridge-qa-rules.json` so QA can test your production build on `sitUrl` with real SSO.
3. **`airgapbridge qa-dev`**:
   - Ensures your local dev server (`devCommand` on `localPort`) is running, opens a `cloudflared` tunnel, copies the tunnel URL to your clipboard, and generates `airgapbridge-qa-rules.json` when you want QA to test directly against your live dev server.

---

## Installation

### Option 1: Quick Install via `curl` (Recommended — No Go Required)
*(Uses `releases/latest/download` — or replace with `releases/download/v1.2.0` to pin a specific version.)*

#### macOS (Apple Silicon & Intel) — Raw Binary
```bash
case "$(uname -m)" in arm64|aarch64) ARCH=arm64;; *) ARCH=amd64;; esac
curl -fsSL -o airgapbridge \
  "https://github.com/ertroz/airgapbridge/releases/latest/download/airgapbridge-darwin-${ARCH}"
chmod +x airgapbridge
sudo mv airgapbridge /usr/local/bin/airgapbridge
airgapbridge --version
```

#### macOS — From `.zip`
```bash
case "$(uname -m)" in arm64|aarch64) ARCH=arm64;; *) ARCH=amd64;; esac
curl -fsSL -o airgapbridge.zip \
  "https://github.com/ertroz/airgapbridge/releases/latest/download/airgapbridge-darwin-${ARCH}.zip"
unzip -o airgapbridge.zip
chmod +x airgapbridge
sudo mv airgapbridge /usr/local/bin/airgapbridge
rm airgapbridge.zip
airgapbridge --version
```

#### Linux (amd64)
```bash
curl -fsSL -o airgapbridge.zip \
  "https://github.com/ertroz/airgapbridge/releases/latest/download/airgapbridge-linux-amd64.zip"
unzip -o airgapbridge.zip
chmod +x airgapbridge
sudo mv airgapbridge /usr/local/bin/airgapbridge
rm airgapbridge.zip
airgapbridge --version
```

#### Windows (PowerShell)
```powershell
Invoke-WebRequest -Uri "https://github.com/ertroz/airgapbridge/releases/latest/download/airgapbridge-windows-amd64.zip" -OutFile "airgapbridge.zip"
Expand-Archive -Path "airgapbridge.zip" -DestinationPath "$env:USERPROFILE\bin" -Force
Remove-Item "airgapbridge.zip"
```

### Option 2: Build or Install from Source (Requires Go 1.24+)

```bash
# Run tests
go test -v ./...

# Build for current OS
go build -o ./bin/airgapbridge ./cmd/airgapbridge

# Or install directly into $GOPATH/bin
go install ./cmd/airgapbridge

# Cross-compile all release binaries
GOOS=linux   GOARCH=amd64 go build -o ./bin/airgapbridge ./cmd/airgapbridge
GOOS=darwin  GOARCH=arm64 go build -o ./bin/airgapbridge-darwin-arm64 ./cmd/airgapbridge
GOOS=darwin  GOARCH=amd64 go build -o ./bin/airgapbridge-darwin-amd64 ./cmd/airgapbridge
GOOS=windows GOARCH=amd64 go build -o ./bin/airgapbridge-windows-amd64.exe ./cmd/airgapbridge
```

### Upgrading
- **From GitHub Releases (macOS/Linux):** re-run the Option 1 `curl` command above to overwrite `/usr/local/bin/airgapbridge` with the latest release, then verify with `airgapbridge --version`.
- **From Source (without local Go, using Docker on macOS):**
  ```bash
  ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
  docker run --rm -v "$PWD:/app" -w /app -e GOOS=darwin -e GOARCH="$ARCH" golang:1.24 \
    go build -ldflags="-s -w" -o airgapbridge ./cmd/airgapbridge
  sudo mv airgapbridge /usr/local/bin/airgapbridge
  airgapbridge --version
  ```

---

## Usage Workflow

### 1. Initialize Project (`airgapbridge init`)
Run inside your Next.js or Vite project root. This creates `airgapbridge.config.json` without modifying your app's business logic.

```bash
# Interactive setup
airgapbridge init

# Non-interactive setup
airgapbridge init \
  --sit-url https://sit-app.company.com \
  --port 3000 \
  --bypass "^/api/auth/.*,^/login/callback.*" \
  --yes
```

Generated `airgapbridge.config.json` (Next.js example):
```json
{
  "sitUrl": "https://sit-app.company.com",
  "localPort": 3000,
  "devCommand": "pnpm dev",
  "qaPort": 3000,
  "buildCommand": "pnpm build",
  "qaCommand": "pnpm start",
  "bypassPaths": [
    "^/api/auth/.*",
    "^/login/callback.*"
  ],
  "tunnelProvider": "cloudflared"
}
```

Package manager auto-detection priority:
1. `bun.lock` / `bun.lockb` → `bun run dev` / `bun run build` / `bun run start` (Next.js) or `bun run preview` (Vite)
2. `yarn.lock` → `yarn dev` / `yarn build` / `yarn start` or `yarn preview`
3. `pnpm-lock.yaml` → `pnpm dev` / `pnpm build` / `pnpm start` or `pnpm preview`
4. `package-lock.json` (or fallback) → `npm run dev` / `npm run build` / `npm run start` or `npm run preview`

### 2. Local SSO Development (`airgapbridge dev`)
```bash
airgapbridge dev
```
- Signs in via real Entra ID SSO on `sitUrl` inside the isolated browser window.
- Edits to your Next.js / Vite code hot-reload (HMR) immediately via WebSocket upgrade.
- Press `Ctrl+C` (or close the browser window) to stop all spawned processes and delete the temporary browser profile.

### 3. Share Pre-PR Build with QA (`airgapbridge qa` & `airgapbridge qa-dev`)
Requires [`cloudflared`](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/) installed in your `PATH`:
- **macOS:** `brew install cloudflared`
- **Windows:** `winget install --id Cloudflare.cloudflared`
- **Linux:** Install via package manager or Cloudflare binary release

> **No Cloudflare Account or Dashboard Config Required:**
> `airgapbridge qa` and `airgapbridge qa-dev` use **Cloudflare Quick Tunnels** (`cloudflared tunnel --url http://localhost:<port>`). You do **not** need `cloudflared tunnel login`, a Cloudflare account, API tokens, or DNS setup—a temporary `https://*.trycloudflare.com` URL is generated automatically per session and torn down on `Ctrl+C`.

```bash
# Recommended: build production bundle, serve it, and open QA tunnel
airgapbridge qa

# Or skip rebuild if you already built manually
airgapbridge qa --skip-build

# Or expose the live dev server directly
airgapbridge qa-dev
```
1. Send the generated `airgapbridge-qa-rules.json` file to your QA engineer.
2. QA imports it into **Header Editor** (default) or **Requestly** (`--format requestly`).
3. QA opens `sitUrl`, logs in via Entra ID SSO, and tests your build seamlessly.

---

## CLI Reference

### Global Commands
| Command | Description |
| :--- | :--- |
| `airgapbridge init [flags]` | Generate `airgapbridge.config.json` in the current working directory |
| `airgapbridge dev [flags]` | Start local dev server, selective TLS proxy, and isolated browser |
| `airgapbridge qa [flags]` | Build production bundle, serve build output, start `cloudflared` tunnel, and generate QA rules |
| `airgapbridge qa-dev [flags]` | Start local dev server, `cloudflared` tunnel, and generate QA rules |
| `airgapbridge version` | Print CLI version (`-v`, `--version`) |
| `airgapbridge help` | Show help (`-h`, `--help`) |

### `airgapbridge init` Flags
| Flag | Default | Description |
| :--- | :--- | :--- |
| `--sit-url <url>` | *(required)* | Target SIT frontend URL (e.g. `https://sit-app.company.com`) |
| `--port <port>` | `3000` (Next.js) / `5173` (Vite) | Local dev server port |
| `--qa-port <port>` | `3000` (Next.js) / `4173` (Vite) | QA build server port |
| `--pm <pm>` | Auto-detected from lockfile | Package manager override: `bun`, `npm`, `yarn`, `pnpm` |
| `--dev-command <cmd>` | Auto-detected from `--pm` | Custom local dev command |
| `--build-command <cmd>` | Auto-detected from `--pm` | Custom QA build command |
| `--qa-command <cmd>` | Auto-detected from `--pm` & framework | Custom QA serve/preview command |
| `--bypass <patterns>` | `^/api/auth/.*,^/login/callback.*` | Comma-separated regex patterns routed to real SIT |
| `-y, --yes` | `false` | Skip interactive prompts and use flags/defaults |

### `airgapbridge dev` Flags
| Flag | Default | Description |
| :--- | :--- | :--- |
| `--pm <pm>` | From config / lockfile | Package manager override: `bun`, `npm`, `yarn`, `pnpm` |
| `--port <port>` | From `airgapbridge.config.json` | Override local dev server port |
| `--proxy-port <port>` | `8899` | Override local HTTP/HTTPS proxy listen port |
| `--browser <path>` | Auto-detected Chrome/Edge | Explicit path to Chrome or Edge executable (or `AIRGAPBRIDGE_BROWSER` env) |
| `--no-browser` | `false` | Run proxy + dev server without launching a browser |
| `-i, --interactive` | `false` | Attach terminal stdin/stdout to dev server |

### `airgapbridge qa` Flags
| Flag | Default | Description |
| :--- | :--- | :--- |
| `--pm <pm>` | From config / lockfile | Package manager override: `bun`, `npm`, `yarn`, `pnpm` |
| `--port <port>` | `qaPort` (`3000` Next.js / `4173` Vite) | Override QA build server port |
| `--build-command <cmd>` | From `airgapbridge.config.json` | Override QA build command |
| `--qa-command <cmd>` | From `airgapbridge.config.json` | Override QA serve/preview command |
| `--skip-build` | `false` | Skip running the build command and serve existing build output |
| `-o, --output <file>` | `airgapbridge-qa-rules.json` | Output filename for generated QA rules |
| `--format <fmt>` | `header-editor` | QA rules output format: `header-editor` or `requestly` |
| `--protocol <proto>` | `http2` | Cloudflared transport protocol: `http2`, `quic`, `auto` |
| `-i, --interactive` | `false` | Attach terminal stdin/stdout to build & QA server |

### `airgapbridge qa-dev` Flags
| Flag | Default | Description |
| :--- | :--- | :--- |
| `--pm <pm>` | From config / lockfile | Package manager override: `bun`, `npm`, `yarn`, `pnpm` |
| `--port <port>` | `localPort` in `airgapbridge.config.json` | Override local dev server port |
| `-o, --output <file>` | `airgapbridge-qa-rules.json` | Output filename for generated QA rules |
| `--format <fmt>` | `header-editor` | QA rules output format: `header-editor` or `requestly` |
| `--protocol <proto>` | `http2` | Cloudflared transport protocol: `http2`, `quic`, `auto` |
| `-i, --interactive` | `false` | Attach terminal stdin/stdout to dev server |

---

## Framework Compatibility Notes

If your framework blocks requests from the SIT hostname or `*.trycloudflare.com`, `airgapbridge` prints a startup warning with the snippet to add:

- **Next.js (`next.config.ts` / `next.config.mjs`) — for `dev` / `qa-dev`:**
  ```ts
  allowedDevOrigins: ['sit-app.company.com', '*.trycloudflare.com'],
  ```
- **Vite (`vite.config.ts`) — for `dev` / `qa-dev` (`server`) and `qa` (`preview`):**
  ```ts
  server: {
    cors: true,
    allowedHosts: true,
  },
  preview: {
    cors: true,
    allowedHosts: true,
  },
  ```
