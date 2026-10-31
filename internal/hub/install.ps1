# machine-monitor agent installer for Windows, served by a machine-monitor hub with the
# values below filled in. Works in Windows PowerShell 5.1 and PowerShell 7. No admin rights.
#   irm -Headers @{Authorization='Bearer KEY'} http://HUB:7777/install.ps1 | iex
#
# It downloads the release the hub runs from GitHub, checks the archive against the SHA-256
# the hub pinned (from the release's signed checksums.txt), installs the binary under
# %LOCALAPPDATA%\Programs\machine-monitor, pairs with the hub and starts the agent at logon.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
# Windows PowerShell 5.1 may default to TLS 1.0, which GitHub refuses.
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$hub = '__HUB__'
$key = '__KEY__'
$version = '__VERSION__'
$base = '__BASE__'
$sums = '__SUMS__'

$arch = 'amd64'
if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64' -or $env:PROCESSOR_ARCHITEW6432 -eq 'ARM64') { $arch = 'arm64' }
$name = "machine-monitor_${version}_windows_$arch.zip"
$want = $null
foreach ($line in ($sums -split "`n")) {
  $parts = $line.Trim() -split '\s+'
  if ($parts.Count -eq 2 -and $parts[1] -eq $name) { $want = $parts[0] }
}
if (-not $want) { throw "machine-monitor install: release $version has no build for windows/$arch" }

$tmp = Join-Path ([IO.Path]::GetTempPath()) ('machine-monitor-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force $tmp | Out-Null
try {
  $zip = Join-Path $tmp $name
  Write-Host "Downloading machine-monitor $version for windows/$arch..."
  Invoke-WebRequest "$base/$name" -UseBasicParsing -OutFile $zip
  $got = (Get-FileHash $zip -Algorithm SHA256).Hash.ToLower()
  if ($got -ne $want) { throw "machine-monitor install: the download failed its SHA-256 check (got $got, want $want); not installing" }
  Expand-Archive $zip -DestinationPath $tmp -Force

  $dir = Join-Path $env:LOCALAPPDATA 'Programs\machine-monitor'
  New-Item -ItemType Directory -Force $dir | Out-Null
  $exe = Join-Path $dir 'machine-monitor.exe'
  # A running agent locks its .exe but it can still be renamed: move it aside, then move the
  # new one in.
  if (Test-Path $exe) {
    Remove-Item "$exe.old" -Force -ErrorAction SilentlyContinue
    Move-Item $exe "$exe.old" -Force
  }
  Move-Item (Join-Path $tmp 'machine-monitor.exe') $exe -Force
  Write-Host "Installed $exe"
} finally {
  Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue
}

# Saves the hub and key in client.json, after the hub proves it holds the key.
& $exe connect --no-agent "$hub/?key=$key"
if ($LASTEXITCODE -ne 0) { throw "machine-monitor install: pairing with $hub failed" }
& $exe service install --role agent --hub $hub
if ($LASTEXITCODE -ne 0) { throw 'machine-monitor install: service install failed' }

# Put the binary on this user's PATH for new terminals.
$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (-not $userPath) { $userPath = '' }
if (($userPath -split ';') -notcontains $dir) {
  [Environment]::SetEnvironmentVariable('Path', (($userPath.TrimEnd(';') + ';' + $dir).TrimStart(';')), 'User')
  Write-Host "Added $dir to your PATH (open a new terminal to use it)."
}
Write-Host "This machine now reports to $hub. Remove with: machine-monitor service uninstall --role agent" -ForegroundColor Green
