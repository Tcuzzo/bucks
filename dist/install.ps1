# install.ps1 — BUCKS guided first-run unpack (Windows).
# This ships INSIDE each Windows release zip next to bucks.exe. It is the friendly
# entry point on Windows: it optionally copies bucks.exe into a user-local apps folder
# on your PATH and launches the guided setup wizard. It touches nothing system-wide and
# never needs Administrator.
$ErrorActionPreference = 'Stop'

$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$bin  = Join-Path $here 'bucks.exe'

if (-not (Test-Path $bin)) {
    Write-Error "BUCKS: cannot find bucks.exe next to this installer ($bin). Unzip the whole BUCKS_windows_amd64.zip and run install.ps1 from inside it."
    exit 1
}

@'
        /\        /\
       /  \  /\  /  \
      ( $  \/  \/  $ )      B U C K S
       \    8-pt    /       the 8-point buck - a trader, not an assistant
        \   buck   /
         \________/

Welcome. BUCKS starts in PAPER mode (simulated money) - going live is a deliberate
choice you make later. Let's get you set up.
'@ | Write-Host

# Offer to install into a user-local apps folder (no admin needed).
$dest = Join-Path $env:LOCALAPPDATA 'BUCKS'
$ans = Read-Host "Install bucks.exe into $dest so you can run it from anywhere? [Y/n]"
if ($ans -match '^(n|no)$') {
    Write-Host "OK - running BUCKS from this folder. You can copy bucks.exe onto your PATH later."
} else {
    New-Item -ItemType Directory -Force -Path $dest | Out-Null
    Copy-Item -Force $bin (Join-Path $dest 'bucks.exe')
    $bin = Join-Path $dest 'bucks.exe'
    Write-Host "Installed to $bin"

    # Add to the USER PATH if not present (current user only; no admin).
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if ($userPath -notlike "*$dest*") {
        [Environment]::SetEnvironmentVariable('Path', "$userPath;$dest", 'User')
        Write-Host "Added $dest to your user PATH (restart your terminal to pick it up)."
    }
}

Write-Host ""
Write-Host "Launching the BUCKS setup wizard..."
& $bin
