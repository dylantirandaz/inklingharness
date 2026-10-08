set -eu

fail() {
    printf 'Native benchmark preflight failed: %s\n' "$1" >&2
    exit 1
}

test "$#" -eq 2 || fail 'expected CPU cores and memory MiB'
expected_cpus=$1
expected_memory_mib=$2
case "$expected_cpus:$expected_memory_mib" in
    *[!0-9:]*|:*|*:) fail 'resource values must be positive integers' ;;
esac
test "$expected_cpus" -gt 0 || fail 'CPU cores must be positive'
test "$expected_memory_mib" -gt 0 || fail 'memory MiB must be positive'
test "$(uname -s)" = Linux || fail 'expected Linux'
test "$(uname -m)" = x86_64 || fail 'expected native x86_64'
kernel=$(uname -r)
printf 'backend=modal\nruntime=vm\nplatform=Linux x86_64\nsandbox_kernel=%s\nexpected_cpus=%s\nexpected_memory_mib=%s\n' "$kernel" "$expected_cpus" "$expected_memory_mib"
case "$kernel" in
    *gvisor*) fail 'expected a VM with its own Linux kernel' ;;
esac

actual_cpus=$(getconf _NPROCESSORS_ONLN)
test "$actual_cpus" -eq "$expected_cpus" || fail 'VM processor count differs from the task'
read -r block_hex < /sys/devices/system/memory/block_size_bytes
case "$block_hex" in
    ''|*[!0-9a-fA-F]*) fail 'invalid VM memory block size' ;;
esac
block_bytes=$((16#$block_hex))
test "$block_bytes" -gt 0 || fail 'VM memory block size must be positive'
memory_bytes=0
for state_file in /sys/devices/system/memory/memory*/state; do
    test -r "$state_file" || fail 'VM memory blocks are not exposed'
    read -r state < "$state_file"
    test "$state" = online || fail 'VM has an offline memory block'
    memory_bytes=$((memory_bytes + block_bytes))
done
test "$memory_bytes" -eq "$((expected_memory_mib * 1024 * 1024))" || fail 'VM physical memory differs from the task'

printf 'guest_logical_cpus=%s\nphysical_memory_bytes=%s\n' "$actual_cpus" "$memory_bytes"
printf 'Visible processor data:\n'
while IFS= read -r line; do
    case "$line" in
        'vendor_id'*|'model name'*|'cpu family'*|'model'*|'cpu MHz'*) printf '%s\n' "$line" ;;
    esac
    test -n "$line" || break
done < /proc/cpuinfo
