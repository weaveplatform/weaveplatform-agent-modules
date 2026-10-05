# authenticode.ps1: the Authenticode signing and checks module-release.yml's
# sign-windows job runs, dot-sourced by its steps:
#
#   . .github/scripts/authenticode.ps1
#   Invoke-AuthenticodeSigning -Path <file>...   sign (SHA-256) and timestamp
#   Assert-Authenticode -Path <file> [-Trusted]  check the signature and its pin
#
# Signing reads SIGNTOOL (signtool.exe's path), CODESIGN_PFX (the PFX's path)
# and PFX_PASSWORD from the environment; the password reaches signtool only
# as an argument, never a file. Checks read THUMBPRINT, the pinned leaf
# certificate's SHA-1 thumbprint.
#
# Signing and timestamping are two signtool calls, not one /tr on sign: a
# timestamp server's transient failure is then retried alone, on a file that is
# already signed, rather than re-signing it.

Set-StrictMode -Version 3
$ErrorActionPreference = 'Stop'

# The RFC 3161 servers tried in turn. DigiCert's is the usual one; Sectigo's
# is there for when DigiCert's has an outage longer than the retries.
$TimestampUrls = @('http://timestamp.digicert.com', 'http://timestamp.sectigo.com')
$TimestampAttempts = 6

function Invoke-AuthenticodeSigning {
    param([Parameter(Mandatory)][string[]]$Path)
    foreach ($file in $Path) {
        Write-Output "== sign $(Split-Path -Leaf $file)"
        & $env:SIGNTOOL sign /fd SHA256 /f $env:CODESIGN_PFX /p $env:PFX_PASSWORD $file
        if ($LASTEXITCODE -ne 0) {
            throw "signtool sign failed for $file (exit $LASTEXITCODE)"
        }
        Add-Timestamp -Path $file
    }
}

function Add-Timestamp {
    param([Parameter(Mandatory)][string]$Path)
    for ($i = 0; $i -lt $TimestampAttempts; $i++) {
        $url = $TimestampUrls[$i % $TimestampUrls.Count]
        & $env:SIGNTOOL timestamp /tr $url /td SHA256 $Path
        if ($LASTEXITCODE -eq 0) {
            return
        }
        $wait = 5 * ($i + 1)
        Write-Warning "timestamp from $url failed for $(Split-Path -Leaf $Path) (attempt $($i + 1)); retrying in ${wait}s"
        Start-Sleep -Seconds $wait
    }
    throw "no RFC 3161 timestamp for $Path after $TimestampAttempts attempts"
}

# Assert-Authenticode checks a file the way core will: it carries a signature
# whose leaf certificate is the pinned one, with a timestamp so it outlives the
# certificate. With -Trusted it also requires WinVerifyTrust to accept the
# chain, which holds only once the public certificate is in the machine's Root
# and TrustedPublisher stores, as core's installer puts it on a guest.
function Assert-Authenticode {
    param(
        [Parameter(Mandatory)][string]$Path,
        [switch]$Trusted
    )
    $name = Split-Path -Leaf $Path
    $sig = Get-AuthenticodeSignature -LiteralPath $Path
    if ($null -eq $sig.SignerCertificate) {
        throw "$name carries no Authenticode signature ($($sig.Status))"
    }
    $got = $sig.SignerCertificate.Thumbprint.ToUpperInvariant()
    if ($got -ne $env:THUMBPRINT.ToUpperInvariant()) {
        throw "$name is signed by $got ($($sig.SignerCertificate.Subject)), not the pinned $($env:THUMBPRINT)"
    }
    if ($null -eq $sig.TimeStamperCertificate) {
        throw "$name carries no timestamp"
    }
    Write-Output "$name signed by $got ($($sig.SignerCertificate.Subject)), timestamped by $($sig.TimeStamperCertificate.Subject)"
    if (-not $Trusted) {
        return
    }
    if ($sig.Status -ne 'Valid') {
        throw "$name signature status $($sig.Status): $($sig.StatusMessage)"
    }
    # signtool verify /pa is WinVerifyTrust with the generic Authenticode
    # policy, the action core's verifier runs; /v prints the file's digest
    # algorithm, which must be SHA-256.
    $out = & $env:SIGNTOOL verify /pa /v $Path 2>&1 | ForEach-Object { "$_" }
    $code = $LASTEXITCODE
    $out | Write-Output
    if ($code -ne 0) {
        throw "WinVerifyTrust rejected $name (signtool verify exit $code)"
    }
    if (-not ($out -match 'Hash of file \(sha256\)')) {
        throw "$name is not signed with a SHA-256 digest"
    }
}
