#!/bin/sh
# Build the pdfcpu2 web UI (WebAssembly) locally.
# Output lands in webui/static/, ready to be served by any static file server.
set -e

root="$(cd "$(dirname "$0")/.." && pwd)"

GOOS=js GOARCH=wasm go build -trimpath -ldflags "-s -w" \
  -o "$root/webui/static/app.wasm" "$root/cmd/webui"

if [ -f "$(go env GOROOT)/lib/wasm/wasm_exec.js" ]; then
  cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" "$root/webui/static/wasm_exec.js"
else
  cp "$(go env GOROOT)/misc/wasm/wasm_exec.js" "$root/webui/static/wasm_exec.js"
fi

date -u +%Y-%m-%dT%H:%M:%SZ > "$root/webui/static/version.txt"

echo "webui built:"
ls -lh "$root/webui/static/app.wasm"
echo "serve with e.g.:  (cd webui/static && python3 -m http.server 8899)"
