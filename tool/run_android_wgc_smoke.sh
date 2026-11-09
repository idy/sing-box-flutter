#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
sdk="${ANDROID_HOME:-${ANDROID_SDK_ROOT:-$root/.build/android-sdk}}"
adb="$sdk/platform-tools/adb"
emulator="$sdk/emulator/emulator"
avdmanager="$sdk/cmdline-tools/latest/bin/avdmanager"
cc="$sdk/ndk/28.0.13004108/toolchains/llvm/prebuilt/linux-x86_64/bin/x86_64-linux-android24-clang"
for executable in "$adb" "$emulator" "$avdmanager" "$cc"; do
  if [[ ! -x "$executable" ]]; then
    printf 'Android smoke requires installed SDK/NDK tool: %s\n' "$executable" >&2
    exit 1
  fi
done
command -v timeout >/dev/null
if [[ ! -r /dev/kvm || ! -w /dev/kvm ]]; then
  printf 'Android smoke requires read/write access to /dev/kvm.\n' >&2
  exit 1
fi

logs="$root/.build/logs"
binary="$root/.build/android-wgc-smoke/wgc.test"
mkdir -p "$logs" "$(dirname "$binary")"
unset TS_PERMIT_TOOLCHAIN_MISMATCH
# Reuse the preceding native build's verified fork and its exact cache policy.
python3 -B - "$root" "$binary" "$cc" <<'PY'
import json
from pathlib import Path
import subprocess
import sys

root, binary, cc = map(Path, sys.argv[1:])
sys.path.insert(0, str(root / "tool"))
from build_libbox import go_environment, host_key
from native_common import sha256

host = host_key()
lock = json.loads((root / "tool/tailscale-go.json").read_text())
bootstrap = root / ".build/bootstrap"
goroot = bootstrap / f"tailscale-go-{host}"
go = goroot / "bin/go"
provenance = json.loads((bootstrap / "toolchain-provenance.json").read_text())
expected = {"host": host, "revision": lock["revision"], "version": lock["version"],
            "sha256": lock["sha256"][host]}
if (any(provenance.get(key) != value for key, value in expected.items())
        or not go.is_file() or sha256(go) != provenance.get("binary_sha256")):
    raise SystemExit("Run the pinned native build first: Tailscale Go provenance differs")
env = go_environment(goroot, root / ".build/cache")
env.update(GOOS="android", GOARCH="amd64", CGO_ENABLED="1", CC=str(cc))
print(f"Compile Android/amd64 smoke with {go} and {cc}", flush=True)
subprocess.run([str(go), "test", "-c", "-tags=with_gvisor", "-o", str(binary), "."],
               cwd=root / "native/wgc", env=env, check=True, timeout=600)
PY

export ANDROID_HOME="$sdk" ANDROID_SDK_ROOT="$sdk"
export ANDROID_USER_HOME="$root/.build/android-user"
export ANDROID_AVD_HOME="$ANDROID_USER_HOME/avd"
mkdir -p "$ANDROID_AVD_HOME"
avd=wgc-smoke
serial="emulator-5554"
remote=/data/local/tmp/wgc-protection-smoke.test
emulator_pid=
adb_smoke() { timeout -k 5s 15s "$adb" -s "$serial" "$@"; }
cleanup() {
  status=$?
  trap - EXIT
  set +e
  if [[ -n "$emulator_pid" ]]; then
    adb_smoke logcat -d > "$logs/android-wgc-logcat.log" 2>&1
    adb_smoke shell rm -f "$remote"
    adb_smoke emu kill >> "$logs/android-wgc-emulator.log" 2>&1
    kill "$emulator_pid" 2>/dev/null
    shutdown_deadline=$((SECONDS + 10))
    while kill -0 "$emulator_pid" 2>/dev/null && (( SECONDS < shutdown_deadline )); do
      sleep 1
    done
    kill -KILL "$emulator_pid" 2>/dev/null
    wait "$emulator_pid" 2>/dev/null
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

printf 'no\n' | timeout -k 5s 60s "$avdmanager" create avd --force --name "$avd" \
  --package 'system-images;android-35;default;x86_64' > "$logs/android-wgc-avd.log" 2>&1
"$emulator" -avd "$avd" -port 5554 -accel on -no-window -no-audio \
  -no-boot-anim -no-snapshot -wipe-data -gpu swiftshader_indirect \
  -camera-back none -camera-front none > "$logs/android-wgc-emulator.log" 2>&1 &
emulator_pid=$!

printf 'Wait up to 180 seconds for API 35 x86_64 emulator boot.\n'
boot_deadline=$((SECONDS + 180))
until [[ "$(adb_smoke shell getprop sys.boot_completed 2>/dev/null | tr -d '\r')" == 1 ]]; do
  if ! kill -0 "$emulator_pid" 2>/dev/null; then
    printf 'Android emulator exited before boot; inspect android-wgc-emulator.log.\n' >&2
    exit 1
  fi
  if (( SECONDS >= boot_deadline )); then
    printf 'Android emulator boot timed out; inspect android-wgc-emulator.log.\n' >&2
    exit 1
  fi
  sleep 2
done
adb_smoke shell getprop ro.product.cpu.abi
adb_smoke push "$binary" "$remote"
adb_smoke shell chmod 755 "$remote"
timeout -k 5s 90s "$adb" -s "$serial" shell \
  "cd /data/local/tmp && $remote -test.run '^TestAndroidUDPProtectionSmoke$' -test.v -test.timeout=60s" \
  2>&1 | tee "$logs/android-wgc-runtime.log"
# Go exits successfully even when -test.run matches nothing or the test skips.
if ! grep -q '^--- PASS: TestAndroidUDPProtectionSmoke ' "$logs/android-wgc-runtime.log"; then
  printf 'Android UDP protection smoke did not report PASS.\n' >&2
  exit 1
fi
