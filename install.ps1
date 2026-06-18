# install.ps1 - BUCKS remote installer / updater (Windows, PowerShell).
#
# One command installs BUCKS if it's absent and updates it if it's present -
# no manual download, no unzip, no reinstall churn. It pulls the latest release
# from GitHub, VERIFIES the download against the published SHA256SUMS (it ABORTS
# on any mismatch - an unverified binary is never installed), and drops
# bucks.exe into a user-local apps folder. It never needs Administrator.
#
#   irm https://raw.githubusercontent.com/Tcuzzo/bucks/main/install.ps1 | iex

$ErrorActionPreference = 'Stop'

$repo = 'Tcuzzo/bucks'
$base = "https://github.com/$repo/releases/latest/download"

Write-Host 'BUCKS installer'
Write-Host 'PAPER mode by default, going live is your choice.'
Write-Host ''

# ---- detect arch ------------------------------------------------------------
$archRaw = $env:PROCESSOR_ARCHITECTURE
switch -Wildcard ($archRaw) {
    'AMD64' { $arch = 'amd64' }
    'ARM64' { $arch = 'arm64' }
    'x86'   { $arch = 'amd64' }   # 32-bit shell on 64-bit Windows; amd64 build runs fine
    default {
        throw "BUCKS: unsupported CPU architecture '$archRaw'. Supported: AMD64, ARM64."
    }
}

$asset      = "BUCKS_windows_$arch.zip"
$extractDir = "BUCKS_windows_$arch"

Write-Host "Detected: windows/$arch"
Write-Host 'Fetching the latest BUCKS release...'

# ---- work dir with cleanup --------------------------------------------------
$work = Join-Path ([System.IO.Path]::GetTempPath()) ("bucks-install-" + [System.IO.Path]::GetRandomFileName())
New-Item -ItemType Directory -Force -Path $work | Out-Null

try {
    $zipPath = Join-Path $work $asset
    $sumPath = Join-Path $work 'SHA256SUMS'

    # ---- download asset + checksums -----------------------------------------
    try {
        Invoke-WebRequest -Uri "$base/$asset" -OutFile $zipPath -UseBasicParsing
    } catch {
        throw "BUCKS: failed to download $asset from the latest release. Check your connection, or grab a zip from https://github.com/$repo/releases/latest"
    }
    try {
        Invoke-WebRequest -Uri "$base/SHA256SUMS" -OutFile $sumPath -UseBasicParsing
    } catch {
        throw 'BUCKS: failed to download SHA256SUMS - cannot verify the binary, aborting.'
    }

    # ---- VERIFY (security core) ---------------------------------------------
    $expected = $null
    foreach ($line in (Get-Content -LiteralPath $sumPath)) {
        $parts = $line -split '\s+', 2
        if ($parts.Count -eq 2 -and $parts[1].Trim() -eq $asset) {
            $expected = $parts[0].Trim().ToLower()
            break
        }
    }
    if (-not $expected) {
        throw "BUCKS: no checksum entry for $asset in SHA256SUMS - refusing to install."
    }

    $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath $zipPath).Hash.ToLower()
    if ($actual -ne $expected) {
        Write-Host "BUCKS: CHECKSUM MISMATCH for $asset - refusing to install." -ForegroundColor Red
        Write-Host "  expected: $expected"
        Write-Host "  actual:   $actual"
        throw 'The download is corrupt or has been tampered with. Nothing was installed.'
    }
    Write-Host 'Checksum verified (sha256).'

    # ---- unzip + locate binary ----------------------------------------------
    $unpacked = Join-Path $work 'unpacked'
    Expand-Archive -LiteralPath $zipPath -DestinationPath $unpacked -Force

    $binSrc = Join-Path (Join-Path $unpacked $extractDir) 'bucks.exe'
    if (-not (Test-Path $binSrc)) {
        $found = Get-ChildItem -Path $unpacked -Recurse -Filter 'bucks.exe' | Select-Object -First 1
        if ($found) { $binSrc = $found.FullName }
    }
    if (-not (Test-Path $binSrc)) {
        throw "BUCKS: could not find bucks.exe inside $asset."
    }

    # ---- install (user-local, no admin) -------------------------------------
    $dest   = Join-Path $env:LOCALAPPDATA 'BUCKS'
    $target = Join-Path $dest 'bucks.exe'

    $oldVer = ''
    if (Test-Path $target) {
        try { $oldVer = (& $target version 2>$null | Select-Object -First 1) } catch { $oldVer = '' }
    }

    New-Item -ItemType Directory -Force -Path $dest | Out-Null
    Copy-Item -Force -LiteralPath $binSrc -Destination $target

    $newVer = ''
    try { $newVer = (& $target version 2>$null | Select-Object -First 1) } catch { $newVer = '' }
    if (-not $newVer) { $newVer = '(installed)' }

    Write-Host ''
    if ($oldVer) {
        Write-Host 'Updated BUCKS:'
        Write-Host "  was: $oldVer"
        Write-Host "  now: $newVer"
    } else {
        Write-Host "Installed BUCKS: $newVer"
    }
    Write-Host "Binary: $target"

    # ---- add dest to USER PATH (idempotent, no admin) -----------------------
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (-not $userPath) { $userPath = '' }
    $onPath = $false
    foreach ($p in ($userPath -split ';')) {
        if ($p.Trim().TrimEnd('\') -ieq $dest.TrimEnd('\')) { $onPath = $true; break }
    }
    if (-not $onPath) {
        $newUserPath = if ($userPath.TrimEnd(';')) { "$($userPath.TrimEnd(';'));$dest" } else { $dest }
        [Environment]::SetEnvironmentVariable('Path', $newUserPath, 'User')
        Write-Host ''
        Write-Host "Added $dest to your user PATH (open a new terminal to pick it up)."
    }

    Write-Host ''
    Write-Host 'Run:           bucks'
    Write-Host 'Help:          bucks help'
    Write-Host "Update later:  re-run this command, or 'bucks update'"
}
finally {
    Remove-Item -Recurse -Force -LiteralPath $work -ErrorAction SilentlyContinue
}
