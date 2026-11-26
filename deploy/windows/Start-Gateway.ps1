# ASCII source also works in Windows PowerShell 5.1 without a BOM.
[CmdletBinding()]
param([switch]$LoginOnly)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$exitCode = 1
$stateLock = $null
$oldDirectory = Get-Location
$oldEncoding = $OutputEncoding
$savedEnvironment = @{}
foreach ($name in @('CLOUDREVE_EMAIL', 'CLOUDREVE_PASSWORD', 'S3_ACCESS_KEY_ID', 'S3_SECRET_ACCESS_KEY')) {
    $savedEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
}

function Get-Setting($Object, [string]$Name, $Default) {
    $property = $Object.PSObject.Properties[$Name]
    if ($null -eq $property) { return $Default }
    return $property.Value
}

function Get-LocalPath([string]$Value) {
    if ([string]::IsNullOrWhiteSpace($Value)) { throw 'State paths must not be empty.' }
    if ([IO.Path]::IsPathRooted($Value)) { return [IO.Path]::GetFullPath($Value) }
    return [IO.Path]::GetFullPath((Join-Path $PSScriptRoot $Value))
}

function Protect-PrivatePath([string]$Path) {
    $item = Get-Item -LiteralPath $Path -Force
    if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw 'State paths must not use junctions or symbolic links.'
    }
    $user = [Security.Principal.WindowsIdentity]::GetCurrent().User
    $system = [Security.Principal.SecurityIdentifier]::new('S-1-5-18')
    $inherit = [Security.AccessControl.InheritanceFlags]::None
    if ($item.PSIsContainer) {
        $acl = [IO.Directory]::GetAccessControl($Path, [Security.AccessControl.AccessControlSections]::Access)
        $inherit = [Security.AccessControl.InheritanceFlags]'ContainerInherit,ObjectInherit'
    } else { $acl = [IO.File]::GetAccessControl($Path, [Security.AccessControl.AccessControlSections]::Access) }
    # Change only the DACL; audit/owner changes require administrator privileges.
    $acl.SetAccessRuleProtection($true, $false)
    foreach ($rule in @($acl.GetAccessRules($true, $false, [Security.Principal.SecurityIdentifier]))) {
        $acl.RemoveAccessRuleSpecific($rule)
    }
    foreach ($identity in @($user, $system)) {
        $rule = [Security.AccessControl.FileSystemAccessRule]::new(
            $identity, 'FullControl', $inherit, 'None', 'Allow')
        $acl.AddAccessRule($rule)
    }
    if ($item.PSIsContainer) { [IO.Directory]::SetAccessControl($Path, $acl) }
    else { [IO.File]::SetAccessControl($Path, $acl) }
}

function New-S3Credentials([string]$Path) {
    $bytes = [byte[]]::new(32)
    $random = [Security.Cryptography.RandomNumberGenerator]::Create()
    try { $random.GetBytes($bytes) } finally { $random.Dispose() }
    $value = [ordered]@{
        access_key_id = 'gateway-local'
        secret_access_key = [BitConverter]::ToString($bytes).Replace('-', '').ToLowerInvariant()
    }
    $temporary = Join-Path ([IO.Path]::GetDirectoryName($Path)) ('.credentials-' + [Guid]::NewGuid().ToString('N'))
    try {
        [IO.File]::WriteAllText($temporary, ($value | ConvertTo-Json), [Text.UTF8Encoding]::new($false))
        # Fail rather than replacing any existing credentials.
        [IO.File]::Move($temporary, $Path)
    } finally {
        if ([IO.File]::Exists($temporary)) { [IO.File]::Delete($temporary) }
    }
}

function Test-SavedSession([string]$Path, [string]$Site) {
    if (-not [IO.File]::Exists($Path)) { return $false }
    try {
        $session = Get-Content -LiteralPath $Path -Raw -Encoding UTF8 | ConvertFrom-Json
        if ((Get-Setting $session 'site' '') -cne $Site.TrimEnd('/')) { return $false }
        if (-not (Get-Setting $session 'access_token' '') -or -not (Get-Setting $session 'refresh_token' '')) { return $false }
        $expires = Get-Setting $session 'refresh_expires' ''
        if ($expires) {
            $expiry = [DateTimeOffset]::Parse($expires)
            if ($expiry.Year -gt 1 -and $expiry -lt [DateTimeOffset]::UtcNow) { return $false }
        }
        return $true
    } catch { return $false }
}

function Read-Keys([string]$Path) {
    try {
        $value = Get-Content -LiteralPath $Path -Raw -Encoding UTF8 | ConvertFrom-Json
        $access = Get-Setting $value 'access_key_id' ''
        $secret = Get-Setting $value 'secret_access_key' ''
        if ($access -isnot [string] -or $secret -isnot [string] -or $access.Length -lt 3 -or $secret.Length -lt 32) {
            throw 'invalid'
        }
        return $value
    } catch { throw 'Invalid s3-credentials.json. Fix it or restore your saved keys; it was not replaced.' }
}

try {
    Set-Location -LiteralPath $PSScriptRoot
    $binary = Join-Path $PSScriptRoot 'cloudreve-s3.exe'
    $configFile = Join-Path $PSScriptRoot 'gateway.json'
    if (-not [IO.File]::Exists($binary)) { throw 'Gateway executable is missing. Extract the complete Windows package.' }
    if (-not [IO.File]::Exists($configFile)) {
        [IO.File]::Copy((Join-Path $PSScriptRoot 'deploy/gateway.local.example.json'), $configFile, $false)
        throw 'Created gateway.json. Edit site and buckets_root, then run start.cmd again.'
    }
    try { $config = Get-Content -LiteralPath $configFile -Raw -Encoding UTF8 | ConvertFrom-Json }
    catch { throw 'Cannot read gateway.json. Check its JSON syntax; no existing files were replaced.' }
    $site = [string](Get-Setting $config 'site' '')
    $origin = $null
    if (-not [Uri]::TryCreate($site, [UriKind]::Absolute, [ref]$origin) -or $origin.Host.EndsWith('.invalid')) {
        throw 'Edit site and buckets_root in gateway.json before starting.'
    }
    if ($origin.Scheme -notin @('https', 'http') -or [string]::IsNullOrEmpty($origin.Host) -or
        $origin.UserInfo -ne '' -or $origin.Query -ne '' -or $origin.Fragment -ne '' -or
        $origin.AbsolutePath -ne '/' -or $site.Contains('\') -or $site -cne $site.Trim()) {
        throw 'site must contain only http(s)://host and an optional port. Remove webpage/API paths, user info, query strings and fragments.'
    }
    $allowHttp = Get-Setting $config 'allow_http' $false
    if ($allowHttp -isnot [bool]) { throw 'allow_http must be a JSON boolean: true or false, without quotes.' }
    if ($origin.Scheme -eq 'http' -and -not $allowHttp) {
        throw 'site uses HTTP. Prefer an HTTPS Cloudreve origin. For intentional HTTP on a trusted network, set "allow_http": true in gateway.json. HTTP sends login credentials and session tokens without encryption.'
    }
    # A PowerShell 7 parent can leave incompatible modules on PSModulePath.
    Import-Module (Join-Path $PSHOME 'Modules/Microsoft.PowerShell.Security/Microsoft.PowerShell.Security.psd1') -ErrorAction Stop
    $sessionFile = Get-LocalPath ([string](Get-Setting $config 'session_file' '.state/session.json'))
    $stateDirectory = [IO.Path]::GetDirectoryName($sessionFile)
    $rootPrefix = $PSScriptRoot.TrimEnd([IO.Path]::DirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar
    if (-not $stateDirectory.StartsWith($rootPrefix, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'The Windows launcher requires session_file in a dedicated subdirectory of the extracted package.'
    }
    $current = $stateDirectory
    while ($current.Length -gt $PSScriptRoot.Length) {
        if (Test-Path -LiteralPath $current) {
            if (((Get-Item -LiteralPath $current -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
                throw 'State paths must not traverse junctions or symbolic links.'
            }
        }
        $current = [IO.Path]::GetDirectoryName($current)
    }
    [IO.Directory]::CreateDirectory($stateDirectory) | Out-Null
    Protect-PrivatePath $stateDirectory
    try {
        $stateLock = [IO.File]::Open((Join-Path $stateDirectory '.windows-launcher.lock'), 'OpenOrCreate', 'ReadWrite', 'None')
    } catch { throw 'This instance is already running or its state is inaccessible. Do not start a second copy.' }
    $credentialsFile = Join-Path $stateDirectory 's3-credentials.json'
    if ([IO.File]::Exists($credentialsFile)) {
        Protect-PrivatePath $credentialsFile
        $credentials = Read-Keys $credentialsFile
    }
    if ([IO.File]::Exists($sessionFile)) { Protect-PrivatePath $sessionFile }
    $env:CLOUDREVE_PASSWORD = $null
    $OutputEncoding = [Text.UTF8Encoding]::new($false)
    if ($LoginOnly -or -not (Test-SavedSession $sessionFile $site)) {
        Write-Host 'Sign in to Cloudreve. Only the session is saved, not your password.'
        $env:CLOUDREVE_EMAIL = Read-Host 'Cloudreve email'
        $password = Read-Host 'Cloudreve password' -AsSecureString
        try {
            $ErrorActionPreference = 'Continue'
            # The CLI emits sanitized errors: never upstream bodies, URLs or credentials.
            $loginOutput = [Net.NetworkCredential]::new('', $password).Password |
                & $binary login -config $configFile -password-stdin 2>&1
            $loginExit = $LASTEXITCODE
        } finally {
            $ErrorActionPreference = 'Stop'
            $password.Dispose()
            $env:CLOUDREVE_EMAIL = $null
        }
        if ($loginExit -ne 0) {
            $loginReason = (@($loginOutput) | ForEach-Object { $_.ToString().Trim() } | Where-Object { $_ }) -join ' '
            if ([string]::IsNullOrWhiteSpace($loginReason)) { $loginReason = 'The executable returned no error details.' }
            if ($loginReason.Length -gt 1024) { $loginReason = $loginReason.Substring(0, 1024) }
            throw ('Login failed (exit ' + $loginExit + '): ' + $loginReason + '. Existing sessions and S3 keys were not replaced by the launcher.')
        }
        Write-Host 'Cloudreve login succeeded; session saved. Password was not persisted.'
        Protect-PrivatePath $sessionFile
    }
    if (-not [IO.File]::Exists($credentialsFile)) { New-S3Credentials $credentialsFile }
    Protect-PrivatePath $credentialsFile
    $credentials = Read-Keys $credentialsFile
    Write-Host ('S3 keys: ' + $credentialsFile + ' (private file; copy these keys to your S3 client)')
    if ($LoginOnly) {
        Write-Host 'Login saved. Run start.cmd to start the gateway.'
        $exitCode = 0
    } else {
        $env:S3_ACCESS_KEY_ID = $credentials.access_key_id
        $env:S3_SECRET_ACCESS_KEY = $credentials.secret_access_key
        Write-Host ('S3 listen: ' + (Get-Setting $config 'listen' '127.0.0.1:7070'))
        Write-Host ('Region: ' + (Get-Setting $config 'region' 'us-east-1'))
        # Report directory semantics without exposing the configured remote path.
        if ((Get-Setting $config 'single_bucket_mode' $false) -eq $true) {
            Write-Host 'Bucket mode: single; the configured directory itself is the same-name bucket.'
        } elseif (-not [string]::IsNullOrWhiteSpace([string](Get-Setting $config 'buckets_root' ''))) {
            Write-Host 'Bucket mode: multiple; immediate child folders of the configured directory are buckets.'
        } else {
            Write-Host 'Bucket mode: explicit configured bucket(s).'
        }
        Write-Host 'Keep this window open. Press Ctrl+C to stop. Run login.cmd if the server rejects your saved session.'
        $ErrorActionPreference = 'Continue'
        & $binary serve -config $configFile
        $exitCode = $LASTEXITCODE
    }
} catch {
    Write-Host ('Gateway stopped: ' + $_.Exception.Message) -ForegroundColor Red
} finally {
    if ($null -ne $stateLock) { $stateLock.Dispose() }
    foreach ($name in $savedEnvironment.Keys) {
        [Environment]::SetEnvironmentVariable($name, $savedEnvironment[$name], 'Process')
    }
    $OutputEncoding = $oldEncoding
    Set-Location -LiteralPath $oldDirectory.Path
}
exit $exitCode
