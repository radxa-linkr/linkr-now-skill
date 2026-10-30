# Run the bundled Windows runtime, or fetch the pinned release on first use.
# Compatible with Windows PowerShell 5.1 and PowerShell 7.

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$PSNativeCommandUseErrorActionPreference = $false
$forwardArguments = @($args)
$offlineHint = 'For offline installation, extract the complete ZIP for your Windows architecture from GitHub Releases and use its included executable.'

function Test-LinkrRuntime {
    param([string]$Executable, [string]$Version)

    if (-not [System.IO.File]::Exists($Executable)) {
        return $false
    }
    try {
        $versionOutput = @(& $Executable --version 2>$null)
        $versionExitCode = $LASTEXITCODE
        return ($versionExitCode -eq 0 -and $versionOutput.Count -eq 1 -and
            [string]$versionOutput[0] -ceq ('linkr-now ' + $Version))
    }
    catch {
        return $false
    }
}

function Get-LinkrArchitecture {
    $architecture = ''
    try {
        $architecture = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
    }
    catch {
        # The environment fallback also handles a 32-bit PowerShell process.
        $architecture = [System.Environment]::GetEnvironmentVariable('PROCESSOR_ARCHITEW6432')
        if ([string]::IsNullOrWhiteSpace($architecture)) {
            $architecture = [System.Environment]::GetEnvironmentVariable('PROCESSOR_ARCHITECTURE')
        }
    }
    switch ($architecture.ToLowerInvariant()) {
        'x64' { return 'amd64' }
        'amd64' { return 'amd64' }
        'arm64' { return 'arm64' }
        default { throw ('Unsupported Windows architecture: ' + $architecture + '. Supported architectures are x64 and ARM64.') }
    }
}

function Get-LinkrCacheDirectory {
    $cacheBase = [System.Environment]::GetEnvironmentVariable('LINKR_NOW_CACHE_DIR')
    if ([string]::IsNullOrWhiteSpace($cacheBase)) {
        $localData = [System.Environment]::GetFolderPath([System.Environment+SpecialFolder]::LocalApplicationData)
        if ([string]::IsNullOrWhiteSpace($localData)) {
            throw 'The Windows local application-data directory is unavailable. Set LINKR_NOW_CACHE_DIR to a writable absolute directory.'
        }
        $cacheBase = [System.IO.Path]::Combine($localData, 'linkr-now-skill', 'Cache')
    }
    if (-not [System.IO.Path]::IsPathRooted($cacheBase)) {
        throw 'LINKR_NOW_CACHE_DIR must be an absolute directory path.'
    }
    return [System.IO.Path]::GetFullPath($cacheBase)
}

function Get-LinkrFileSHA256 {
    param([string]$Path)

    # Use .NET directly so hashing does not depend on PowerShell module discovery.
    $stream = $null
    $sha256 = $null
    try {
        $stream = [System.IO.File]::OpenRead($Path)
        $sha256 = [System.Security.Cryptography.SHA256]::Create()
        $digest = $sha256.ComputeHash($stream)
        return [System.BitConverter]::ToString($digest).Replace('-', '').ToLowerInvariant()
    }
    finally {
        if ($null -ne $stream) { $stream.Dispose() }
        if ($null -ne $sha256) { $sha256.Dispose() }
    }
}

function Get-LinkrRuntime {
    param([string]$SkillDirectory, [string]$Version, [string]$Architecture)

    $platform = 'windows-' + $Architecture
    $bundled = [System.IO.Path]::Combine($SkillDirectory, 'bin', $platform, 'linkr-now.exe')
    if (Test-LinkrRuntime $bundled $Version) {
        return $bundled
    }

    $cacheDirectory = [System.IO.Path]::Combine((Get-LinkrCacheDirectory), $Version, $platform)
    $cached = [System.IO.Path]::Combine($cacheDirectory, 'linkr-now.exe')
    if (Test-LinkrRuntime $cached $Version) {
        return $cached
    }
    [void][System.IO.Directory]::CreateDirectory($cacheDirectory)

    # An exclusive file handle is released by Windows even if this process exits.
    $lockPath = [System.IO.Path]::Combine($cacheDirectory, '.install.lock')
    $lock = $null
    $lockDeadline = [System.DateTime]::UtcNow.AddSeconds(90)
    while ($null -eq $lock) {
        try {
            $lock = [System.IO.File]::Open($lockPath, [System.IO.FileMode]::OpenOrCreate,
                [System.IO.FileAccess]::ReadWrite, [System.IO.FileShare]::None)
        }
        catch [System.IO.IOException] {
            # PowerShell may wrap a .NET method failure in another exception.
            $lockFailure = $_.Exception
            while ($null -ne $lockFailure -and -not ($lockFailure -is [System.IO.IOException])) {
                $lockFailure = $lockFailure.InnerException
            }
            if ($null -eq $lockFailure -or ($lockFailure.HResult -band 0xffff) -notin @(32, 33)) {
                throw
            }
            if ([System.DateTime]::UtcNow -ge $lockDeadline) {
                throw 'Timed out waiting for another runtime installation. Retry after the other process finishes.'
            }
            Start-Sleep -Milliseconds 200
        }
    }

    $staging = $null
    $manifest = $null
    try {
        # Another process may have completed installation while we waited.
        if (Test-LinkrRuntime $cached $Version) {
            return $cached
        }

        $asset = 'linkr-now-' + $Version + '-' + $platform + '.exe'
        $releaseBase = 'https://github.com/radxa-linkr/linkr-now-skill/releases/download/' + [System.Uri]::EscapeDataString($Version)
        $temporaryId = [System.Guid]::NewGuid().ToString('N')
        $staging = [System.IO.Path]::Combine($cacheDirectory, '.download-' + $temporaryId + '.exe')
        $manifest = [System.IO.Path]::Combine($cacheDirectory, '.checksums-' + $temporaryId + '.txt')
        [System.Console]::Error.WriteLine('Downloading Linkr Now ' + $Version + ' for ' + $platform + '...')

        # PowerShell 5.1 may otherwise negotiate an obsolete TLS version.
        $priorSecurityProtocol = [System.Net.ServicePointManager]::SecurityProtocol
        try {
            [System.Net.ServicePointManager]::SecurityProtocol = $priorSecurityProtocol -bor [System.Net.SecurityProtocolType]::Tls12
            Invoke-WebRequest -UseBasicParsing -Uri ($releaseBase + '/SHA256SUMS') -OutFile $manifest -TimeoutSec 60 -ErrorAction Stop
            if (([System.IO.FileInfo]$manifest).Length -gt 1048576) {
                throw 'The release checksum manifest exceeds the size limit.'
            }
            Invoke-WebRequest -UseBasicParsing -Uri ($releaseBase + '/' + [System.Uri]::EscapeDataString($asset)) -OutFile $staging -TimeoutSec 60 -ErrorAction Stop
        }
        finally {
            [System.Net.ServicePointManager]::SecurityProtocol = $priorSecurityProtocol
        }
        if (([System.IO.FileInfo]$staging).Length -gt 67108864 -or ([System.IO.FileInfo]$staging).Length -eq 0) {
            throw 'The downloaded runtime has an invalid size.'
        }

        $checksumPattern = '^([a-fA-F0-9]{64})  ' + [System.Text.RegularExpressions.Regex]::Escape($asset) + '$'
        $checksumMatches = @()
        foreach ($line in [System.IO.File]::ReadAllLines($manifest)) {
            $match = [System.Text.RegularExpressions.Regex]::Match($line, $checksumPattern)
            if ($match.Success) {
                $checksumMatches += $match.Groups[1].Value.ToLowerInvariant()
            }
        }
        if ($checksumMatches.Count -ne 1) {
            throw ('The release checksum manifest must contain exactly one entry for ' + $asset + '.')
        }
        $actualChecksum = Get-LinkrFileSHA256 $staging
        if ($actualChecksum -cne $checksumMatches[0]) {
            throw 'Runtime checksum verification failed. The downloaded file will not be used.'
        }
        if (-not (Test-LinkrRuntime $staging $Version)) {
            throw ('The downloaded runtime did not report the required version: linkr-now ' + $Version + '.')
        }

        # Stage on the same volume, then activate without exposing partial data.
        if ([System.IO.File]::Exists($cached)) {
            [System.IO.File]::Replace($staging, $cached, $null)
        }
        else {
            [System.IO.File]::Move($staging, $cached)
        }
        $staging = $null
        return $cached
    }
    finally {
        foreach ($temporary in @($staging, $manifest)) {
            if (-not [string]::IsNullOrEmpty($temporary) -and [System.IO.File]::Exists($temporary)) {
                try { [System.IO.File]::Delete($temporary) } catch { }
            }
        }
        $lock.Dispose()
    }
}

try {
    if ([System.Environment]::OSVersion.Platform -ne [System.PlatformID]::Win32NT) {
        throw 'This launcher supports Windows. Use scripts/linkr-now.sh on macOS or Linux.'
    }
    $skillDirectory = [System.IO.Path]::GetFullPath([System.IO.Path]::Combine($PSScriptRoot, '..'))
    $versionPath = [System.IO.Path]::Combine($skillDirectory, 'VERSION')
    if (-not [System.IO.File]::Exists($versionPath)) {
        throw 'The skill VERSION file is missing. Reinstall the complete skill package.'
    }
    $versionLines = @([System.IO.File]::ReadAllLines($versionPath))
    if ($versionLines.Count -ne 1 -or $versionLines[0] -cnotmatch '^v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?$') {
        throw 'The skill VERSION file must contain one valid release version, such as v0.1.0.'
    }
    $runtimeVersion = $versionLines[0]
    $architecture = Get-LinkrArchitecture
    $runtime = Get-LinkrRuntime $skillDirectory $runtimeVersion $architecture
}
catch {
    [System.Console]::Error.WriteLine('Linkr Now setup failed: ' + $_.Exception.Message)
    [System.Console]::Error.WriteLine($offlineHint)
    exit 1
}

# The installation lock is released before a potentially long monitoring run.
try {
    & $runtime @forwardArguments
    $runtimeExitCode = $LASTEXITCODE
    exit $runtimeExitCode
}
catch {
    [System.Console]::Error.WriteLine('Unable to run Linkr Now: ' + $_.Exception.Message)
    [System.Console]::Error.WriteLine($offlineHint)
    exit 1
}
