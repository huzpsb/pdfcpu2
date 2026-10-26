# Build the pdfcpu2 web UI (WebAssembly) locally.
# Output lands in webui/static/, ready to be served by any static file server.

$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot -Parent

$env:GOOS = 'js'
$env:GOARCH = 'wasm'
go build -trimpath -ldflags "-s -w" -o "$root\webui\static\app.wasm" "$root\cmd\webui"
if ($LASTEXITCODE -ne 0) { exit 1 }
$env:GOOS = ''
$env:GOARCH = ''

$execJs = Join-Path (go env GOROOT) 'lib\wasm\wasm_exec.js'
if (-not (Test-Path $execJs)) {
  $execJs = Join-Path (go env GOROOT) 'misc\wasm\wasm_exec.js'
}
Copy-Item $execJs "$root\webui\static\wasm_exec.js" -Force

Get-Date -Format 'yyyy-MM-ddTHH:mm:ss' | Set-Content "$root\webui\static\version.txt"

Write-Host 'webui built:'
Write-Host "  wasm: $root\webui\static\app.wasm ($([math]::Round((Get-Item "$root\webui\static\app.wasm").Length / 1MB, 1)) MB)"
Write-Host 'serve with e.g.:  python -m http.server -d webui\static 8899'
