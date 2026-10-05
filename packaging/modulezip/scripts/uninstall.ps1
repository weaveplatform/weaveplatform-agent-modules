<#
.SYNOPSIS
Removes a weave module that a module package installed.

.DESCRIPTION
Written by packaging/modulezip. The install puts this script at
<InstallDir>\uninstall.d\<id>.ps1, and run from there it removes that module:

    powershell.exe -NoProfile -ExecutionPolicy Bypass -File "$env:ProgramFiles\Weave\uninstall.d\<id>.ps1"

Run from the unpacked package as uninstall.ps1, it removes the package's
module instead.

It removes the manifest first, so core reads the directory as no module from
then on, then the binary, then the directory if nothing else is left in it (an
operator's config.json stays), and then this uninstaller. It then runs
`weavectl reload`, so a running weave-agent stops the module at once; core's
periodic rescan does the same within a minute otherwise. A failed reload never
fails the removal.

.PARAMETER Id
The module to remove: by default this script's name once installed, or the
package's module when run as uninstall.ps1.

.PARAMETER InstallDir
weave-agent's install directory: the parent of uninstall.d when run from
there, otherwise %ProgramFiles%\Weave.

.PARAMETER WeaveCtl
The weavectl that asks core to reload: <InstallDir>\weavectl.exe unless set.

.PARAMETER NoReload
Do not ask weave-agent to reload.
#>
[CmdletBinding()]
param(
    [string]$Id = '',
    [string]$InstallDir = '',
    [string]$WeaveCtl = '',
    [switch]$NoReload
)

Set-StrictMode -Version 3
$ErrorActionPreference = 'Stop'

# Invoke-Reload asks a running weave-agent to reread its modules directory.
# It only ever warns: the module is gone whether or not the reload lands.
function Invoke-Reload([string]$Ctl) {
    if ($NoReload) {
        Write-Output 'not reloading weave-agent (-NoReload): it stops the module at its next rescan'
        return
    }
    if (-not (Test-Path -LiteralPath $Ctl -PathType Leaf)) {
        Write-Output "no weavectl at ${Ctl}: nothing to reload"
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
        Write-Warning "weavectl reload exited ${LASTEXITCODE}: $($out -join ' '); weave-agent stops the module at its next rescan"
    } catch {
        Write-Warning "weavectl reload failed: $($_.Exception.Message); weave-agent stops the module at its next rescan"
    }
}

# Remove-IfPresent deletes one file, if it is there.
function Remove-IfPresent([string]$Path) {
    if (Test-Path -LiteralPath $Path) {
        Remove-Item -LiteralPath $Path -Force
    }
}

try {
    $here = Split-Path -Parent $PSCommandPath
    $installed = (Split-Path -Leaf $here) -eq 'uninstall.d'
    if (-not $Id) {
        if ($installed) {
            $Id = [System.IO.Path]::GetFileNameWithoutExtension($PSCommandPath)
        } else {
            $manifestPath = Join-Path (Join-Path $here 'module') 'module.manifest.json'
            $Id = [string](Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json).id
        }
    }
    # The id names the directory removed below; one that could climb out of
    # the modules tree is refused.
    if ($Id -cnotmatch '^[a-z][a-z0-9-]*$') {
        throw "invalid module id '$Id'"
    }
    if (-not $InstallDir) {
        if ($installed) {
            $InstallDir = Split-Path -Parent $here
        } else {
            $InstallDir = Join-Path $env:ProgramFiles 'Weave'
        }
    }
    if (-not $WeaveCtl) {
        $WeaveCtl = Join-Path $InstallDir 'weavectl.exe'
    }
    $moduleDir = Join-Path (Join-Path $InstallDir 'modules') $Id
    foreach ($name in 'module.manifest.json', 'module.manifest.json.new', "$Id.exe", "$Id.exe.new") {
        Remove-IfPresent (Join-Path $moduleDir $name)
    }
    if ((Test-Path -LiteralPath $moduleDir) -and -not (Get-ChildItem -LiteralPath $moduleDir -Force)) {
        Remove-Item -LiteralPath $moduleDir -Force
    }
    $uninstaller = Join-Path (Join-Path $InstallDir 'uninstall.d') "$Id.ps1"
    Remove-IfPresent $uninstaller
    Remove-IfPresent "$uninstaller.new"
    Write-Output "removed $Id from $moduleDir"
} catch {
    $hint = ''
    if ($_.Exception -is [System.UnauthorizedAccessException] -or
        $_.CategoryInfo.Category -eq 'PermissionDenied') {
        $hint = ' (is this elevated?)'
    }
    [Console]::Error.WriteLine("uninstall.ps1: $($_.Exception.Message)$hint")
    exit 1
}
Invoke-Reload $WeaveCtl
exit 0
