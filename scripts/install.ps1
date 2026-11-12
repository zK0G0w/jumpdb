# jumpdb Windows 安装脚本：下载本机架构的发布包，按 SHA256SUMS 校验后安装到用户目录，并把安装目录加入当前用户的 PATH。
#
# 用法（PowerShell）：
#   iex ([Text.Encoding]::UTF8.GetString((iwr -useb https://github.com/zK0G0w/jumpdb/releases/latest/download/install.ps1).Content))
#
# 环境变量：
#   JUMPDB_VERSION      要安装的标签，例如 v0.1.0；默认最新稳定版
#   JUMPDB_INSTALL_DIR  安装目录，必须是绝对路径；默认 %LOCALAPPDATA%\Programs\jumpdb
#
# 不需要管理员权限，只修改当前用户的 PATH，不读取或改动 jumpdb 配置，也不解除下载文件的安全标记（不执行 Unblock-File）。
# 校验和与压缩包来自同一 Release，只能发现下载损坏，不能证明来源。
#
# 兼容 Windows PowerShell 5.1 与 PowerShell 7。文件以无 BOM 的 UTF-8 保存。Release 资产的响应没有 charset，
# Windows PowerShell 5.1 的 irm 会按 ISO-8859-1 解码并忽略 BOM，中文乱码且脚本无法执行，
# 所以安装命令取原始字节自行按 UTF-8 解码，不能写成 irm | iex。
# 通过 iex 执行时与调用者共用会话，因此全部逻辑放在脚本块内，不留下函数或变量；失败时不调用 exit，避免关闭用户的终端窗口。

& {
	Set-StrictMode -Version 2.0
	$ErrorActionPreference = 'Stop'
	# Windows PowerShell 5.1 的下载进度条会让下载明显变慢。
	$ProgressPreference = 'SilentlyContinue'
	$repo = 'zK0G0w/jumpdb'
	$failPrefix = 'jumpdb 安装失败：'

	function Fail([string]$message) {
		throw ($failPrefix + $message)
	}

	function Get-Arch {
		if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) { Fail '仅支持 Windows' }
		if (-not [Environment]::Is64BitOperatingSystem) { Fail '仅支持 64 位 Windows' }
		# 以处理器架构为准：ARM 设备上的 x64 转译 PowerShell 也报告 AMD64 环境变量，但应安装原生 arm64 版本。
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
				default { Fail '无法识别处理器架构' }
			}
		}
		switch ([int]$architecture) {
			9 { return 'amd64' }
			12 { return 'arm64' }
			default { Fail "不支持的处理器架构（$architecture）" }
		}
	}

	# 运行原生命令时临时放宽错误策略：Windows PowerShell 5.1 在 Stop 下会把原生命令的 stderr 当作终止错误。
	function Invoke-Quiet([string]$path, [string[]]$arguments) {
		$ErrorActionPreference = 'Continue'
		try {
			$output = & $path @arguments 2>$null
			return @{ Ok = ($LASTEXITCODE -eq 0); Output = (@($output) -join "`n") }
		} catch {
			return @{ Ok = $false; Output = '' }
		}
	}

	# 输出二进制的版本；v0.0.1 等早于 version 命令的版本无法读取时输出 fallback。
	function Get-BinaryVersion([string]$path, [string]$fallback) {
		$result = Invoke-Quiet $path @('version')
		$fields = @($result.Output.Trim() -split '\s+')
		if ($result.Ok -and $fields.Count -ge 2 -and $fields[0] -eq 'jumpdb') { return $fields[1] }
		return $fallback
	}

	# 展开环境变量并去掉末尾反斜杠，用于比较 PATH 条目，避免 %VAR% 写法或尾斜杠造成误判。
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

	# 只修改 HKCU\Environment 的 Path，保留原值类型与未展开的 %VAR% 条目；
	# [Environment]::SetEnvironmentVariable 在 Windows PowerShell 5.1 中会把它改写成 REG_SZ，导致这些条目失效。
	# 返回 $true 表示新写入，$false 表示已存在。
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
		# 广播环境变量变更，让之后从资源管理器或 Windows Terminal 打开的终端读到新 PATH。
		# 借助 .NET 写入用户变量时自带的 WM_SETTINGCHANGE 广播，写入后立即删除这个临时变量，避免编译 P/Invoke 代码。
		try {
			[Environment]::SetEnvironmentVariable('JUMPDB_INSTALL_NOTIFY', '1', 'User')
			[Environment]::SetEnvironmentVariable('JUMPDB_INSTALL_NOTIFY', $null, 'User')
		} catch {
			Write-Host '提示：未能通知系统刷新环境变量，新终端可能需要注销并重新登录后才能找到 jumpdb。'
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
			if (-not $env:LOCALAPPDATA) { Fail '无法确定 %LOCALAPPDATA%，请用 JUMPDB_INSTALL_DIR 指定安装目录' }
			$installDir = Join-Path $env:LOCALAPPDATA 'Programs\jumpdb'
		}
		# 只接受盘符或 UNC 开头的绝对路径；相对路径或 \foo 会随当前目录变化。
		if ($installDir -notmatch '^([A-Za-z]:\\|\\\\[^\\]+\\[^\\]+)') { Fail 'JUMPDB_INSTALL_DIR 必须是绝对路径' }
		# 去掉末尾反斜杠（保留盘符根目录），便于与 PATH 条目比较。
		if ($installDir -notmatch '^[A-Za-z]:\\$') { $installDir = $installDir.TrimEnd('\') }
		# 版本会拼入下载地址，只接受 v 开头的标签字符。
		if ($version -eq 'latest') {
			$base = "https://github.com/$repo/releases/latest/download"
		} else {
			if ($version -cnotmatch '^v') { Fail 'JUMPDB_VERSION 应为 v 开头的标签，例如 v0.1.0' }
			if ($version -cnotmatch '^v[0-9A-Za-z.+-]+$') { Fail 'JUMPDB_VERSION 含有不允许的字符' }
			$base = "https://github.com/$repo/releases/download/$version"
		}
		$arch = Get-Arch
		$asset = "jumpdb-windows-$arch.zip"
		# Windows PowerShell 5.1 默认可能不启用 TLS 1.2；只追加协议，不关闭证书校验。
		[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

		$work = Join-Path ([IO.Path]::GetTempPath()) ('jumpdb-install-' + [guid]::NewGuid().ToString('N'))
		New-Item -ItemType Directory -Path $work | Out-Null

		Write-Host "下载 $asset（$version）……"
		$zip = Join-Path $work $asset
		$sums = Join-Path $work 'SHA256SUMS'
		try {
			Invoke-WebRequest -Uri "$base/$asset" -OutFile $zip -UseBasicParsing
		} catch {
			Fail "下载 $asset 失败，请确认版本存在且网络可用"
		}
		try {
			Invoke-WebRequest -Uri "$base/SHA256SUMS" -OutFile $sums -UseBasicParsing
		} catch {
			Fail '下载 SHA256SUMS 失败'
		}
		$expected = $null
		foreach ($line in [IO.File]::ReadAllLines($sums)) {
			if ($line -match '^([0-9A-Fa-f]{64}) [ *](.+)$' -and $Matches[2] -ceq $asset) { $expected = $Matches[1] }
		}
		if (-not $expected) { Fail "SHA256SUMS 中缺少 $asset" }
		$actual = (Get-FileHash -LiteralPath $zip -Algorithm SHA256).Hash
		if ($actual -ne $expected) { Fail "$asset 的 SHA-256 校验不一致，已停止安装" }

		$extract = Join-Path $work 'extract'
		try {
			Expand-Archive -LiteralPath $zip -DestinationPath $extract
		} catch {
			Fail "解压 $asset 失败"
		}
		$binary = Join-Path $extract 'jumpdb.exe'
		$item = Get-Item -LiteralPath $binary -Force -ErrorAction SilentlyContinue
		if ($null -eq $item -or $item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
			Fail '压缩包内容不符合预期'
		}

		try {
			New-Item -ItemType Directory -Path $installDir -Force | Out-Null
		} catch {
			Fail "无法创建安装目录 $installDir"
		}
		$target = Join-Path $installDir 'jumpdb.exe'
		# 清理以前因旧进程仍在运行而未能删除的旧版本文件。
		Get-ChildItem -LiteralPath $installDir -Filter '.jumpdb.old.*.exe' -Force -ErrorAction SilentlyContinue |
			Remove-Item -Force -ErrorAction SilentlyContinue
		$previous = $null
		if (Test-Path -LiteralPath $target) {
			$previous = Get-BinaryVersion $target '旧版本（不支持 version 命令）'
		}
		$staged = Join-Path $installDir ".jumpdb.install.$PID.exe"
		try {
			Copy-Item -LiteralPath $binary -Destination $staged -Force
		} catch {
			Fail "无法写入安装目录 $installDir"
		}
		# 替换前确认新文件能在本机运行；用 --help 而非 version，以便仍可安装不支持 version 的旧版本。
		if (-not (Invoke-Quiet $staged @('--help')).Ok) { Fail '新下载的 jumpdb 无法在本机运行，已保留原有文件' }
		$installed = Get-BinaryVersion $staged '版本未知（该版本不支持 version 命令）'

		# 运行中的 exe 不能被覆盖或删除，但可以改名：先把旧文件移开，再把新文件移入，失败时移回旧文件。
		$old = $null
		if (Test-Path -LiteralPath $target) {
			$old = Join-Path $installDir ".jumpdb.old.$PID.exe"
			try {
				Move-Item -LiteralPath $target -Destination $old -Force
			} catch {
				Fail "无法替换 $target，已保留原有文件"
			}
		}
		try {
			Move-Item -LiteralPath $staged -Destination $target
		} catch {
			if ($old) { Move-Item -LiteralPath $old -Destination $target -Force -ErrorAction SilentlyContinue }
			Fail "无法替换 $target，已保留原有文件"
		}
		$staged = $null
		$oldLeft = $false
		if ($old) {
			Remove-Item -LiteralPath $old -Force -ErrorAction SilentlyContinue
			$oldLeft = Test-Path -LiteralPath $old
		}

		if ($previous) {
			Write-Host "已升级 ${target}：$previous → $installed"
		} else {
			Write-Host "已安装 ${target}：$installed"
		}
		if ($oldLeft) {
			Write-Host "旧版本仍在运行，未能删除 $old；下次安装时会自动清理。"
		}

		if (Add-UserPath $installDir) {
			Write-Host ''
			Write-Host "已把 $installDir 加入当前用户的 PATH；此前已打开的其他终端需重新打开后才能找到 jumpdb。"
		}
		# 让当前会话立即可用；与新终端一致，用户 PATH 排在系统 PATH 之后。
		if (-not (Test-PathListContains $env:Path $installDir)) {
			$env:Path = $env:Path.TrimEnd(';') + ';' + $installDir
		}
		$resolved = @(Get-Command jumpdb -CommandType Application -ErrorAction SilentlyContinue)
		if ($resolved.Count -gt 0) {
			$resolvedDir = Split-Path -Parent $resolved[0].Path
			if ((Get-ComparablePath $resolvedDir) -ine (Get-ComparablePath $installDir)) {
				Write-Host ''
				Write-Host "警告：当前 PATH 中优先生效的 jumpdb 是 $($resolved[0].Path)，不是刚安装的 $target。"
				Write-Host '请删除或调整那个文件在 PATH 中的位置。'
			}
		}

		Write-Host ''
		Write-Host '下一步：运行 jumpdb --help 查看命令；首次使用先运行 jumpdb token set。'
		$global:LASTEXITCODE = 0
	} catch {
		$message = $_.Exception.Message
		if (-not $message.StartsWith($failPrefix)) { $message = $failPrefix + '发生意外错误（' + $_.Exception.GetType().Name + '）' }
		[Console]::Error.WriteLine($message)
		# 不调用 exit：通过 iex 执行时会直接关闭用户的终端。用退出码告知调用方。
		$global:LASTEXITCODE = 1
	} finally {
		# 任何失败都清理下载目录及安装目录中的临时文件，不留下半截二进制。
		if ($staged) { Remove-Item -LiteralPath $staged -Force -ErrorAction SilentlyContinue }
		if ($work) { Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue }
	}
}
