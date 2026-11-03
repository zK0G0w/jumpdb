#!/bin/sh
# jumpdb 安装脚本：下载本机架构的发布包，按 SHA256SUMS 校验后安装到用户目录。
#
# 用法：
#   curl -fsSL https://github.com/zK0G0w/jumpdb/releases/latest/download/install.sh | sh
#
# 环境变量：
#   JUMPDB_VERSION      要安装的标签，例如 v0.1.0；默认最新稳定版
#   JUMPDB_INSTALL_DIR  安装目录，必须是绝对路径；默认 ~/.local/bin
#
# 不使用 sudo，不修改 shell 配置文件，不读取或改动 jumpdb 配置，也不移除 macOS 隔离属性。
# 校验和与压缩包来自同一 Release，只能发现下载损坏，不能证明来源。

set -eu

REPO="zK0G0w/jumpdb"

fail() {
	printf 'jumpdb 安装失败：%s\n' "$1" >&2
	exit 1
}

detect_arch() {
	[ "$(uname -s)" = Darwin ] || fail "仅支持 macOS"
	case "$(uname -m)" in
	arm64) echo arm64 ;;
	x86_64)
		# 在 Rosetta 转译的终端中 uname 报告 x86_64，此时仍安装原生 arm64 版本。
		if [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || true)" = 1 ]; then
			echo arm64
		else
			echo amd64
		fi
		;;
	*) fail "不支持的架构：$(uname -m)" ;;
	esac
}

# 输出二进制的版本；v0.0.1 等早于 version 命令的版本无法读取时输出第二个参数的提示文字。
binary_version() {
	output="$("$1" version 2>/dev/null)" || {
		echo "$2"
		return
	}
	echo "$output" | awk '{print $2}'
}

# 输出目录解析符号链接后的物理路径，目录不存在时输出空；用于与 PATH 条目比较，避免尾斜杠或链接造成误判。
physical_dir() {
	(cd "$1" 2>/dev/null && pwd -P) || true
}

main() {
	version="${JUMPDB_VERSION:-latest}"
	install_dir="${JUMPDB_INSTALL_DIR:-$HOME/.local/bin}"
	# 去掉末尾斜杠，便于与 PATH 中的条目比较。
	while [ "${install_dir%/}" != "$install_dir" ] && [ "$install_dir" != / ]; do
		install_dir="${install_dir%/}"
	done
	case "$install_dir" in
	/*) ;;
	*) fail "JUMPDB_INSTALL_DIR 必须是绝对路径" ;;
	esac
	# 版本会拼入下载地址，只接受 v 开头的标签字符。
	if [ "$version" = latest ]; then
		base="https://github.com/$REPO/releases/latest/download"
	else
		case "$version" in
		v*) ;;
		*) fail "JUMPDB_VERSION 应为 v 开头的标签，例如 v0.1.0" ;;
		esac
		case "$version" in
		*[!0-9A-Za-z.+-]*) fail "JUMPDB_VERSION 含有不允许的字符" ;;
		esac
		base="https://github.com/$REPO/releases/download/$version"
	fi
	command -v curl >/dev/null 2>&1 || fail "未找到 curl"
	arch="$(detect_arch)"
	asset="jumpdb-darwin-$arch.tar.gz"

	work="$(mktemp -d)"
	staged=""
	# 任何失败都清理下载目录及安装目录中的临时文件，不留下半截二进制。
	trap 'rm -rf "$work"; [ -z "$staged" ] || rm -f "$staged"' EXIT
	trap 'exit 1' HUP INT TERM

	echo "下载 $asset（$version）……"
	curl -fsSL --proto =https --tlsv1.2 -o "$work/$asset" "$base/$asset" || fail "下载 $asset 失败，请确认版本存在且网络可用"
	curl -fsSL --proto =https --tlsv1.2 -o "$work/SHA256SUMS" "$base/SHA256SUMS" || fail "下载 SHA256SUMS 失败"
	expected="$(awk -v name="$asset" '$2 == name || $2 == "*" name {print $1}' "$work/SHA256SUMS")"
	[ -n "$expected" ] || fail "SHA256SUMS 中缺少 $asset"
	actual="$(shasum -a 256 "$work/$asset" | awk '{print $1}')"
	[ "$expected" = "$actual" ] || fail "$asset 的 SHA-256 校验不一致，已停止安装"

	mkdir "$work/extract"
	tar -xzf "$work/$asset" -C "$work/extract" jumpdb || fail "解压 $asset 失败"
	[ -f "$work/extract/jumpdb" ] && [ ! -L "$work/extract/jumpdb" ] || fail "压缩包内容不符合预期"

	mkdir -p "$install_dir" || fail "无法创建安装目录 $install_dir"
	target="$install_dir/jumpdb"
	previous=""
	if [ -e "$target" ]; then
		previous="$(binary_version "$target" "旧版本（不支持 version 命令）")"
	fi
	# 先写入同目录临时文件再 rename，替换是原子的，正在运行的旧进程不受影响。
	staged="$install_dir/.jumpdb.install.$$"
	cp "$work/extract/jumpdb" "$staged" || fail "无法写入安装目录 $install_dir"
	chmod 755 "$staged"
	# 替换前确认新文件能在本机运行；用 --help 而非 version，以便仍可安装不支持 version 的旧版本。
	"$staged" --help >/dev/null 2>&1 || fail "新下载的 jumpdb 无法在本机运行，已保留原有文件"
	installed="$(binary_version "$staged" "版本未知（该版本不支持 version 命令）")"
	mv -f "$staged" "$target" || fail "无法替换 $target"
	staged=""

	if [ -n "$previous" ]; then
		echo "已升级 $target：$previous → $installed"
	else
		echo "已安装 $target：$installed"
	fi

	install_physical="$(physical_dir "$install_dir")"
	in_path=no
	old_ifs=$IFS
	IFS=:
	set -f
	for entry in $PATH; do
		if [ -n "$entry" ] && [ "$(physical_dir "$entry")" = "$install_physical" ]; then
			in_path=yes
			break
		fi
	done
	set +f
	IFS=$old_ifs
	if [ "$in_path" = no ]; then
		echo
		echo "注意：$install_dir 不在 PATH 中。可在 ~/.zshrc 中加入下面一行，然后重新打开终端："
		echo "  export PATH=\"$install_dir:\$PATH\""
	fi
	resolved="$(command -v jumpdb 2>/dev/null || true)"
	case "$resolved" in
	/*) resolved_physical="$(physical_dir "${resolved%/*}")" ;;
	*) resolved_physical="" ;;
	esac
	if [ -n "$resolved" ] && [ "$resolved_physical" != "$install_physical" ]; then
		echo
		echo "警告：当前 PATH 中优先生效的 jumpdb 是 $resolved，不是刚安装的 $target。"
		echo "请删除或调整那个文件在 PATH 中的位置。"
	fi

	echo
	echo "下一步：运行 jumpdb --help 查看命令；首次使用先运行 jumpdb token set。"
}

# 整个脚本下载完成后才执行 main，避免 curl | sh 在传输中断时执行半截脚本。
main "$@"
