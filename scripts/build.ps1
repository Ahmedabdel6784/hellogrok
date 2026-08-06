# Build Windows tray and console binaries.
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Set-Location $root
$env:Path = "C:\Program Files\Go\bin;$env:USERPROFILE\go\bin;" + $env:Path

# Replace only artifacts owned by this script. Other user files in dist are
# unrelated to the build and must not be deleted.
New-Item -ItemType Directory -Force -Path dist | Out-Null
foreach ($artifact in @("dist/hellogrok.exe", "dist/hellogrok-cli.exe")) {
    if (Test-Path -LiteralPath $artifact) {
        Remove-Item -LiteralPath $artifact -Force
    }
}

# The repository includes architecture-specific Windows resources generated from
# cmd/hellogrok/icon.ico, so normal builds need only Go.
go build -trimpath -ldflags "-s -w -H windowsgui" -o dist/hellogrok.exe ./cmd/hellogrok
go build -trimpath -ldflags "-s -w" -o dist/hellogrok-cli.exe ./cmd/hellogrok
Write-Host "OK: $root\dist\hellogrok.exe"
Write-Host "OK: $root\dist\hellogrok-cli.exe"
Get-ChildItem dist | Format-Table Name, Length
