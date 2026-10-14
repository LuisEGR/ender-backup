# Installs photo-backup to %LOCALAPPDATA%\photo-backup and starts it hidden at every login
# (via a launcher in the Startup folder). Run from the repo root after building:
#   cd server; go build -o ..\bin\photo-backup.exe .; cd ..
#   powershell -ExecutionPolicy Bypass -File scripts\service\install-windows.ps1
$dir = Join-Path $env:LOCALAPPDATA 'photo-backup'
New-Item -ItemType Directory -Force $dir | Out-Null
$running = Get-Process photo-backup -ErrorAction SilentlyContinue
$running | Stop-Process -Force
$running | Wait-Process -Timeout 10 -ErrorAction SilentlyContinue # the file stays locked until it exits
$exe = Join-Path $dir 'photo-backup.exe'
# Windows (or antivirus) can hold the old file for a moment after the process exits.
$copied = $false
for ($i = 0; $i -lt 20 -and -not $copied; $i++) {
    try {
        Copy-Item (Join-Path $PSScriptRoot '..\..\bin\photo-backup.exe') $exe -Force -ErrorAction Stop
        $copied = $true
    } catch {
        Get-Process photo-backup -ErrorAction SilentlyContinue | Stop-Process -Force
        Start-Sleep -Milliseconds 500
    }
}
if (-not $copied) { throw "Couldn't replace $exe (still in use). Close photo-backup and run this again." }
$vbs = Join-Path ([Environment]::GetFolderPath('Startup')) 'photo-backup.vbs'
Set-Content -Path $vbs -Encoding ASCII -Value "CreateObject(""WScript.Shell"").Run """"""$exe"""" serve"", 0, False"
Start-Process wscript.exe -ArgumentList "`"$vbs`""
$port = 8765
$cfg = Join-Path $env:APPDATA 'photo-backup\config.json'
if (Test-Path $cfg) {
    try { $p = (Get-Content $cfg -Raw | ConvertFrom-Json).port; if ($p) { $port = $p } } catch {}
}
Write-Host "photo-backup installed to $exe and started. Settings: http://localhost:$port/"
