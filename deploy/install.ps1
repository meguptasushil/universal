# Universal installer for Windows. In PowerShell, run:
#
#   irm https://raw.githubusercontent.com/meguptasushil/universal/main/deploy/install.ps1 | iex
#
# It downloads Universal, adds it to your PATH, creates "Universal Portal"
# shortcuts on the Desktop and in the Start menu, and opens the portal.
$ErrorActionPreference = 'Stop'
$repo = 'meguptasushil/universal'
$dir = Join-Path $env:LOCALAPPDATA 'Universal'
$exe = Join-Path $dir 'universal.exe'
$arch = if ([Environment]::Is64BitOperatingSystem) { 'amd64' } else { throw 'Universal needs 64-bit Windows.' }

New-Item -ItemType Directory -Force -Path $dir | Out-Null
Write-Host 'Downloading Universal...'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
Invoke-WebRequest -UseBasicParsing "https://github.com/$repo/releases/latest/download/universal-windows-$arch.exe" -OutFile $exe
Unblock-File $exe

# add to the user's PATH so "universal" works in new terminals
$path = [Environment]::GetEnvironmentVariable('Path', 'User')
if (-not ($path -split ';' | Where-Object { $_ -eq $dir })) {
    [Environment]::SetEnvironmentVariable('Path', ($path.TrimEnd(';') + ';' + $dir).TrimStart(';'), 'User')
}

# shortcuts that open the portal
$shell = New-Object -ComObject WScript.Shell
$places = @([Environment]::GetFolderPath('Desktop'), (Join-Path ([Environment]::GetFolderPath('StartMenu')) 'Programs'))
foreach ($place in $places) {
    $lnk = $shell.CreateShortcut((Join-Path $place 'Universal Portal.lnk'))
    $lnk.TargetPath = $exe
    $lnk.Arguments = 'portal --open'
    $lnk.WorkingDirectory = $dir
    $lnk.Description = 'Put apps from this computer on the internet'
    $lnk.Save()
}

Write-Host ''
Write-Host "Installed $exe"
Write-Host 'Opening the Universal portal. Next time, use the "Universal Portal" shortcut on your Desktop.'
Start-Process $exe -ArgumentList 'portal', '--open'
