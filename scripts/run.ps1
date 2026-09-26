# fyisp launcher for Windows PowerShell 5.1+ and PowerShell 7 (also Linux and
# macOS): downloads the pinned fyisp release for this machine into a temporary
# directory, verifies its SHA-256 against the release's SHA256SUMS (mandatory)
# and, if the GitHub CLI is installed and logged in, its build provenance
# attestation; then runs it with your arguments. Nothing is installed; the
# download is deleted when fyisp exits.
#
#   powershell -ExecutionPolicy Bypass -File run.ps1 [fyisp args]
#   & ([scriptblock]::Create((irm https://github.com/fuck-you-isp/fyisp/releases/download/<version>/run.ps1))) --share
#
# Environment:
#   FYISP_BASE_URL  download from here instead of the GitHub release (must be
#                   https://, or http:// on localhost / 127.0.0.1 for tests)
#
# The whole script is inside Main, called on the last lines, so a truncated
# download never runs a partial script.

function Main {
    param([string[]]$FyispArgs)
    Set-StrictMode -Version 2.0
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue' # Invoke-WebRequest is very slow with the progress bar

    # Filled in by the release workflow; do not edit by hand.
    $FyispVersion = '@FYISP_VERSION@'
    $repo = 'fuck-you-isp/fyisp'

    function Say([string]$msg) { [Console]::Error.WriteLine("fyisp-run: $msg") }

    # ---- where to download from ----
    $base = $env:FYISP_BASE_URL
    if (-not $base) {
        if ($FyispVersion.StartsWith('@')) {
            throw "this run.ps1 is not from a release (no version pinned); download it from https://github.com/$repo/releases or set FYISP_BASE_URL"
        }
        $base = "https://github.com/$repo/releases/download/$FyispVersion"
    }
    $base = $base.TrimEnd('/')
    if ($base -notmatch '^https://') {
        if ($base -match '^http://(localhost|127\.0\.0\.1)(:\d+)?(/|$)') {
            Say "warning: downloading over plain HTTP from $base (test mode)"
        } else {
            throw "FYISP_BASE_URL must start with https:// (http:// only for localhost or 127.0.0.1): $base"
        }
    }

    # ---- which binary ----
    $isWin = $true
    $os = 'windows'
    if (Test-Path variable:IsWindows) { $isWin = $IsWindows }
    if (-not $isWin) {
        if ($IsLinux) { $os = 'linux' } elseif ($IsMacOS) { $os = 'darwin' } else { throw 'unsupported operating system' }
    }
    $arch = $null
    try {
        $arch = [string][System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture
    } catch {
        $arch = $env:PROCESSOR_ARCHITEW6432
        if (-not $arch) { $arch = $env:PROCESSOR_ARCHITECTURE }
    }
    switch -regex ($arch) {
        '^(X64|AMD64)$' { $arch = 'amd64'; break }
        '^(Arm64|ARM64)$' { $arch = 'arm64'; break }
        '^(Arm|ARM)$' { if ($os -eq 'linux') { $arch = 'armv7'; break } else { throw "unsupported CPU architecture: $arch" } }
        default { throw "unsupported CPU architecture: $arch (fyisp ships amd64 and arm64)" }
    }
    $asset = "fyisp-$os-$arch"
    if ($os -eq 'windows') { $asset += '.exe' }

    # ---- download into a private temporary directory ----
    if ($PSVersionTable.PSVersion.Major -lt 6) {
        [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    }
    $tmp = Join-Path ([IO.Path]::GetTempPath()) ('fyisp-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        function Fetch([string]$url, [string]$out) {
            $p = @{ Uri = $url; OutFile = $out; UseBasicParsing = $true; MaximumRedirection = 5 }
            try { Invoke-WebRequest @p } catch { throw "download failed: $url ($($_.Exception.Message))" }
            # Neither PowerShell 5.1 nor 7 follows an https -> http redirect.
        }
        Say "downloading $asset from $base"
        $sums = Join-Path $tmp 'SHA256SUMS'
        $bin = Join-Path $tmp $asset
        Fetch "$base/SHA256SUMS" $sums
        Fetch "$base/$asset" $bin

        # ---- verify (mandatory) ----
        $want = $null
        foreach ($line in Get-Content -LiteralPath $sums) {
            $f = -split $line
            if ($f.Count -ge 2 -and ($f[1] -eq $asset -or $f[1] -eq "*$asset")) { $want = $f[0].ToLowerInvariant(); break }
        }
        if (-not $want) { throw "$asset is not listed in SHA256SUMS" }
        $got = (Get-FileHash -LiteralPath $bin -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($got -ne $want) { throw "checksum mismatch for ${asset}: got $got, want $want; not running it" }
        Say "sha256 ok: $got"

        if (-not $env:FYISP_BASE_URL -and (Get-Command gh -ErrorAction SilentlyContinue)) {
            & gh auth status 2>$null | Out-Null
            if ($LASTEXITCODE -eq 0) {
                & gh attestation verify $bin --repo $repo | Out-Null
                if ($LASTEXITCODE -ne 0) { throw "build provenance attestation check failed for $asset; not running it" }
                Say "attestation ok (built by github.com/$repo)"
            }
        }
        if (-not $isWin) { & chmod 0755 $bin }

        # ---- run ---- (output goes straight to the console; $LASTEXITCODE
        # is fyisp's exit code when Main returns)
        & $bin @FyispArgs
    } finally {
        Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Main -FyispArgs $args
if ($PSCommandPath) { exit $LASTEXITCODE }
