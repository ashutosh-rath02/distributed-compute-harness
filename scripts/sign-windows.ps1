# Authenticode-signs Windows binaries in place and checks every result.
# scripts/sign-windows.sh runs it (from the build scripts) only when a
# certificate is configured, with ONE of:
#
#   HARNESS_SIGN_THUMBPRINT     the thumbprint of a code-signing certificate in
#                               Cert:\CurrentUser\My. A certificate on a USB
#                               token, or behind a cloud HSM's key storage
#                               provider (DigiCert KeyLocker, SSL.com eSigner,
#                               ...), shows up there once its software is installed.
#   HARNESS_SIGN_PFX            a .pfx file, its password in
#                               HARNESS_SIGN_PFX_PASS_FILE (default: the .pfx
#                               path + ".pass"; no such file = no password).
#                               Publicly trusted certificates issued since June
#                               2023 never come as a .pfx (their keys must stay
#                               in certified hardware), so this is for an older
#                               certificate, a company CA, or testing.
#   HARNESS_SIGN_SIGNTOOL_ARGS  signtool arguments that pick the key themselves,
#                               e.g. Azure Artifact Signing (formerly Trusted Signing):
#                                 /dlib "C:\...\x64\Azure.CodeSigning.Dlib.dll" /dmdf "C:\...\metadata.json"
#                               Needs signtool.exe. May be added to either of the above.
# and optionally:
#   HARNESS_SIGN_TIMESTAMP_URL  RFC 3161 timestamp server (default
#                               http://timestamp.digicert.com; for Artifact Signing
#                               use http://timestamp.acs.microsoft.com). The timestamp
#                               keeps signatures valid after the certificate expires;
#                               "off" leaves it out, and then they expire with it.
#   HARNESS_SIGNTOOL            the signtool.exe to use (default: the newest x64 one
#                               in the Windows SDK, else one on PATH)
#   HARNESS_SIGN_CACHE          where signed copies are kept (default build\signed-cache)
#
# signtool.exe signs when found (SHA-256, RFC 3161 timestamp). Without it,
# Windows PowerShell's Set-AuthenticodeSignature does (SHA-256; it
# timestamps with the older Authenticode protocol, checked 2026-10-04,
# which Windows accepts the same way); a cloud HSM through
# HARNESS_SIGN_SIGNTOOL_ARGS always needs signtool. Every file is then
# checked with Get-AuthenticodeSignature, and the build fails unless the
# signature is Valid, made with the configured certificate, and timestamped.
#
# Go builds are reproducible, so a rebuild of the same code gives the same
# unsigned file; its signed copy is then reused from the cache. Same signed
# bytes: agents don't all show "update available" after every build, and
# the APK and the Windows bundle carry identical agents.
#
# HARNESS_SIGN_ALLOW_UNTRUSTED_ROOT=1 is for testing with a self-signed
# certificate only: it accepts a signature whose one fault is a root
# Windows doesn't trust, still requiring an intact file signed with the
# configured certificate. Never set it for a release.
param([Parameter(Mandatory = $true, ValueFromRemainingArguments = $true)][string[]]$Files)
$ErrorActionPreference = "Stop"

$thumb = ("" + $env:HARNESS_SIGN_THUMBPRINT) -replace '[^0-9A-Fa-f]', ''
$thumb = $thumb.ToUpper()
$pfx = $env:HARNESS_SIGN_PFX
$extra = $env:HARNESS_SIGN_SIGNTOOL_ARGS
$ts = $env:HARNESS_SIGN_TIMESTAMP_URL
if (-not $ts) { $ts = "http://timestamp.digicert.com" }
if ($ts -eq "off") {
  $ts = ""
  Write-Warning "signing without a timestamp (HARNESS_SIGN_TIMESTAMP_URL=off): the signatures stop being valid when the certificate expires"
}
if ($thumb -and $pfx) { throw "set HARNESS_SIGN_THUMBPRINT or HARNESS_SIGN_PFX, not both" }

$cert = $null
$pass = ""
if ($thumb) {
  $cert = Get-Item -Path ("Cert:\CurrentUser\My\" + $thumb) -ErrorAction SilentlyContinue
  if (-not $cert) { throw "HARNESS_SIGN_THUMBPRINT: there is no certificate $thumb in Cert:\CurrentUser\My" }
} elseif ($pfx) {
  $pfx = (Resolve-Path -LiteralPath $pfx).ProviderPath
  $passFile = $env:HARNESS_SIGN_PFX_PASS_FILE
  if (-not $passFile) { $passFile = $pfx + ".pass" }
  if (Test-Path -LiteralPath $passFile) { $pass = [IO.File]::ReadAllText((Resolve-Path -LiteralPath $passFile).ProviderPath).Trim() }
  # Not Get-PfxCertificate: Windows PowerShell's has no -Password and would prompt.
  $cert = New-Object System.Security.Cryptography.X509Certificates.X509Certificate2($pfx, $pass)
}
if ($cert) {
  if (-not $cert.HasPrivateKey) { throw "the certificate $($cert.Thumbprint) ($($cert.Subject)) has no private key on this PC" }
  if ($cert.NotAfter -lt (Get-Date)) { throw "the certificate $($cert.Thumbprint) expired on $($cert.NotAfter)" }
  $eku = $cert.Extensions | Where-Object { $_ -is [System.Security.Cryptography.X509Certificates.X509EnhancedKeyUsageExtension] }
  if ($eku -and -not ($eku.EnhancedKeyUsages | Where-Object { $_.Value -eq "1.3.6.1.5.5.7.3.3" })) {
    throw "the certificate $($cert.Thumbprint) ($($cert.Subject)) is not a code-signing certificate"
  }
}

$signtool = $env:HARNESS_SIGNTOOL
if (-not $signtool -and ${env:ProgramFiles(x86)}) {
  $kits = Join-Path ${env:ProgramFiles(x86)} "Windows Kits\10\bin"
  $found = Get-ChildItem -Path $kits -Filter signtool.exe -Recurse -ErrorAction SilentlyContinue |
    Where-Object { $_.Directory.Name -eq "x64" } |
    Sort-Object { $v = [version]"0.0"; [void][version]::TryParse($_.Directory.Parent.Name, [ref]$v); $v } |
    Select-Object -Last 1
  if ($found) { $signtool = $found.FullName }
}
if (-not $signtool) {
  $onPath = Get-Command signtool.exe -ErrorAction SilentlyContinue
  if ($onPath) { $signtool = $onPath.Source }
}
if ($signtool -and -not (Test-Path -LiteralPath $signtool)) { throw "HARNESS_SIGNTOOL: $signtool doesn't exist" }
if ($extra -and -not $signtool) { throw "HARNESS_SIGN_SIGNTOOL_ARGS needs signtool.exe (Windows SDK), and none was found; set HARNESS_SIGNTOOL" }

function Invoke-Sign([string]$file) {
  if ($signtool) {
    $a = @("sign", "/fd", "sha256")
    if ($ts) { $a += @("/tr", $ts, "/td", "sha256") }
    if ($thumb) { $a += @("/s", "My", "/sha1", $thumb) }
    elseif ($pfx) {
      $a += @("/f", $pfx)
      if ($pass) { $a += @("/p", $pass) }
    }
    if ($extra) { $a += @([regex]::Matches($extra, '"[^"]*"|\S+') | ForEach-Object { $_.Value.Trim('"') }) }
    & $signtool @a $file | Out-Host
    if ($LASTEXITCODE -ne 0) { throw "signtool failed on $file (exit $LASTEXITCODE)" }
  } else {
    $p = @{ FilePath = $file; Certificate = $cert; HashAlgorithm = "SHA256" }
    if ($ts) { $p.TimestampServer = $ts }
    Set-AuthenticodeSignature @p | Out-Null
  }
}

# Assert-Signed fails unless $file's signature is Valid (or, only with
# HARNESS_SIGN_ALLOW_UNTRUSTED_ROOT=1, untrusted for no other reason than
# its root), made with the configured certificate, and timestamped.
function Assert-Signed([string]$file, [string]$from) {
  $s = Get-AuthenticodeSignature -LiteralPath $file
  $ok = $s.Status -eq "Valid"
  if (-not $ok -and $env:HARNESS_SIGN_ALLOW_UNTRUSTED_ROOT -eq "1" -and $s.Status -eq "UnknownError" -and $s.SignerCertificate) {
    # Get-AuthenticodeSignature reports an untrusted root as UnknownError
    # with a localized message: ask the certificate chain instead.
    $chain = New-Object System.Security.Cryptography.X509Certificates.X509Chain
    $chain.ChainPolicy.RevocationMode = "NoCheck"
    [void]$chain.Build($s.SignerCertificate)
    $faults = @($chain.ChainStatus | ForEach-Object { $_.Status.ToString() })
    $ok = $faults.Count -gt 0 -and @($faults | Where-Object { $_ -ne "UntrustedRoot" }).Count -eq 0
    if ($ok) { Write-Warning "$file is signed by a certificate Windows doesn't trust: accepted only because HARNESS_SIGN_ALLOW_UNTRUSTED_ROOT=1 (never for a release)" }
  }
  if (-not $ok) { throw "$file failed its signature check$from - $($s.Status): $($s.StatusMessage)" }
  if ($cert -and $s.SignerCertificate.Thumbprint -ne $cert.Thumbprint) {
    throw "$file is signed by $($s.SignerCertificate.Thumbprint)$from, not by the configured certificate $($cert.Thumbprint)"
  }
  if ($ts -and -not $s.TimeStamperCertificate) { throw "$file has no timestamp${from}: its signature would stop being valid when the certificate expires" }
  return $s
}

$cacheDir = $env:HARNESS_SIGN_CACHE
if (-not $cacheDir) { $cacheDir = Join-Path (Split-Path -Parent $PSScriptRoot) "build\signed-cache" }
$sha = [System.Security.Cryptography.SHA256]::Create()
$who = ""
if ($cert) { $who = $cert.Thumbprint }
$signer = ([BitConverter]::ToString($sha.ComputeHash([Text.Encoding]::UTF8.GetBytes("$who|$extra|$ts"))) -replace '-', '').Substring(0, 16).ToLower()

try {
  foreach ($f in $Files) {
    $file = (Resolve-Path -LiteralPath $f).ProviderPath
    $unsigned = (Get-FileHash -LiteralPath $file -Algorithm SHA256).Hash.ToLower()
    $cached = Join-Path $cacheDir ("$unsigned-$signer" + [IO.Path]::GetExtension($file))
    if (Test-Path -LiteralPath $cached) {
      Copy-Item -LiteralPath $cached -Destination $file -Force
      $s = Assert-Signed $file " (a signed copy from $cached; delete it to sign afresh)"
      Write-Host "reused the signed $file ($($s.SignerCertificate.Subject))"
      continue
    }
    Invoke-Sign $file
    $s = Assert-Signed $file ""
    New-Item -ItemType Directory -Force -Path $cacheDir | Out-Null
    Copy-Item -LiteralPath $file -Destination $cached -Force
    Write-Host "signed $file ($($s.SignerCertificate.Subject))"
  }
} finally {
  # A .pfx key was loaded into a temporary key container: drop it now.
  if ($pfx -and $cert) { $cert.Reset() }
}
