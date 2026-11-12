# jumpdb installer for Windows: downloads the release package for this machine's architecture,
# verifies it against SHA256SUMS, installs it to a per-user directory and adds that directory to the user PATH.
#
# Usage (PowerShell):
#   irm https://github.com/zK0G0w/jumpdb/releases/latest/download/install.ps1 | iex
#
# Environment variables:
#   JUMPDB_VERSION      tag to install, e.g. v0.1.0; defaults to the latest stable release
#   JUMPDB_INSTALL_DIR  install directory, must be absolute; defaults to %LOCALAPPDATA%\Programs\jumpdb
#
# No administrator rights needed. Only the current user's PATH is modified; the jumpdb config is never read or
# changed, and downloaded files are not unblocked (no Unblock-File).
# SHA256SUMS comes from the same release as the archive: it detects corrupted downloads, not tampering.
#
# Works with Windows PowerShell 5.1 and PowerShell 7.
# This file MUST stay pure ASCII without a BOM: GitHub serves release assets without a charset, and
# Windows PowerShell 5.1's irm then decodes them as ISO-8859-1, so any non-ASCII byte breaks `irm | iex`.
# When run through iex it shares the caller's session, so everything lives inside a script block (no functions
# or variables leak), and failures never call exit (that would close the user's terminal window).

& {
	Set-StrictMode -Version 2.0
	$ErrorActionPreference = 'Stop'
	# The progress bar makes downloads much slower in Windows PowerShell 5.1.
	$ProgressPreference = 'SilentlyContinue'
	$repo = 'zK0G0w/jumpdb'
	$failPrefix = 'jumpdb install failed: '

	function Fail([string]$message) {
		throw ($failPrefix + $message)
	}

	function Get-Arch {
		if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) { Fail 'only Windows is supported' }
		if (-not [Environment]::Is64BitOperatingSystem) { Fail 'only 64-bit Windows is supported' }
		# Trust the processor, not the process: x64 PowerShell emulated on ARM reports AMD64 in the
		# environment, but the native arm64 build should still be installed.
		$architecture = $null
		try {
			$architecture = @(Get-CimInstance -ClassName Win32_Processor)[0].Architecture
		} catch {
			$architecture = $null
		}
		if ($null -eq $architecture) {
			switch ($env:PROCESSOR_ARCHITECTURE) {
				'AMD64' { return 'amd64' }
				'ARM64' { return 'arm64' }
				default { Fail 'cannot determine the processor architecture' }
			}
		}
		switch ([int]$architecture) {
			9 { return 'amd64' }
			12 { return 'arm64' }
			default { Fail "unsupported processor architecture ($architecture)" }
		}
	}

	# Relax the error policy while running native commands: under Stop, Windows PowerShell 5.1 turns
	# native stderr output into terminating errors.
	function Invoke-Quiet([string]$path, [string[]]$arguments) {
		$ErrorActionPreference = 'Continue'
		try {
			$output = & $path @arguments 2>$null
			return @{ Ok = ($LASTEXITCODE -eq 0); Output = (@($output) -join "`n") }
		} catch {
			return @{ Ok = $false; Output = '' }
		}
	}

	# Returns the binary's version, or $fallback for builds older than the version command (e.g. v0.0.1).
	function Get-BinaryVersion([string]$path, [string]$fallback) {
		$result = Invoke-Quiet $path @('version')
		$fields = @($result.Output.Trim() -split '\s+')
		if ($result.Ok -and $fields.Count -ge 2 -and $fields[0] -eq 'jumpdb') { return $fields[1] }
		return $fallback
	}

	# Expands %VAR% and drops trailing backslashes so PATH entries compare equal regardless of spelling.
	function Get-ComparablePath([string]$path) {
		$expanded = [Environment]::ExpandEnvironmentVariables($path.Trim().Trim('"'))
		if ($expanded -match '^[A-Za-z]:\\$') { return $expanded }
		return $expanded.TrimEnd('\')
	}

	function Test-PathListContains([string]$list, [string]$directory) {
		$wanted = Get-ComparablePath $directory
		foreach ($entry in ($list -split ';')) {
			if ($entry.Trim() -and ((Get-ComparablePath $entry) -ieq $wanted)) { return $true }
		}
		return $false
	}

	# Edits HKCU\Environment\Path directly, keeping its value kind and unexpanded %VAR% entries;
	# [Environment]::SetEnvironmentVariable in Windows PowerShell 5.1 would rewrite it as REG_SZ and break them.
	# Returns $true when the directory was added, $false when it was already present.
	function Add-UserPath([string]$directory) {
		$key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment')
		try {
			$raw = [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
			if (Test-PathListContains $raw $directory) { return $false }
			$kind = [Microsoft.Win32.RegistryValueKind]::ExpandString
			if ($null -ne $key.GetValue('Path')) { $kind = $key.GetValueKind('Path') }
			$trimmed = $raw.TrimEnd(';')
			if ($trimmed) { $updated = "$trimmed;$directory" } else { $updated = $directory }
			$key.SetValue('Path', $updated, $kind)
		} finally {
			$key.Close()
		}
		# Broadcast the change so terminals opened later from Explorer or Windows Terminal see the new PATH.
		# Writing a user variable through .NET sends WM_SETTINGCHANGE for us; the temporary variable is deleted
		# right away. This avoids compiling P/Invoke code.
		try {
			[Environment]::SetEnvironmentVariable('JUMPDB_INSTALL_NOTIFY', '1', 'User')
			# PowerShell turns $null into '' for string parameters, and PowerShell 7 does not delete a user
			# variable for ''; [NullString]::Value passes a real null.
			[Environment]::SetEnvironmentVariable('JUMPDB_INSTALL_NOTIFY', [NullString]::Value, 'User')
		} catch {
			Write-Host 'Note: could not notify Windows about the PATH change; new terminals may not find jumpdb until you sign out and back in.'
		}
		return $true
	}

	$work = $null
	$staged = $null
	try {
		$version = $env:JUMPDB_VERSION
		if (-not $version) { $version = 'latest' }
		$installDir = $env:JUMPDB_INSTALL_DIR
		if (-not $installDir) {
			if (-not $env:LOCALAPPDATA) { Fail '%LOCALAPPDATA% is not set; choose a directory with JUMPDB_INSTALL_DIR' }
			$installDir = Join-Path $env:LOCALAPPDATA 'Programs\jumpdb'
		}
		# Only drive-letter or UNC absolute paths; relative paths and \foo depend on the current location.
		if ($installDir -notmatch '^([A-Za-z]:\\|\\\\[^\\]+\\[^\\]+)') { Fail 'JUMPDB_INSTALL_DIR must be an absolute path' }
		# Drop trailing backslashes (except for a drive root) so it compares cleanly with PATH entries.
		if ($installDir -notmatch '^[A-Za-z]:\\$') { $installDir = $installDir.TrimEnd('\') }
		# The version becomes part of the download URL, so only tag characters starting with v are accepted.
		if ($version -eq 'latest') {
			$base = "https://github.com/$repo/releases/latest/download"
		} else {
			if ($version -cnotmatch '^v') { Fail 'JUMPDB_VERSION must be a tag starting with v, e.g. v0.1.0' }
			if ($version -cnotmatch '^v[0-9A-Za-z.+-]+$') { Fail 'JUMPDB_VERSION contains characters that are not allowed' }
			$base = "https://github.com/$repo/releases/download/$version"
		}
		$arch = Get-Arch
		$asset = "jumpdb-windows-$arch.zip"
		# Windows PowerShell 5.1 may not enable TLS 1.2 by default; only add the protocol, never relax certificate checks.
		[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

		$work = Join-Path ([IO.Path]::GetTempPath()) ('jumpdb-install-' + [guid]::NewGuid().ToString('N'))
		New-Item -ItemType Directory -Path $work | Out-Null

		Write-Host "Downloading $asset ($version)..."
		$zip = Join-Path $work $asset
		$sums = Join-Path $work 'SHA256SUMS'
		try {
			Invoke-WebRequest -Uri "$base/$asset" -OutFile $zip -UseBasicParsing
		} catch {
			Fail "could not download $asset; check that the version exists and the network is available"
		}
		try {
			Invoke-WebRequest -Uri "$base/SHA256SUMS" -OutFile $sums -UseBasicParsing
		} catch {
			Fail 'could not download SHA256SUMS'
		}
		$expected = $null
		foreach ($line in [IO.File]::ReadAllLines($sums)) {
			if ($line -match '^([0-9A-Fa-f]{64}) [ *](.+)$' -and $Matches[2] -ceq $asset) { $expected = $Matches[1] }
		}
		if (-not $expected) { Fail "SHA256SUMS has no entry for $asset" }
		$actual = (Get-FileHash -LiteralPath $zip -Algorithm SHA256).Hash
		if ($actual -ne $expected) { Fail "SHA-256 mismatch for $asset; nothing was installed" }

		$extract = Join-Path $work 'extract'
		try {
			Expand-Archive -LiteralPath $zip -DestinationPath $extract
		} catch {
			Fail "could not extract $asset"
		}
		$binary = Join-Path $extract 'jumpdb.exe'
		$item = Get-Item -LiteralPath $binary -Force -ErrorAction SilentlyContinue
		if ($null -eq $item -or $item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
			Fail 'unexpected archive contents'
		}

		try {
			New-Item -ItemType Directory -Path $installDir -Force | Out-Null
		} catch {
			Fail "could not create install directory $installDir"
		}
		$target = Join-Path $installDir 'jumpdb.exe'
		# Remove old binaries left behind because they were still running during a previous install.
		Get-ChildItem -LiteralPath $installDir -Filter '.jumpdb.old.*.exe' -Force -ErrorAction SilentlyContinue |
			Remove-Item -Force -ErrorAction SilentlyContinue
		$previous = $null
		if (Test-Path -LiteralPath $target) {
			$previous = Get-BinaryVersion $target 'an older version (no version command)'
		}
		$staged = Join-Path $installDir ".jumpdb.install.$PID.exe"
		try {
			Copy-Item -LiteralPath $binary -Destination $staged -Force
		} catch {
			Fail "could not write to install directory $installDir"
		}
		# Make sure the new binary runs here before replacing anything; --help rather than version so that
		# releases without the version command can still be installed.
		if (-not (Invoke-Quiet $staged @('--help')).Ok) { Fail 'the downloaded jumpdb does not run on this machine; the existing file was kept' }
		$installed = Get-BinaryVersion $staged 'unknown version (no version command)'

		# A running exe cannot be overwritten or deleted but can be renamed: move the old file aside, move the
		# new one in, and move the old one back if that fails.
		$old = $null
		if (Test-Path -LiteralPath $target) {
			$old = Join-Path $installDir ".jumpdb.old.$PID.exe"
			try {
				Move-Item -LiteralPath $target -Destination $old -Force
			} catch {
				Fail "could not replace $target; the existing file was kept"
			}
		}
		try {
			Move-Item -LiteralPath $staged -Destination $target
		} catch {
			if ($old) { Move-Item -LiteralPath $old -Destination $target -Force -ErrorAction SilentlyContinue }
			Fail "could not replace $target; the existing file was kept"
		}
		$staged = $null
		$oldLeft = $false
		if ($old) {
			Remove-Item -LiteralPath $old -Force -ErrorAction SilentlyContinue
			$oldLeft = Test-Path -LiteralPath $old
		}

		if ($previous) {
			Write-Host "Upgraded ${target}: $previous -> $installed"
		} else {
			Write-Host "Installed ${target}: $installed"
		}
		if ($oldLeft) {
			Write-Host "The previous version is still running, so $old could not be removed; the next install will clean it up."
		}

		if (Add-UserPath $installDir) {
			Write-Host ''
			Write-Host "Added $installDir to your user PATH. Terminals that were already open must be reopened to find jumpdb."
		}
		# Make jumpdb usable in this session right away; like new terminals, the user PATH comes after the system PATH.
		if (-not (Test-PathListContains $env:Path $installDir)) {
			$env:Path = $env:Path.TrimEnd(';') + ';' + $installDir
		}
		$resolved = @(Get-Command jumpdb -CommandType Application -ErrorAction SilentlyContinue)
		if ($resolved.Count -gt 0) {
			$resolvedDir = Split-Path -Parent $resolved[0].Path
			if ((Get-ComparablePath $resolvedDir) -ine (Get-ComparablePath $installDir)) {
				Write-Host ''
				Write-Host "Warning: the jumpdb found first on PATH is $($resolved[0].Path), not the newly installed $target."
				Write-Host 'Remove that file or move it later in PATH.'
			}
		}

		Write-Host ''
		Write-Host 'Next: run jumpdb --help to see the commands; on first use, run jumpdb token set.'
		$global:LASTEXITCODE = 0
	} catch {
		$message = $_.Exception.Message
		if (-not $message.StartsWith($failPrefix)) { $message = $failPrefix + 'unexpected error (' + $_.Exception.GetType().Name + ')' }
		[Console]::Error.WriteLine($message)
		# No exit: under iex that would close the user's terminal. Report failure through the exit code instead.
		$global:LASTEXITCODE = 1
	} finally {
		# On any failure, remove the download directory and the staged file; never leave a half-written binary.
		if ($staged) { Remove-Item -LiteralPath $staged -Force -ErrorAction SilentlyContinue }
		if ($work) { Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue }
	}
}
