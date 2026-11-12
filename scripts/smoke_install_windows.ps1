# install.ps1 的 Windows 合成 smoke：用给定的 jumpdb.exe 构造本地 Release 资产，逐个用例在子进程中执行 README 中的安装命令。
#
# 用法：
#   powershell -NoProfile -ExecutionPolicy Bypass -File scripts/smoke_install_windows.ps1 -Binary <jumpdb.exe>
#
# - install.ps1 由本机 127.0.0.1 回环服务以 application/octet-stream（无 charset，与 GitHub Release 资产相同）提供，
#   子进程按文档命令取原始字节自行 UTF-8 解码后 iex，验证中文提示不乱码（irm | iex 在 5.1 下会乱码且无法执行）。
# - 资产下载通过子进程中同名的 Invoke-WebRequest 函数从本地目录提供，回环地址仍交给真实 cmdlet；
#   install.ps1 本身不提供改下载来源的开关。
# - 会读写当前用户的 PATH（HKCU\Environment），结束时恢复原值；只应在 CI runner 或可丢弃的环境中运行。
# - 默认依次用 Windows PowerShell 5.1 与 PowerShell 7（若已安装）作为执行 install.ps1 的 shell。

param(
	[Parameter(Mandatory = $true)][string]$Binary,
	[string[]]$Shells
)

Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$repoRoot = Split-Path -Parent $PSScriptRoot
$installer = Join-Path $PSScriptRoot 'install.ps1'
$Binary = (Resolve-Path -LiteralPath $Binary).Path
$utf8Bom = New-Object Text.UTF8Encoding($true)
$utf8 = New-Object Text.UTF8Encoding($false)
$failures = New-Object Collections.Generic.List[string]

# 安装命令自行按 UTF-8 解码，BOM 会成为脚本开头的 U+FEFF；install.ps1 必须保存为无 BOM 的 UTF-8。
$head = [IO.File]::ReadAllBytes($installer)
if ($head.Length -ge 3 -and $head[0] -eq 0xEF -and $head[1] -eq 0xBB -and $head[2] -eq 0xBF) {
	[Console]::Error.WriteLine('FAIL install.ps1 不应带 UTF-8 BOM')
	exit 1
}

if (-not $Shells) {
	$Shells = @('powershell')
	if (Get-Command pwsh -ErrorAction SilentlyContinue) { $Shells += 'pwsh' }
}

function Get-HostArch {
	switch ([int]@(Get-CimInstance -ClassName Win32_Processor)[0].Architecture) {
		9 { return 'amd64' }
		12 { return 'arm64' }
		default { throw '不支持的 runner 架构' }
	}
}

# 构造一个本地 Release 目录：<asset> 压缩包与 SHA256SUMS。$exe 为放入压缩包的 jumpdb.exe 来源。
function New-Release([string]$directory, [string]$asset, [string]$exe, [string]$sumsMode) {
	New-Item -ItemType Directory -Path $directory -Force | Out-Null
	$stage = Join-Path $directory 'stage'
	New-Item -ItemType Directory -Path $stage -Force | Out-Null
	Copy-Item -LiteralPath $exe -Destination (Join-Path $stage 'jumpdb.exe')
	Copy-Item -LiteralPath (Join-Path $repoRoot 'README.md') -Destination (Join-Path $stage 'README.md')
	$zip = Join-Path $directory $asset
	Compress-Archive -Path (Join-Path $stage 'jumpdb.exe'), (Join-Path $stage 'README.md') -DestinationPath $zip
	Remove-Item -LiteralPath $stage -Recurse -Force
	$hash = (Get-FileHash -LiteralPath $zip -Algorithm SHA256).Hash.ToLowerInvariant()
	switch ($sumsMode) {
		'good' { $lines = @("$hash  $asset") }
		'mismatch' { $lines = @(('0' * 64) + "  $asset") }
		'missing' { $lines = @(('0' * 64) + '  jumpdb-other.zip') }
	}
	[IO.File]::WriteAllLines((Join-Path $directory 'SHA256SUMS'), $lines, $utf8)
}

# 在后台 runspace 中以 HTTP/1.1 回环服务反复提供 install.ps1 的原始字节。
function Start-ScriptServer([byte[]]$bytes) {
	$listener = New-Object Net.Sockets.TcpListener([Net.IPAddress]::Loopback, 0)
	$listener.Start()
	$runner = [powershell]::Create()
	[void]$runner.AddScript({
		param($listener, $bytes)
		while ($true) {
			try { $client = $listener.AcceptTcpClient() } catch { break }
			try {
				$stream = $client.GetStream()
				$reader = New-Object IO.StreamReader($stream, [Text.Encoding]::ASCII, $false, 1024, $true)
				while ($true) {
					$line = $reader.ReadLine()
					if ($null -eq $line -or $line -eq '') { break }
				}
				$header = [Text.Encoding]::ASCII.GetBytes("HTTP/1.1 200 OK`r`nContent-Type: application/octet-stream`r`nContent-Length: $($bytes.Length)`r`nConnection: close`r`n`r`n")
				$stream.Write($header, 0, $header.Length)
				$stream.Write($bytes, 0, $bytes.Length)
				$stream.Flush()
			} catch {
			} finally {
				$client.Close()
			}
		}
	}).AddArgument($listener).AddArgument($bytes)
	$handle = $runner.BeginInvoke()
	return @{ Listener = $listener; Runner = $runner; Handle = $handle; Url = "http://127.0.0.1:$($listener.LocalEndpoint.Port)/install.ps1" }
}

function Get-UserPathState {
	$key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment')
	try {
		if ($null -eq $key.GetValue('Path')) { return @{ Exists = $false; Raw = ''; Kind = $null } }
		return @{
			Exists = $true
			Raw = [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
			Kind = $key.GetValueKind('Path')
		}
	} finally {
		$key.Close()
	}
}

function Set-UserPathState($state) {
	$key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment')
	try {
		if ($state.Exists) { $key.SetValue('Path', $state.Raw, $state.Kind) } else { $key.DeleteValue('Path', $false) }
	} finally {
		$key.Close()
	}
}

# 在独立子进程中执行文档中的安装命令：$envs 为该进程的环境变量（值为 $null 表示删除），
# $prelude / $postlude 为执行前后在同一会话中运行的代码。
function Invoke-Installer([string]$shell, [hashtable]$envs, [string]$prelude, [string]$postlude) {
	$caseDir = Join-Path $script:root ('case-' + [guid]::NewGuid().ToString('N'))
	New-Item -ItemType Directory -Path $caseDir | Out-Null
	$uriLog = Join-Path $caseDir 'uris.log'
	$assignments = foreach ($name in $envs.Keys) {
		if ($null -eq $envs[$name]) { "Remove-Item -LiteralPath 'Env:$name' -ErrorAction SilentlyContinue" }
		else { "`${env:$name} = '" + ([string]$envs[$name]).Replace("'", "''") + "'" }
	}
	$child = @"
`$ErrorActionPreference = 'Continue'
[Console]::OutputEncoding = New-Object Text.UTF8Encoding(`$false)
$($assignments -join "`n")
`$fixtureDir = '$($script:releaseDir.Replace("'", "''"))'
`$uriLog = '$($uriLog.Replace("'", "''"))'
function Invoke-WebRequest {
	param([string]`$Uri, [string]`$OutFile, [switch]`$UseBasicParsing)
	if (`$Uri.StartsWith('http://127.0.0.1:')) { return Microsoft.PowerShell.Utility\Invoke-WebRequest -Uri `$Uri -UseBasicParsing }
	Add-Content -LiteralPath `$uriLog -Value `$Uri
	if (`$Uri -notmatch '^https://github\.com/zK0G0w/jumpdb/releases/(latest/download|download/v[0-9A-Za-z.+-]+)/([^/]+)$') { throw 'unexpected uri' }
	`$source = Join-Path `$fixtureDir `$Matches[2]
	if (-not (Test-Path -LiteralPath `$source)) { throw 'not found' }
	Copy-Item -LiteralPath `$source -Destination `$OutFile
}
$prelude
iex ([Text.Encoding]::UTF8.GetString((iwr -useb '$($script:server.Url)').Content))
`$code = `$LASTEXITCODE
Write-Output 'SESSION-ALIVE'
$postlude
exit `$code
"@
	$childFile = Join-Path $caseDir 'child.ps1'
	[IO.File]::WriteAllText($childFile, $child, $utf8Bom)
	$info = New-Object Diagnostics.ProcessStartInfo
	# 用完整路径启动，不依赖 CreateProcess 对裸名称的搜索规则。
	$info.FileName = @(Get-Command $shell -CommandType Application)[0].Path
	$info.Arguments = "-NoProfile -NonInteractive -ExecutionPolicy Bypass -File `"$childFile`""
	$info.UseShellExecute = $false
	$info.RedirectStandardOutput = $true
	$info.RedirectStandardError = $true
	$info.StandardOutputEncoding = $utf8
	$info.StandardErrorEncoding = $utf8
	$process = [Diagnostics.Process]::Start($info)
	$stdout = $process.StandardOutput.ReadToEndAsync()
	$stderr = $process.StandardError.ReadToEnd()
	$process.WaitForExit()
	$uris = @()
	if (Test-Path -LiteralPath $uriLog) { $uris = @(Get-Content -LiteralPath $uriLog) }
	return @{ Code = $process.ExitCode; Out = $stdout.Result; Err = $stderr; Uris = $uris }
}

function Assert([bool]$condition, [string]$message, $result) {
	if ($condition) { return }
	$detail = ''
	if ($result) { $detail = "`n  exit=$($result.Code)`n  stdout: $($result.Out)`n  stderr: $($result.Err)" }
	$failures.Add("[$script:currentShell] $message$detail")
	# 立即输出，后续用例异常中止时也能看到最早的失败。
	[Console]::Error.WriteLine("FAIL [$script:currentShell] $message$detail")
}

function Assert-Contains([string]$text, [string]$expected, [string]$message, $result) {
	Assert ($text.Contains($expected)) "$message（缺少：$expected）" $result
}

function Get-Sha([string]$path) {
	return (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash
}

# 失败用例的公共断言：退出码 1、会话未被关闭、原有安装文件不变、未留下临时文件。
function Assert-FailedCleanly($result, [string]$directory, [string]$beforeHash, [string]$message, [string]$expectedError) {
	Assert ($result.Code -eq 1) "$message：退出码应为 1" $result
	Assert-Contains $result.Out 'SESSION-ALIVE' "$message：失败后会话应保持可用" $result
	Assert-Contains $result.Err $expectedError "$message：错误提示" $result
	$target = Join-Path $directory 'jumpdb.exe'
	if ($beforeHash) {
		Assert ((Test-Path -LiteralPath $target) -and (Get-Sha $target) -eq $beforeHash) "$message：原有文件应保持不变" $result
	} else {
		Assert (-not (Test-Path -LiteralPath $target)) "$message：不应留下 jumpdb.exe" $result
	}
	if (Test-Path -LiteralPath $directory) {
		$leftover = @(Get-ChildItem -LiteralPath $directory -Filter '.jumpdb.install.*' -Force)
		Assert ($leftover.Count -eq 0) "$message：不应留下临时文件" $result
	}
}

function Test-UserPathCount([string]$directory) {
	$count = 0
	foreach ($entry in ((Get-UserPathState).Raw -split ';')) {
		if ($entry -and ([Environment]::ExpandEnvironmentVariables($entry).TrimEnd('\') -ieq $directory.TrimEnd('\'))) { $count++ }
	}
	return $count
}

function New-LocalAppData {
	$local = Join-Path $script:root ('local-' + [guid]::NewGuid().ToString('N'))
	New-Item -ItemType Directory -Path $local | Out-Null
	return $local
}

function Install-Good([string]$shell, [string]$local) {
	$script:releaseDir = $script:goodRelease
	$result = Invoke-Installer $shell @{ LOCALAPPDATA = $local; JUMPDB_VERSION = $null; JUMPDB_INSTALL_DIR = $null } '' ''
	Assert ($result.Code -eq 0) '前置安装应成功' $result
}

$arch = Get-HostArch
$asset = "jumpdb-windows-$arch.zip"
$expectedVersion = (@(& $Binary version) -join ' ').Split(' ')[1]
$binaryHash = Get-Sha $Binary
$oldExe = Join-Path $env:SystemRoot 'System32\whoami.exe'
$script:root = Join-Path ([IO.Path]::GetTempPath()) ('jumpdb-install-smoke-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $script:root | Out-Null
$brokenExe = Join-Path $script:root 'broken.exe'
[IO.File]::WriteAllText($brokenExe, 'not an executable', $utf8)
$script:goodRelease = Join-Path $script:root 'release-good'
$mismatchRelease = Join-Path $script:root 'release-mismatch'
$missingRelease = Join-Path $script:root 'release-missing'
$brokenRelease = Join-Path $script:root 'release-broken'
$emptyRelease = Join-Path $script:root 'release-empty'
New-Release $script:goodRelease $asset $Binary 'good'
New-Release $mismatchRelease $asset $Binary 'mismatch'
New-Release $missingRelease $asset $Binary 'missing'
New-Release $brokenRelease $asset $brokenExe 'good'
New-Item -ItemType Directory -Path $emptyRelease | Out-Null

$originalPath = Get-UserPathState
$script:server = Start-ScriptServer ([IO.File]::ReadAllBytes($installer))
$uiProcess = $null
try {
	foreach ($shell in $Shells) {
		$script:currentShell = $shell
		Write-Host "== $shell"

		# 首次安装：默认最新版与默认目录，加入用户 PATH，当前会话立即可用，不在会话中留下函数。
		Set-UserPathState $originalPath
		$local = New-LocalAppData
		$dir = Join-Path $local 'Programs\jumpdb'
		$target = Join-Path $dir 'jumpdb.exe'
		$script:releaseDir = $script:goodRelease
		$postlude = @'
Write-Output ('RESOLVED=' + @(Get-Command jumpdb -CommandType Application)[0].Path)
if (Get-Command Fail -ErrorAction SilentlyContinue) { Write-Output 'LEAKED' }
'@
		$result = Invoke-Installer $shell @{ LOCALAPPDATA = $local; JUMPDB_VERSION = $null; JUMPDB_INSTALL_DIR = $null } '' $postlude
		Assert ($result.Code -eq 0) '首次安装应成功' $result
		Assert-Contains $result.Out "已安装 ${target}：$expectedVersion" '首次安装提示' $result
		Assert-Contains $result.Out "已把 $dir 加入当前用户的 PATH" 'PATH 写入提示' $result
		Assert-Contains $result.Out '下一步：运行 jumpdb --help 查看命令' '下一步提示' $result
		Assert-Contains $result.Out "RESOLVED=$target" '当前会话应能直接找到 jumpdb' $result
		Assert (-not $result.Out.Contains('LEAKED')) '不应在调用者会话中留下函数' $result
		Assert (($result.Uris -join '|') -eq "https://github.com/zK0G0w/jumpdb/releases/latest/download/$asset|https://github.com/zK0G0w/jumpdb/releases/latest/download/SHA256SUMS") '应从 latest 下载压缩包与校验文件' $result
		Assert ((Test-Path -LiteralPath $target) -and (Get-Sha $target) -eq $binaryHash) '应安装压缩包中的二进制' $result
		Assert ((Test-UserPathCount $dir) -eq 1) '用户 PATH 应恰好包含一次安装目录' $result
		$expectedKind = [Microsoft.Win32.RegistryValueKind]::ExpandString
		if ($originalPath.Exists) { $expectedKind = $originalPath.Kind }
		Assert ((Get-UserPathState).Kind -eq $expectedKind) '应保留用户 PATH 的值类型' $result
		Assert ($null -eq [Environment]::GetEnvironmentVariable('JUMPDB_INSTALL_NOTIFY', 'User')) '不应留下刷新通知用的临时变量' $result

		# 再次安装：显示升级，PATH 不重复写入。
		$result = Invoke-Installer $shell @{ LOCALAPPDATA = $local; JUMPDB_VERSION = $null; JUMPDB_INSTALL_DIR = $null } '' ''
		Assert ($result.Code -eq 0) '再次安装应成功' $result
		Assert-Contains $result.Out "已升级 ${target}：$expectedVersion → $expectedVersion" '升级提示' $result
		Assert (-not $result.Out.Contains('加入当前用户的 PATH')) '已在 PATH 中时不应再次写入' $result
		Assert ((Test-UserPathCount $dir) -eq 1) '再次安装后 PATH 不应重复' $result

		# 覆盖不支持 version 命令的旧版本。
		Set-UserPathState $originalPath
		$local = New-LocalAppData
		$dir = Join-Path $local 'Programs\jumpdb'
		$target = Join-Path $dir 'jumpdb.exe'
		New-Item -ItemType Directory -Path $dir | Out-Null
		Copy-Item -LiteralPath $oldExe -Destination $target
		$result = Invoke-Installer $shell @{ LOCALAPPDATA = $local; JUMPDB_VERSION = $null; JUMPDB_INSTALL_DIR = $null } '' ''
		Assert ($result.Code -eq 0) '覆盖旧版本应成功' $result
		Assert-Contains $result.Out "已升级 ${target}：旧版本（不支持 version 命令） → $expectedVersion" '旧版本升级提示' $result
		Assert ((Get-Sha $target) -eq $binaryHash) '旧版本应被替换' $result

		# 指定版本与自定义目录（含空格和末尾反斜杠）。
		Set-UserPathState $originalPath
		$local = New-LocalAppData
		$custom = Join-Path $local 'custom dir'
		$result = Invoke-Installer $shell @{ LOCALAPPDATA = $local; JUMPDB_VERSION = 'v9.9.9'; JUMPDB_INSTALL_DIR = "$custom\" } '' ''
		Assert ($result.Code -eq 0) '指定版本与目录应成功' $result
		Assert ($result.Uris -contains "https://github.com/zK0G0w/jumpdb/releases/download/v9.9.9/$asset") '应下载指定版本' $result
		Assert (Test-Path -LiteralPath (Join-Path $custom 'jumpdb.exe')) '应安装到自定义目录' $result
		Assert ((Test-UserPathCount $custom) -eq 1) '自定义目录应写入 PATH' $result
		Assert (((Get-UserPathState).Raw -split ';') -ccontains $custom) 'PATH 条目不应带末尾反斜杠' $result

		# 用户 PATH 已有 %VAR% 形式的同一目录：不重复写入，原值与类型保持不变。
		$local = New-LocalAppData
		$rawPath = '%LOCALAPPDATA%\Programs\jumpdb\;C:\jumpdb-smoke-keep'
		Set-UserPathState @{ Exists = $true; Raw = $rawPath; Kind = [Microsoft.Win32.RegistryValueKind]::ExpandString }
		$result = Invoke-Installer $shell @{ LOCALAPPDATA = $local; JUMPDB_VERSION = $null; JUMPDB_INSTALL_DIR = $null } '' ''
		$state = Get-UserPathState
		Assert ($result.Code -eq 0) '已有 %VAR% 条目时应安装成功' $result
		Assert (-not $result.Out.Contains('加入当前用户的 PATH')) '%VAR% 条目应视为已在 PATH 中' $result
		Assert ($state.Raw -ceq $rawPath -and $state.Kind -eq [Microsoft.Win32.RegistryValueKind]::ExpandString) '不应改写已有 PATH 及其类型' $result

		# PATH 中更靠前的其他 jumpdb.exe 应给出警告。
		Set-UserPathState $originalPath
		$local = New-LocalAppData
		$other = Join-Path $local 'other'
		New-Item -ItemType Directory -Path $other | Out-Null
		Copy-Item -LiteralPath $oldExe -Destination (Join-Path $other 'jumpdb.exe')
		$prelude = "`$env:Path = '$($other.Replace("'", "''"));' + `$env:Path"
		$result = Invoke-Installer $shell @{ LOCALAPPDATA = $local; JUMPDB_VERSION = $null; JUMPDB_INSTALL_DIR = $null } $prelude ''
		Assert ($result.Code -eq 0) '存在冲突时仍应安装成功' $result
		Assert-Contains $result.Out "警告：当前 PATH 中优先生效的 jumpdb 是 $(Join-Path $other 'jumpdb.exe')" '冲突警告' $result

		# 旧版本正在运行时仍可替换；旧文件在进程退出后的下一次安装中清理。
		Set-UserPathState $originalPath
		$local = New-LocalAppData
		$dir = Join-Path $local 'Programs\jumpdb'
		$target = Join-Path $dir 'jumpdb.exe'
		Install-Good $shell $local
		if (-not (Test-Path -LiteralPath $target)) {
			Assert $false '前置安装未产生 jumpdb.exe，跳过运行中替换用例' $null
			continue
		}
		$uiInfo = New-Object Diagnostics.ProcessStartInfo
		$uiInfo.FileName = $target
		$uiInfo.Arguments = 'ui --no-open'
		$uiInfo.UseShellExecute = $false
		$uiInfo.RedirectStandardOutput = $true
		$uiInfo.RedirectStandardError = $true
		# 配置目录指向临时目录，不读写 runner 用户的 jumpdb 配置。
		$uiInfo.EnvironmentVariables['XDG_CONFIG_HOME'] = (Join-Path $local 'config')
		$uiProcess = [Diagnostics.Process]::Start($uiInfo)
		Start-Sleep -Seconds 2
		Assert (-not $uiProcess.HasExited) '前置条件：jumpdb ui 应保持运行' $null
		$result = Invoke-Installer $shell @{ LOCALAPPDATA = $local; JUMPDB_VERSION = $null; JUMPDB_INSTALL_DIR = $null } '' ''
		Assert ($result.Code -eq 0) '旧版本运行中时应替换成功' $result
		Assert-Contains $result.Out '旧版本仍在运行' '应提示旧文件暂未删除' $result
		Assert ((Get-Sha $target) -eq $binaryHash) '运行中替换后应为新文件' $result
		$uiProcess.Kill()
		$uiProcess.WaitForExit()
		$uiProcess = $null
		$result = Invoke-Installer $shell @{ LOCALAPPDATA = $local; JUMPDB_VERSION = $null; JUMPDB_INSTALL_DIR = $null } '' ''
		Assert ($result.Code -eq 0) '旧进程退出后再次安装应成功' $result
		Assert (@(Get-ChildItem -LiteralPath $dir -Filter '.jumpdb.old.*' -Force).Count -eq 0) '应清理遗留的旧版本文件' $result

		# 失败用例：原有文件、用户 PATH 均不变，不留临时文件，会话不被关闭。
		$cases = @(
			@{ Name = '校验和不一致'; Release = $mismatchRelease; Prior = $true; Error = "$asset 的 SHA-256 校验不一致，已停止安装" },
			@{ Name = 'SHA256SUMS 缺少条目'; Release = $missingRelease; Prior = $false; Error = "SHA256SUMS 中缺少 $asset" },
			@{ Name = '新文件无法运行'; Release = $brokenRelease; Prior = $true; Error = '新下载的 jumpdb 无法在本机运行，已保留原有文件' },
			@{ Name = '下载失败'; Release = $emptyRelease; Prior = $false; Error = "下载 $asset 失败" }
		)
		foreach ($case in $cases) {
			Set-UserPathState $originalPath
			$local = New-LocalAppData
			$dir = Join-Path $local 'Programs\jumpdb'
			$beforeHash = $null
			if ($case.Prior) {
				Install-Good $shell $local
				$beforeHash = Get-Sha (Join-Path $dir 'jumpdb.exe')
			}
			$pathBefore = (Get-UserPathState).Raw
			$script:releaseDir = $case.Release
			$result = Invoke-Installer $shell @{ LOCALAPPDATA = $local; JUMPDB_VERSION = $null; JUMPDB_INSTALL_DIR = $null } '' ''
			Assert-FailedCleanly $result $dir $beforeHash $case.Name $case.Error
			Assert ((Get-UserPathState).Raw -ceq $pathBefore) "$($case.Name)：不应修改用户 PATH" $result
		}

		# 参数与环境错误：不发起任何下载。
		$script:releaseDir = $script:goodRelease
		$cases = @(
			@{ Name = '版本缺少 v 前缀'; Envs = @{ JUMPDB_VERSION = '0.1.0'; JUMPDB_INSTALL_DIR = $null }; Prelude = ''; Error = 'JUMPDB_VERSION 应为 v 开头的标签' },
			@{ Name = '版本含非法字符'; Envs = @{ JUMPDB_VERSION = 'v1;x'; JUMPDB_INSTALL_DIR = $null }; Prelude = ''; Error = 'JUMPDB_VERSION 含有不允许的字符' },
			@{ Name = '相对安装目录'; Envs = @{ JUMPDB_VERSION = $null; JUMPDB_INSTALL_DIR = 'relative\dir' }; Prelude = ''; Error = 'JUMPDB_INSTALL_DIR 必须是绝对路径' },
			@{ Name = '不支持的架构'; Envs = @{ JUMPDB_VERSION = $null; JUMPDB_INSTALL_DIR = $null }; Prelude = 'function Get-CimInstance { [pscustomobject]@{ Architecture = 5 } }'; Error = '不支持的处理器架构（5）' }
		)
		foreach ($case in $cases) {
			Set-UserPathState $originalPath
			$local = New-LocalAppData
			$envs = $case.Envs.Clone()
			$envs['LOCALAPPDATA'] = $local
			$result = Invoke-Installer $shell $envs $case.Prelude ''
			Assert-FailedCleanly $result (Join-Path $local 'Programs\jumpdb') $null $case.Name $case.Error
			Assert ($result.Uris.Count -eq 0) "$($case.Name)：不应发起下载" $result
		}
	}
} catch {
	# 记录意外异常的位置，避免只看到一行没有行号的错误。
	$failures.Add("[$script:currentShell] 意外异常：$($_.Exception.Message)`n$($_.ScriptStackTrace)")
	[Console]::Error.WriteLine("FAIL " + $failures[$failures.Count - 1])
} finally {
	if ($uiProcess -and -not $uiProcess.HasExited) { $uiProcess.Kill(); $uiProcess.WaitForExit() }
	Set-UserPathState $originalPath
	$script:server.Listener.Stop()
	$script:server.Runner.Dispose()
	Remove-Item -LiteralPath $script:root -Recurse -Force -ErrorAction SilentlyContinue
}

if ($failures.Count -gt 0) {
	[Console]::Error.WriteLine("install.ps1 Windows 合成 smoke 失败：共 $($failures.Count) 项，详见上方 FAIL 行。")
	exit 1
}
Write-Host "install.ps1 Windows 合成 smoke 通过（$($Shells -join '、')）：安装命令 UTF-8 解码、首次安装与升级、旧版覆盖、指定版本与目录、用户 PATH 写入与去重、冲突警告、运行中替换、校验失败与无法运行时保留原文件、参数与架构错误。"
