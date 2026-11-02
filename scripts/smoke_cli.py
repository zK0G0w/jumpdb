"""仅用临时 HOME、合成凭据和回环服务验证构建后的 macOS CLI。"""

import hashlib
import json
import os
from pathlib import Path
import pty
import select
import signal
import socket
import subprocess
import sys
import tempfile
import termios
import threading
import time
import urllib.error
import urllib.parse
import urllib.request


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def config_path(home):
    return home / ".config" / "jumpdb" / "config.json"


def environment(home):
    result = os.environ.copy()
    result["HOME"] = str(home)
    result.pop("XDG_CONFIG_HOME", None)
    return result


def run(binary, home, args, expected):
    result = subprocess.run(
        [binary, *args], env=environment(home), input=b"fixture-piped-input\n",
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10,
    )
    require(result.returncode == expected, "实际命令退出码不符合预期")
    return result.stdout + result.stderr


def interactive(binary, home, args, fields, cancel=False, external_signal=False):
    master, slave = pty.openpty()
    original = termios.tcgetattr(slave)
    process = subprocess.Popen(
        [binary, *args], env=environment(home), stdin=slave, stdout=slave, stderr=slave,
    )
    captured = bytearray()

    def read_until(marker):
        deadline = time.monotonic() + 10
        while marker.encode() not in captured:
            require(time.monotonic() < deadline, "等待终端提示超时")
            readable, _, _ = select.select([master], [], [], 0.1)
            if readable:
                captured.extend(os.read(master, 4096))
            require(process.poll() is None or marker.encode() in captured, "提示前命令已退出")

    try:
        for marker, value in fields:
            read_until(marker)
            os.write(master, value.encode() + b"\n")
        if cancel:
            read_until("Token")
            if external_signal:
                process.send_signal(signal.SIGINT)
            else:
                os.write(master, b"fixture-unfinished\x03")
        require(process.wait(timeout=10) == 0, "交互命令未正常退出")
        while select.select([master], [], [], 0.1)[0]:
            captured.extend(os.read(master, 4096))
        restored = termios.tcgetattr(slave)
        # macOS 恢复规范模式时可置 PENDIN；它是驱动的待重处理状态，不是回显配置。
        original[3] &= ~termios.PENDIN
        restored[3] &= ~termios.PENDIN
        require(restored == original, "终端状态未恢复")
        for _, value in fields:
            require(value.encode() not in captured, "隐藏录入发生回显")
        require(b"fixture-unfinished" not in captured, "取消前输入发生回显")
    finally:
        if process.poll() is None:
            process.kill()
            process.wait()
        os.close(master)
        os.close(slave)


def local_listener():
    listener = socket.socket()
    listener.bind(("127.0.0.1", 0))
    listener.listen()
    return listener


def free_port():
    with local_listener() as listener:
        return listener.getsockname()[1]


def assert_released(port):
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", port))


def write_fixture(home, portal):
    path = config_path(home)
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    data = {"portal": portal, "trans_key": "t" * 16, "db_key": "d" * 16, "token": "fixture-token"}
    descriptor = os.open(path, os.O_CREAT | os.O_TRUNC | os.O_WRONLY, 0o600)
    with os.fdopen(descriptor, "w") as output:
        json.dump(data, output)


def check_network_exit(binary, home, cancel):
    with local_listener() as portal:
        write_fixture(home, "https://127.0.0.1:" + str(portal.getsockname()[1]))
        accepted = threading.Event()
        stop = threading.Event()
        portal.settimeout(10)

        def accept():
            with portal.accept()[0] as connection:
                accepted.set()
                if cancel:
                    stop.wait(10)

        worker = threading.Thread(target=accept, daemon=True)
        worker.start()
        port = free_port()
        process = subprocess.Popen(
            [binary, "connect", "--id", "856", "--port", str(port)],
            env=environment(home), stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )
        try:
            require(accepted.wait(10), "未连接本机合成门户")
            if cancel:
                process.send_signal(signal.SIGINT)
            output, error_output = process.communicate(timeout=10)
            require(process.returncode == (0 if cancel else 1), "网络阶段退出码不正确")
            expected = "已取消" if cancel else "门户时间探测失败"
            require(expected.encode() in output + error_output, "缺少取消或失败提示")
            require(b"fixture-token" not in output + error_output, "输出泄露合成凭据")
            assert_released(port)
        finally:
            stop.set()
            if process.poll() is None:
                process.kill()
                process.wait()
            worker.join(timeout=10)


def http_status(url, headers=None, host=None):
    """返回状态码与响应体；host 用于模拟 DNS 重绑定后的外部域名。"""
    request = urllib.request.Request(url, headers=headers or {})
    if host:
        request.add_header("Host", host)
    try:
        with urllib.request.urlopen(request, timeout=5) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as error:
        return error.code, error.read()


def check_ui(binary, home):
    process = subprocess.Popen(
        [binary, "ui", "--no-open"], env=environment(home), stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE,
    )
    try:
        address = None
        deadline = time.monotonic() + 10
        while address is None:
            require(time.monotonic() < deadline, "等待控制页地址超时")
            require(process.poll() is None, "控制页提前退出")
            if select.select([process.stdout], [], [], 0.1)[0]:
                line = process.stdout.readline().decode()
                if line.startswith("控制页："):
                    address = line.removeprefix("控制页：").strip()
        parsed = urllib.parse.urlsplit(address)
        token = urllib.parse.parse_qs(parsed.query)["token"][0]
        base = f"http://{parsed.netloc}"
        require(parsed.hostname == "127.0.0.1", "控制页未限定 127.0.0.1")
        require(http_status(address)[0] == 200, "带令牌的控制页不可访问")
        require(http_status(base + "/")[0] == 403, "缺少令牌未被拒绝")
        status, body = http_status(base + "/api/state", {"X-Jumpdb-Token": token})
        require(status == 200 and json.loads(body)["config"]["configured"], "状态接口不可用")
        require(b"fixture-token" not in body and b"tttt" not in body, "状态接口泄露凭据")
        require(http_status(base + "/api/state", {"X-Jumpdb-Token": token}, host=f"rebind.invalid:{parsed.port}")[0] == 403, "未拒绝外部 Host")
        process.send_signal(signal.SIGINT)
        output, error_output = process.communicate(timeout=10)
        require(process.returncode == 0, "控制页 SIGINT 退出码不正确")
        require(b"fixture-token" not in output + error_output, "控制页输出泄露凭据")
        # 服务端主动关闭过 HTTP 连接，端口会处于 TIME_WAIT，不能用 bind 判断；改为确认已无监听。
        with socket.socket() as probe:
            require(probe.connect_ex(("127.0.0.1", parsed.port)) != 0, "控制页退出后仍在监听")
    finally:
        if process.poll() is None:
            process.kill()
            process.wait()


INSTALL_SCRIPT = Path(__file__).resolve().parent / "install.sh"
RELEASES_PREFIX = "https://github.com/zK0G0w/jumpdb/releases/"

# 假 curl 只按发布地址从本地目录取文件，不访问网络；生产脚本因此无需可改下载来源的开关。
FAKE_CURL = """#!/bin/sh
output=
url=
while [ $# -gt 0 ]; do
  case "$1" in
    -o) output=$2; shift 2 ;;
    --proto) shift 2 ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
echo "$url" >> "$FAKE_CURL_LOG"
case "$url" in
  {prefix}latest/download/*) source="$FAKE_RELEASES/latest/${{url##*/}}" ;;
  {prefix}download/*) rest=${{url#{prefix}download/}}; source="$FAKE_RELEASES/${{rest%%/*}}/${{url##*/}}" ;;
  *) exit 22 ;;
esac
[ -f "$source" ] || exit 22
cp "$source" "$output"
""".replace("{prefix}", RELEASES_PREFIX).replace("{{", "{").replace("}}", "}")

FAKE_UNAME = """#!/bin/sh
case "$1" in
  -s) echo Darwin ;;
  -m) echo sparc64 ;;
esac
"""


def write_executable(path, content):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content)
    path.chmod(0o755)


def host_arch():
    machine = subprocess.run(["uname", "-m"], stdout=subprocess.PIPE, check=True).stdout.decode().strip()
    return {"arm64": "arm64", "x86_64": "amd64"}[machine]


def make_release(directory, binary, corrupt=False):
    """在本地目录生成与 CI 同结构的发布资产；corrupt 时写入错误校验和。"""
    directory.mkdir(parents=True)
    staging = directory / "staging"
    staging.mkdir()
    (staging / "jumpdb").write_bytes(Path(binary).read_bytes())
    (staging / "jumpdb").chmod(0o755)
    (staging / "README.md").write_text("fixture\n")
    asset = f"jumpdb-darwin-{host_arch()}.tar.gz"
    subprocess.run(["tar", "-czf", str(directory / asset), "-C", str(staging), "jumpdb", "README.md"],
                   check=True, env={**os.environ, "COPYFILE_DISABLE": "1"})
    digest = hashlib.sha256((directory / asset).read_bytes()).hexdigest()
    if corrupt:
        digest = "0" * 64
    (directory / "SHA256SUMS").write_text(f"{digest}  {asset}\n")


def run_install(home, fake_bin, path_entries, extra_env=None):
    env = {
        "HOME": str(home), "PATH": ":".join([str(fake_bin), *map(str, path_entries), "/usr/bin", "/bin", "/usr/sbin"]),
        "FAKE_RELEASES": str(home / "releases"), "FAKE_CURL_LOG": str(home / "curl.log"),
        **(extra_env or {}),
    }
    return subprocess.run(["/bin/sh", str(INSTALL_SCRIPT)], env=env, stdin=subprocess.DEVNULL,
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30)


def check_install(binary):
    syntax = subprocess.run(["/bin/sh", "-n", str(INSTALL_SCRIPT)], stderr=subprocess.PIPE)
    require(syntax.returncode == 0, "install.sh 语法检查失败")
    binary_bytes = Path(binary).read_bytes()
    expected_version = subprocess.run([binary, "version"], stdout=subprocess.PIPE, check=True).stdout.decode().split()[1]
    with tempfile.TemporaryDirectory(prefix="jumpdb-install-smoke-") as directory:
        home = Path(directory)
        fake_bin = home / "fakebin"
        write_executable(fake_bin / "curl", FAKE_CURL)
        make_release(home / "releases" / "latest", binary)
        make_release(home / "releases" / "v0.0.0-smoke", binary)
        make_release(home / "releases" / "v0.0.0-corrupt", binary, corrupt=True)
        default_dir = home / ".local" / "bin"
        target = default_dir / "jumpdb"

        # 首次安装到默认目录；目录不在 PATH 时只提示，不改 shell 配置。
        result = run_install(home, fake_bin, [])
        output = (result.stdout + result.stderr).decode()
        require(result.returncode == 0, "首次安装失败：" + output)
        require(target.read_bytes() == binary_bytes and target.stat().st_mode & 0o777 == 0o755, "安装的二进制或权限不正确")
        require(f"已安装 {target}：{expected_version}" in output, "首次安装未报告版本")
        require("不在 PATH 中" in output and f'export PATH="{default_dir}:$PATH"' in output, "缺少 PATH 提示")
        require(not (home / ".zshrc").exists() and not (home / ".config").exists(), "安装脚本改动了 shell 或 jumpdb 配置")
        require((home / "curl.log").read_text().splitlines()[0].startswith(RELEASES_PREFIX + "latest/download/"), "默认未从最新稳定版下载")

        # 覆盖不支持 version 命令的旧二进制，并指定版本下载。
        write_executable(target, "#!/bin/sh\nexit 2\n")
        (home / "curl.log").unlink()
        result = run_install(home, fake_bin, [default_dir], {"JUMPDB_VERSION": "v0.0.0-smoke"})
        output = (result.stdout + result.stderr).decode()
        require(result.returncode == 0, "升级安装失败：" + output)
        require(target.read_bytes() == binary_bytes, "升级未替换二进制")
        require(f"旧版本（不支持 version 命令） → {expected_version}" in output, "升级未报告新旧版本")
        require("不在 PATH 中" not in output and "警告" not in output, "目录已在 PATH 时不应提示")
        require(all(line.startswith(RELEASES_PREFIX + "download/v0.0.0-smoke/")
                    for line in (home / "curl.log").read_text().splitlines()), "未使用指定版本下载")

        # PATH 中更靠前的同名二进制需要警告。
        shadow = home / "shadow"
        write_executable(shadow / "jumpdb", "#!/bin/sh\nexit 0\n")
        result = run_install(home, fake_bin, [shadow, default_dir])
        require(result.returncode == 0 and f"优先生效的 jumpdb 是 {shadow / 'jumpdb'}" in result.stdout.decode(), "缺少 PATH 冲突警告")

        # 校验和不一致时拒绝安装，保留原二进制且不留临时文件；自定义目录同样适用。
        custom = home / "custom"
        result = run_install(home, fake_bin, [], {"JUMPDB_VERSION": "v0.0.0-corrupt", "JUMPDB_INSTALL_DIR": str(custom) + "/"})
        require(result.returncode == 1 and "SHA-256 校验不一致" in result.stderr.decode(), "校验和不一致未被拒绝")
        require(not (custom / "jumpdb").exists(), "校验失败仍安装了二进制")
        result = run_install(home, fake_bin, [], {"JUMPDB_VERSION": "v0.0.0-corrupt"})
        require(result.returncode == 1 and target.read_bytes() == binary_bytes, "校验失败改变了已安装的二进制")
        require(sorted(path.name for path in default_dir.iterdir()) == ["jumpdb"], "安装目录残留临时文件")

        result = run_install(home, fake_bin, [], {"JUMPDB_VERSION": "v0.0.0-smoke", "JUMPDB_INSTALL_DIR": str(custom) + "/"})
        require(result.returncode == 0 and (custom / "jumpdb").read_bytes() == binary_bytes, "自定义安装目录失败")

        # 参数校验与不支持的架构在下载前即停止。
        for extra, message in [
            ({"JUMPDB_VERSION": "0.1.0"}, "v 开头"),
            ({"JUMPDB_VERSION": "v0.1.0/../x"}, "不允许的字符"),
            ({"JUMPDB_VERSION": "v9.9.9"}, "下载 jumpdb-darwin-"),
            ({"JUMPDB_INSTALL_DIR": "relative/bin"}, "绝对路径"),
        ]:
            result = run_install(home, fake_bin, [], extra)
            require(result.returncode == 1 and message in result.stderr.decode(), f"未拒绝 {extra}")
        uname_bin = home / "unamebin"
        write_executable(uname_bin / "uname", FAKE_UNAME)
        result = run_install(home, fake_bin, [], {"PATH": f"{uname_bin}:{fake_bin}:/usr/bin:/bin:/usr/sbin"})
        require(result.returncode == 1 and "不支持的架构：sparc64" in result.stderr.decode(), "未拒绝不支持的架构")


def main():
    require(len(sys.argv) == 2, "用法：python3 scripts/smoke_cli.py /path/to/jumpdb")
    binary = str(Path(sys.argv[1]).resolve())
    with tempfile.TemporaryDirectory(prefix="jumpdb-cli-smoke-") as directory:
        home = Path(directory)
        run(binary, home, ["--help"], 0)
        run(binary, home, ["connect"], 2)
        run(binary, home, ["connect", "--id", "856", "--port", "invalid"], 2)
        run(binary, home, ["token", "set", "--token=fixture-token"], 2)
        run(binary, home, ["token", "set"], 1)
        run(binary, home, ["connect", "--id", "856"], 1)
        run(binary, home, ["ui", "fixture-extra"], 2)
        fields = [
            ("门户 HTTPS", "https://portal.invalid"),
            ("传输密钥", "t" * 16),
            ("数据库密钥", "d" * 16),
            ("Token", "fixture-token"),
        ]
        interactive(binary, home, ["token", "set"], fields)
        path = config_path(home)
        require(path.stat().st_mode & 0o777 == 0o600, "首次创建权限错误")
        original = json.loads(path.read_text())
        interactive(binary, home, ["token", "set"], [("Token", "fixture-updated")])
        updated = json.loads(path.read_text())
        require(updated["token"] == "fixture-updated", "Token 更新未生效")
        require(all(updated[key] == original[key] for key in ["portal", "trans_key", "db_key"]), "更新 Token 改变了其他字段")
        require(path.stat().st_mode & 0o777 == 0o600, "更新后权限错误")
        saved = path.read_bytes()
        for external_signal in [False, True]:
            interactive(binary, home, ["token", "set"], [], cancel=True, external_signal=external_signal)
            require(path.read_bytes() == saved, "取消录入改变了原配置")
        interactive(binary, home, ["token", "set", "--configure"], fields)
        run(binary, home, ["connect", "--id", "856", "--show-password"], 1)
        with local_listener() as occupied:
            output = run(binary, home, ["connect", "--id", "856", "--port", str(occupied.getsockname()[1])], 1)
            require("端口是否已被占用".encode() in output, "端口占用提示缺失")
        check_network_exit(binary, home, cancel=False)
        check_network_exit(binary, home, cancel=True)
        check_ui(binary, home)
    check_install(binary)
    print("CLI 实机合成 smoke 通过：隐藏录入/更新/取消恢复、参数与端口错误、门户探测失败、SIGINT、端口重绑、控制页令牌/Host 校验与退出，以及 install.sh 安装/升级/校验失败/PATH 提示。")


if __name__ == "__main__":
    main()
