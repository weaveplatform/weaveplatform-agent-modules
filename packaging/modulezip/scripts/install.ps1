<#
.SYNOPSIS
Installs the weave module in this package into weave-agent's modules directory.

.DESCRIPTION
Written by packaging/modulezip. Run it elevated from the unpacked package:

    powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\install.ps1

It copies module\<id>.exe and module\module.manifest.json into
<InstallDir>\modules\<id>\, the tree core's WeaveAgent service runs modules
from (the counterpart of /usr/lib/weave/modules). It installs the uninstaller
as <InstallDir>\uninstall.d\<id>.ps1 and then runs `weavectl reload`, so a
running weave-agent starts or replaces the module at once. Windows has no
SIGHUP, and core has no directory watch there yet: without the reload, the
module starts at core's next periodic rescan (a minute by default). A failed
reload never fails the install.

Each file is written beside its destination and renamed over it, the binary
first and the manifest last. Core reads a directory with no manifest as no
module, so a rescan during an install never finds a manifest without its
binary. Core runs a copy of every Windows module from its own exec
directory, so the running module never locks the installed binary, and an
upgrade replaces it in place.

Installing core again with a modules\ tree on its media replaces the whole
modules tree; install module packages after core.

.PARAMETER InstallDir
weave-agent's install directory: %ProgramFiles%\Weave unless set.

.PARAMETER WeaveCtl
The weavectl that asks core to reload: <InstallDir>\weavectl.exe unless set.

.PARAMETER NoReload
Do not ask weave-agent to reload, as when building an image.
#>
[CmdletBinding()]
param(
    [string]$InstallDir = '',
    [string]$WeaveCtl = '',
    [switch]$NoReload
)

Set-StrictMode -Version 3
$ErrorActionPreference = 'Stop'

# Install-File copies Source to a sibling of Destination and renames it over
# Destination, so nothing ever reads a half-written file.
function Install-File([string]$Source, [string]$Destination) {
    $staged = "$Destination.new"
    Copy-Item -LiteralPath $Source -Destination $staged -Force
    if (Test-Path -LiteralPath $Destination) {
        [System.IO.File]::Replace($staged, $Destination, [NullString]::Value)
    } else {
        [System.IO.File]::Move($staged, $Destination)
    }
}

# Invoke-Reload asks a running weave-agent to reread its modules directory.
# It only ever warns: the module is in place whether or not the reload lands.
function Invoke-Reload([string]$Ctl) {
    if ($NoReload) {
        Write-Output 'not reloading weave-agent (-NoReload): it finds the module at its next rescan'
        return
    }
    if (-not (Test-Path -LiteralPath $Ctl -PathType Leaf)) {
        Write-Output "no weavectl at ${Ctl}: weave-agent finds the module when it starts"
        return
    }
    # Windows PowerShell turns a native command's stderr into errors, which
    # 'Stop' would throw on; the exit code is what decides here.
    $ErrorActionPreference = 'Continue'
    try {
        $out = & $Ctl reload 2>&1 | ForEach-Object { "$_" }
        if ($LASTEXITCODE -eq 0) {
            $out | Write-Output
            return
        }
        Write-Warning "weavectl reload exited ${LASTEXITCODE}: $($out -join ' '); weave-agent finds the module at its next rescan"
    } catch {
        Write-Warning "weavectl reload failed: $($_.Exception.Message); weave-agent finds the module at its next rescan"
    }
}

try {
    if (-not $InstallDir) {
        $InstallDir = Join-Path $env:ProgramFiles 'Weave'
    }
    if (-not $WeaveCtl) {
        $WeaveCtl = Join-Path $InstallDir 'weavectl.exe'
    }
    $source = Join-Path $PSScriptRoot 'module'
    $manifestPath = Join-Path $source 'module.manifest.json'
    $manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
    $id = [string]$manifest.id
    # The id names directories below; one that could climb out of them, or
    # that core would not load, is refused.
    if ($id -cnotmatch '^[a-z][a-z0-9-]*$') {
        throw "module.manifest.json has an invalid module id '$id'"
    }
    $binary = Join-Path $source "$id.exe"
    if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) {
        throw "the package has no module\$id.exe"
    }
    $moduleDir = Join-Path (Join-Path $InstallDir 'modules') $id
    $uninstallDir = Join-Path $InstallDir 'uninstall.d'
    New-Item -ItemType Directory -Force -Path $moduleDir, $uninstallDir | Out-Null
    Install-File $binary (Join-Path $moduleDir "$id.exe")
    Install-File $manifestPath (Join-Path $moduleDir 'module.manifest.json')
    Install-File (Join-Path $PSScriptRoot 'uninstall.ps1') (Join-Path $uninstallDir "$id.ps1")
    Write-Output "installed $id $($manifest.version) in $moduleDir"
} catch {
    $hint = ''
    if ($_.Exception -is [System.UnauthorizedAccessException] -or
        $_.CategoryInfo.Category -eq 'PermissionDenied') {
        $hint = ' (is this elevated?)'
    }
    [Console]::Error.WriteLine("install.ps1: $($_.Exception.Message)$hint")
    exit 1
}
Invoke-Reload $WeaveCtl
exit 0
