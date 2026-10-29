#!/usr/bin/env bash
# The M6 VM bench (decision 0073 block 4): two libvirt VMs on this machine,
# bs-m6-server and bs-m6-client, reached only through the bench's own ssh
# config, login key and known_hosts, and removed by down alone.
#
#   scripts/vm-bench.sh up              build, boot, provision, snapshot bs-m6-clean
#   scripts/vm-bench.sh status          the domains, their states, IPs, snapshot
#   scripts/vm-bench.sh ssh <vm> [cmd]  server, client (user ubuntu), server-bs, client-bs (user bs)
#   scripts/vm-bench.sh reset           both VMs back to bs-m6-clean
#   scripts/vm-bench.sh down [--purge]  every bs-m6-* domain, snapshot and volume, and VB_DIR
#                                       but results/ and cache/ (--purge: cache/ too)
#   scripts/vm-bench.sh run [check…]    reset, the install chain and two-clients, then the
#                                       checks named (default: all); results/<UTC stamp>.md
#
# Run by hand only, when the user asks: never on CI, from a timer or a hook
# (it refuses when CI or GITHUB_ACTIONS is set). Every libvirt resource it
# makes is named bs-m6-*, and down removes only the names vb_owned accepts.
# It never reads or writes the user's ~/.ssh or ~/.claude: every ssh is
# `ssh -F $VB_DIR/ssh_config`, which offers only the bench's key (no agent)
# and trusts only the host keys the bench generated for its VMs (no first
# use). Tests source this file: vb_main runs only when it is executed.

VB_URI=qemu:///system
VB_NET=default
VB_POOL=default
# Pinned, not current/: a rerun boots the same image.
VB_IMAGE_URL=https://cloud-images.ubuntu.com/releases/noble/release-20261017/ubuntu-24.04-server-cloudimg-amd64.img
VB_IMAGE_SHA256=04e8f2461a16a44e78a370284471f29616cdd9baf1893df25807b71f985b6c94
VB_SERVER_MAC=52:54:00:b6:06:01
VB_CLIENT_MAC=52:54:00:b6:06:02
# A testkey build above any release, as update-chain's 0.900.x.
VB_VERSION=0.940.0
# CI's pins (.github/workflows/ci.yml); the line of nodejs.org's SHASUMS256.txt.
VB_NODE=24.22.1
VB_NODE_SHASUM='6c172a9ab6718f7b801fb5a7bd486af2ee5e32758086a04b84fcce2aa77095e6  node-v24.22.1-linux-x64.tar.xz'
VB_CLAUDE=2.1.310
VB_BASE=bs-m6-base-noble.qcow2
VB_DIR=${BS_M6_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/bs-m6}
VB_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

vb_say() { printf 'bs-m6: %s\n' "$*" >&2; }
vb_fail() { printf 'bs-m6: %s\n' "$*" >&2; exit 1; }
vb_refuse() { printf 'bs-m6: %s\n' "$*" >&2; exit 2; }

# vb_owned <name>: 0 iff the name is the bench's. The one gate of down.
vb_owned() { [[ $1 =~ ^bs-m6-[a-z0-9.-]+$ ]]; }

# Its stdin is /dev/null: no virsh call reads one, and a loop reading names
# from its own stdin must keep them.
vb_virsh() { LC_ALL=C virsh -c "$VB_URI" "$@" < /dev/null; }

# vb_vm <server|client|bs-m6-…>: the VM's short name.
vb_vm() {
  case ${1#bs-m6-} in
    server | client) echo "${1#bs-m6-}" ;;
    *) vb_fail "no VM $1 (server or client)" ;;
  esac
}

vb_mac() {
  case $(vb_vm "$1") in
    server) echo "$VB_SERVER_MAC" ;;
    client) echo "$VB_CLIENT_MAC" ;;
  esac
}

# vb_ip <vm> [client id…]: the VM's address, from the network's DHCP lease
# of its fixed MAC that expires last (a VM made again by up keeps its MAC,
# and its previous boots' leases stay listed until they expire), leases of
# the client ids given ignored; fails when there is none.
vb_ip() {
  local mac ip
  mac=$(vb_mac "$1")
  shift
  ip=$(vb_virsh net-dhcp-leases "$VB_NET" --mac "$mac" |
    awk -v stale=" $* " '$4 == "ipv4" && index(stale, " " $7 " ") == 0 && $1 " " $2 > newest { newest = $1 " " $2; ip = $5 }
      END { sub("/.*", "", ip); print ip }')
  [ -n "$ip" ] || return 1
  echo "$ip"
}

# vb_lease_ids <vm>: the DHCP client ids of the VM's MAC's current leases.
vb_lease_ids() {
  vb_virsh net-dhcp-leases "$VB_NET" --mac "$(vb_mac "$1")" | awk '$4 == "ipv4" { print $7 }'
}

# vb_domains, vb_volumes: every name of the connection's domains and of the
# pool's volumes, one a line (a name with a space would split in two, and
# neither half passes vb_owned); a failing virsh fails.
vb_domains() {
  local out
  out=$(vb_virsh list --all --name) || vb_fail "virsh list failed ($VB_URI)"
  printf '%s\n' "$out" | awk 'NF'
}
vb_volumes() {
  local out
  out=$(vb_virsh vol-list "$VB_POOL") || vb_fail "virsh vol-list $VB_POOL failed ($VB_URI)"
  printf '%s\n' "$out" | awk 'NR > 2 && NF { print $1 }'
}

# The bench directory must be named bs-m6 and not be a symlink: down
# empties it, so a BS_M6_DIR pointed elsewhere by mistake, or a bs-m6 link
# to another directory, must lose nothing.
vb_check_dir() {
  [ "${VB_DIR##*/}" = bs-m6 ] || vb_refuse "the bench directory must be named bs-m6, not $VB_DIR (BS_M6_DIR)"
  [ ! -L "$VB_DIR" ] || vb_refuse "the bench directory $VB_DIR is a symlink: it must be a directory of its own"
}

vb_mkdir() { (umask 077 && mkdir -p "$@"); }

# vb_keys: the bench's login key and each VM's ssh host key, made once.
vb_keys() {
  local vm
  vb_check_dir
  vb_mkdir "$VB_DIR" "$VB_DIR/hostkeys/server" "$VB_DIR/hostkeys/client"
  chmod 700 "$VB_DIR"
  [ -s "$VB_DIR/key" ] || ssh-keygen -q -t ed25519 -N '' -C bs-m6-bench -f "$VB_DIR/key"
  for vm in server client; do
    [ -s "$VB_DIR/hostkeys/$vm/ssh_host_ed25519_key" ] ||
      ssh-keygen -q -t ed25519 -N '' -C "bs-m6-$vm" -f "$VB_DIR/hostkeys/$vm/ssh_host_ed25519_key"
  done
}

# vb_write_ssh_config <server ip> <client ip>: the only ssh config the
# bench uses, and its known_hosts, written from the host keys the bench
# generated (nothing is trusted on first use).
vb_write_ssh_config() {
  local sip=$1 cip=$2 vm
  vb_keys
  (
    umask 077
    cat > "$VB_DIR/ssh_config.tmp" <<EOF
Host bs-m6-server bs-m6-server-bs
  HostName $sip
  HostKeyAlias bs-m6-server
Host bs-m6-client bs-m6-client-bs
  HostName $cip
  HostKeyAlias bs-m6-client
Host bs-m6-server bs-m6-client
  User ubuntu
Host bs-m6-server-bs bs-m6-client-bs
  User bs
Host *
  IdentityFile $VB_DIR/key
  IdentitiesOnly yes
  IdentityAgent none
  UserKnownHostsFile $VB_DIR/known_hosts
  GlobalKnownHostsFile /dev/null
  StrictHostKeyChecking yes
  UpdateHostKeys no
  CheckHostIP no
  BatchMode yes
  ForwardAgent no
  ConnectTimeout 10
EOF
    for vm in server client; do
      printf 'bs-m6-%s %s\n' "$vm" "$(cut -d' ' -f1,2 < "$VB_DIR/hostkeys/$vm/ssh_host_ed25519_key.pub")"
    done > "$VB_DIR/known_hosts.tmp"
  )
  mv "$VB_DIR/ssh_config.tmp" "$VB_DIR/ssh_config"
  mv "$VB_DIR/known_hosts.tmp" "$VB_DIR/known_hosts"
}

# vb_ssh <host> <cmd…>: host bs-m6-server, server-bs, …; stdin passed through.
vb_ssh() {
  local h=$1
  shift
  case $h in bs-m6-*) ;; *) h=bs-m6-$h ;; esac
  ssh -F "$VB_DIR/ssh_config" "$h" "$@"
}

# vb_user_data <server|client>: the VM's cloud-config. Its host key is the
# bench's: the image's keys deleted, ed25519 the only type cloud-init may
# generate, and it never does, the key being given (cloud-init's schema
# refuses an empty ssh_genkeytypes, and a schema warning leaves cloud-init
# "degraded"); ubuntu (sudo) and bs log in with the
# bench key; bs's login shell is tcsh on the server (the install must run
# through a non-POSIX shell) and bash on the client, which also gets netem
# (linux-modules-extra) and bs its own ssh key (server install --ssh).
vb_user_data() {
  local vm login hostkey bs_shell bs_groups='' packages p runcmd
  vm=$(vb_vm "$1")
  vb_keys
  login=$(cat "$VB_DIR/key.pub")
  hostkey=$VB_DIR/hostkeys/$vm/ssh_host_ed25519_key
  runcmd='  - [loginctl, enable-linger, bs]'
  case $vm in
    server)
      bs_shell=/usr/bin/tcsh bs_groups=$'\n    groups: [docker]'
      packages=(docker.io docker-compose-v2 tcsh csh dash zsh fish jq)
      ;;
    client)
      bs_shell=/bin/bash
      packages=(jq curl)
      runcmd+=$'\n'"  - [sh, -c, 'apt-get install -y \"linux-modules-extra-\$(uname -r)\"']"
      runcmd+=$'\n'"  - [su, \"-\", bs, -c, \"ssh-keygen -q -t ed25519 -N '' -f ~/.ssh/id_ed25519\"]"
      ;;
  esac
  cat <<EOF
#cloud-config
hostname: bs-m6-$vm
ssh_pwauth: false
ssh_deletekeys: true
ssh_genkeytypes: [ed25519]
ssh_keys:
  ed25519_private: |
$(sed 's/^/    /' "$hostkey")
  ed25519_public: $(cut -d' ' -f1,2 < "$hostkey.pub")
users:
  - name: ubuntu
    shell: /bin/bash
    sudo: ALL=(ALL) NOPASSWD:ALL
    lock_passwd: true
    ssh_authorized_keys:
      - $login
  - name: bs
    shell: $bs_shell$bs_groups
    lock_passwd: true
    ssh_authorized_keys:
      - $login
package_update: true
packages:
$(for p in "${packages[@]}"; do printf '  - %s\n' "$p"; done)
runcmd:
$runcmd
EOF
}

vb_log() { echo "$VB_DIR/logs/up.log"; }

# vb_preflight: up's refusals (exit 2), before anything is made.
vb_preflight() {
  local t list names n owned=() avail mem
  for t in virsh virt-install xorriso minisign ssh-keygen make go curl sha256sum jq timeout; do
    command -v "$t" >/dev/null || vb_refuse "$t is needed"
  done
  list=$(virt-install --osinfo list) || vb_refuse "virt-install --osinfo list failed"
  grep -Eq '^ubuntu24\.04(,|$)' <<<"$list" || vb_refuse "virt-install does not know ubuntu24.04 (update osinfo-db)"
  grep -Eq '^Active: +yes$' <<<"$(vb_virsh net-info "$VB_NET")" || vb_refuse "the libvirt network $VB_NET is not active ($VB_URI)"
  grep -Eq '^State: +running$' <<<"$(vb_virsh pool-info --bytes "$VB_POOL")" || vb_refuse "the libvirt pool $VB_POOL is not active ($VB_URI)"
  names=$(vb_domains) || exit
  while IFS= read -r n; do
    if vb_owned "$n"; then owned+=("$n"); fi
  done <<<"$names"
  [ ${#owned[@]} -eq 0 ] || vb_refuse "a bench exists (${owned[*]}): run reset or down"
  names=$(vb_volumes) || exit
  while IFS= read -r n; do
    if vb_owned "$n"; then owned+=("$n"); fi
  done <<<"$names"
  [ ${#owned[@]} -eq 0 ] || vb_refuse "leftover bench volumes in $VB_POOL (${owned[*]}): run down"
  mem=$(awk '/^MemAvailable:/ { print $2 }' "${VB_MEMINFO:-/proc/meminfo}")
  [ "${mem:-0}" -ge $((16 * 1024 * 1024)) ] || vb_refuse "MemAvailable is $((${mem:-0} / 1024)) MiB, the bench needs 16 GiB"
  avail=$(vb_virsh pool-info --bytes "$VB_POOL" | awk '$1 == "Available:" { print $2 }')
  [ "${avail:-0}" -ge $((80 * 1024 * 1024 * 1024)) ] || vb_refuse "the pool $VB_POOL has $((${avail:-0} >> 30)) GiB free, the bench needs 80 GiB"
}

# vb_fetch <url> <file> <sha256sum line>: downloads once into cache/,
# checks the sum every time.
vb_fetch() {
  local url=$1 file=$2 sum=$3
  vb_mkdir "$VB_DIR/cache"
  if [ ! -s "$VB_DIR/cache/$file" ]; then
    vb_say "downloading $url"
    curl -fsSL --retry 3 -o "$VB_DIR/cache/$file.part" "$url" || vb_fail "cannot download $url"
    mv "$VB_DIR/cache/$file.part" "$VB_DIR/cache/$file"
  fi
  (cd "$VB_DIR/cache" && sha256sum -c --quiet <<<"$sum") || vb_fail "$VB_DIR/cache/$file does not match its pinned sha256 (remove it to download again)"
}

# vb_build: the testkey build of this checkout as VB_VERSION, trusting a
# throwaway minisign key, and its signed release (vb_release).
vb_build() {
  local pub
  [ -s "$VB_DIR/minisign.key" ] || minisign -G -W -p "$VB_DIR/minisign.pub" -s "$VB_DIR/minisign.key" >/dev/null
  pub=$(sed -n 2p "$VB_DIR/minisign.pub")
  vb_mkdir "$VB_DIR/bin"
  GOOS=linux GOARCH=amd64 make -s -C "$VB_ROOT" build OUT="$VB_DIR/bin/backsight" VERSION="$VB_VERSION" TEST_KEY="$pub" ||
    vb_fail "make build failed"
  vb_release
}

# vb_release: release/v<VB_VERSION>/ as update-chain's uc_build lays one
# out for linux/amd64: the archive, the bare binary, checksums.txt, its
# signature, CHANGELOG.md.
vb_release() {
  local rel=$VB_DIR/release/v$VB_VERSION stage=$VB_DIR/work/stage archive bare
  archive=backsight_${VB_VERSION}_linux_amd64.tar.gz bare=backsight_${VB_VERSION}_linux_amd64
  rm -rf "$rel" "$stage"
  vb_mkdir "$rel" "$stage"
  cp "$VB_DIR/bin/backsight" "$stage/backsight"
  cp "$VB_ROOT/LICENSE" "$VB_ROOT/README.md" "$stage/"
  tar -czf "$rel/$archive" -C "$stage" backsight LICENSE README.md
  cp "$VB_DIR/bin/backsight" "$rel/$bare"
  (cd "$rel" && sha256sum "$archive" "$bare" > checksums.txt)
  minisign -S -s "$VB_DIR/minisign.key" -m "$rel/checksums.txt" >/dev/null
  printf '## Changelog\n* 0000000 🧪 The M6 bench release\n' > "$rel/CHANGELOG.md"
  rm -rf "$stage"
}

# vb_upload <volume> <file> <format>: a new pool volume holding the file.
vb_upload() {
  vb_virsh vol-create-as "$VB_POOL" "$1" "$(stat -c %s "$2")" --format "$3" >> "$(vb_log)"
  vb_virsh vol-upload --pool "$VB_POOL" "$1" "$2" >> "$(vb_log)"
}

# vb_create <vm> <vcpus> <MiB> <disk>: its seed ISO, its overlay on the
# base image, the domain.
vb_create() {
  local vm=$1 seed=$VB_DIR/work/seed-$1
  vb_mkdir "$seed"
  (umask 077 && vb_user_data "$vm" > "$seed/user-data")
  printf 'instance-id: bs-m6-%s\nlocal-hostname: bs-m6-%s\n' "$vm" "$vm" > "$seed/meta-data"
  xorriso -as mkisofs -output "$seed/seed.iso" -volid cidata -joliet -rock "$seed/user-data" "$seed/meta-data" >> "$(vb_log)" 2>&1 ||
    vb_fail "xorriso failed (see $(vb_log))"
  vb_upload "bs-m6-$vm-seed.iso" "$seed/seed.iso" raw
  rm -rf "$seed"
  vb_virsh vol-create-as "$VB_POOL" "bs-m6-$vm.qcow2" "$4" --format qcow2 --backing-vol "$VB_BASE" --backing-vol-format qcow2 >> "$(vb_log)"
  virt-install --connect "$VB_URI" --name "bs-m6-$vm" --osinfo ubuntu24.04 --vcpus "$2" --memory "$3" \
    --disk "vol=$VB_POOL/bs-m6-$vm.qcow2" --disk "vol=$VB_POOL/bs-m6-$vm-seed.iso,device=cdrom" \
    --network "network=$VB_NET,mac=$(vb_mac "$vm")" --graphics none --noautoconsole --import >> "$(vb_log)" 2>&1 ||
    vb_fail "virt-install bs-m6-$vm failed (see $(vb_log))"
}

# vb_wait_ip <vm> [client id…]: the VM's address once it has a lease of
# another client id than those given (5 min).
vb_wait_ip() {
  local deadline=$((SECONDS + 300)) ip
  until ip=$(vb_ip "$@"); do
    [ $SECONDS -lt $deadline ] || vb_fail "bs-m6-$1 has no DHCP lease after 5 min"
    sleep 3
  done
  echo "$ip"
}

# vb_wait_ssh <host>: the bench's ssh answers (5 min).
vb_wait_ssh() {
  local deadline=$((SECONDS + 300))
  vb_mkdir "$VB_DIR/logs" # ssh's stderr goes there: a failed redirection is a failed try
  until vb_ssh "$1" true < /dev/null 2>> "$VB_DIR/logs/ssh.log"; do
    [ $SECONDS -lt $deadline ] || vb_fail "no ssh to $1 after 5 min (see $VB_DIR/logs/ssh.log)"
    sleep 5
  done
}

# vb_images: the images the pinned compose renders (internal/server/render.go).
vb_images() {
  sed -n -e 's/^const pinnedHindsight = "\(.*\)"$/\1/p' \
    -e 's/^[[:space:]]*PostgresImage[[:space:]]*= "\(.*\)"$/\1/p' "$VB_ROOT/internal/server/render.go"
}

# vb_provision_server: a warm image cache, so a run downloads no image.
vb_provision_server() {
  local images img
  mapfile -t images < <(vb_images)
  [ ${#images[@]} -eq 2 ] || vb_fail "want the Postgres and Hindsight pins in internal/server/render.go, found: ${images[*]}"
  for img in "${images[@]}"; do
    vb_say "server: docker pull $img"
    vb_ssh bs-m6-server "sudo docker pull -q '$img'" < /dev/null >> "$(vb_log)" 2>&1 || vb_fail "server: docker pull $img failed (see $(vb_log))"
  done
  # bs: tcsh login shell, in docker, lingering.
  [ "$(vb_ssh bs-m6-server 'getent passwd bs | cut -d: -f7' < /dev/null)" = /usr/bin/tcsh ] || vb_fail "server: bs's shell is not /usr/bin/tcsh"
  vb_ssh bs-m6-server-bs 'docker ps -q' < /dev/null > /dev/null 2>> "$(vb_log)" || vb_fail "server: bs cannot use docker"
  [ "$(vb_ssh bs-m6-server 'loginctl show-user bs -p Linger --value' < /dev/null)" = yes ] || vb_fail "server: bs does not linger"
}

# vb_provision_client: Node and Claude Code (CI's pins) for bs, the bench
# build and its release, netem.
vb_provision_client() {
  local node=node-v$VB_NODE-linux-x64.tar.xz
  vb_fetch "https://nodejs.org/dist/v$VB_NODE/$node" "$node" "$VB_NODE_SHASUM"
  vb_say "client: Node $VB_NODE, Claude Code $VB_CLAUDE"
  vb_ssh bs-m6-client-bs 'rm -rf ~/.local/node && mkdir -p ~/.local/node ~/.npm-global && tar -xJf - -C ~/.local/node --strip-components=1' < "$VB_DIR/cache/$node" ||
    vb_fail "client: cannot unpack Node"
  # Every shell of bs, login or not (Ubuntu's bash reads .bashrc for an
  # ssh command, before its interactive test when sourced first).
  vb_ssh bs-m6-client-bs 'echo '\''export PATH="$HOME/.npm-global/bin:$HOME/.local/node/bin:$PATH"'\'' > ~/.bs-m6-env &&
    sed -i "1i . ~/.bs-m6-env" ~/.bashrc && echo ". ~/.bs-m6-env" >> ~/.profile' < /dev/null || vb_fail "client: cannot set bs's PATH"
  vb_ssh bs-m6-client-bs "npm install -g --prefix ~/.npm-global --allow-scripts=@anthropic-ai/claude-code @anthropic-ai/claude-code@$VB_CLAUDE" < /dev/null >> "$(vb_log)" 2>&1 ||
    vb_fail "client: npm install of Claude Code failed (see $(vb_log))"
  case $(vb_ssh bs-m6-client-bs 'claude --version' < /dev/null 2>> "$(vb_log)") in
    "$VB_CLAUDE "*) ;;
    *) vb_fail "client: claude is not $VB_CLAUDE on bs's PATH" ;;
  esac
  vb_ssh bs-m6-client-bs 'cat > ~/backsight && chmod 755 ~/backsight' < "$VB_DIR/bin/backsight" || vb_fail "client: cannot copy backsight"
  tar -C "$VB_DIR" -cf - release | vb_ssh bs-m6-client-bs 'rm -rf ~/release && tar -xf - -C ~' || vb_fail "client: cannot copy the release"
  [ "$(vb_ssh bs-m6-client-bs '~/backsight version --json' < /dev/null | jq -r .data.version)" = "$VB_VERSION" ] ||
    vb_fail "client: ~/backsight is not $VB_VERSION"
  vb_ssh bs-m6-client-bs 'test -s ~/.ssh/id_ed25519' < /dev/null || vb_fail "client: bs has no ssh key"
  vb_ssh bs-m6-client 'sudo tc qdisc add dev lo root netem delay 1ms && sudo tc qdisc del dev lo root' < /dev/null 2>> "$(vb_log)" ||
    vb_fail "client: netem does not work (see $(vb_log))"
}

# vb_snapshot <vm>: the seed ISO (the host's private key) ejected, then
# bs-m6-clean, the state every run starts from.
vb_snapshot() {
  local name=bs-m6-$1 t
  for t in $(vb_virsh domblklist "$name" --details | awk '$2 == "cdrom" { print $3 }'); do
    vb_virsh change-media "$name" "$t" --eject --live --config --force >> "$(vb_log)"
  done
  vb_virsh snapshot-create-as "$name" bs-m6-clean >> "$(vb_log)" || vb_fail "snapshot of $name failed"
}

vb_up() {
  local start=$SECONDS sip cip vm stale_s stale_c
  vb_check_dir
  vb_preflight
  vb_keys
  vb_mkdir "$VB_DIR/cache" "$VB_DIR/results" "$VB_DIR/logs" "$VB_DIR/work" "$VB_DIR/client-b"
  : > "$(vb_log)"
  vb_say "building backsight $VB_VERSION (testkey) and its release"
  vb_build
  vb_fetch "$VB_IMAGE_URL" "${VB_IMAGE_URL##*/}" "$VB_IMAGE_SHA256  ${VB_IMAGE_URL##*/}"
  vb_say "creating the volumes and domains"
  vb_upload "$VB_BASE" "$VB_DIR/cache/${VB_IMAGE_URL##*/}" qcow2
  # The leases of an earlier bench's VMs (same MACs) are not these VMs'.
  stale_s=$(vb_lease_ids server)
  stale_c=$(vb_lease_ids client)
  vb_create server 4 8192 40G
  vb_create client 2 4096 20G
  # shellcheck disable=SC2086 # one client id a word
  sip=$(vb_wait_ip server $stale_s)
  # shellcheck disable=SC2086
  cip=$(vb_wait_ip client $stale_c)
  vb_write_ssh_config "$sip" "$cip"
  for vm in server client; do
    vb_say "waiting for cloud-init on bs-m6-$vm"
    vb_wait_ssh "bs-m6-$vm"
    timeout 600 ssh -F "$VB_DIR/ssh_config" "bs-m6-$vm" 'cloud-init status --wait' < /dev/null >> "$(vb_log)" 2>&1 ||
      vb_fail "cloud-init on bs-m6-$vm did not finish cleanly (scripts/vm-bench.sh ssh $vm cloud-init status --long)"
  done
  vb_provision_server
  vb_provision_client
  for vm in server client; do vb_snapshot "$vm"; done
  vb_say "up in $(((SECONDS - start) / 60)) min $(((SECONDS - start) % 60)) s"
  echo "bs-m6: up (server $sip, client $cip)"
}

# vb_down [--purge]: every domain and volume vb_owned accepts, and VB_DIR
# but results/ and cache/ (--purge: cache/ too).
vb_down() {
  local purge=0 names n f
  case ${1:-} in
    '') ;;
    --purge) purge=1 ;;
    *) vb_refuse "down [--purge], not down $*" ;;
  esac
  vb_check_dir
  names=$(vb_domains) || exit
  while IFS= read -r n; do
    vb_owned "$n" || continue
    # destroy fails on a domain that is not running: then it must be off.
    if ! vb_virsh destroy "$n" > /dev/null 2>&1; then
      [ "$(vb_virsh domstate "$n")" = "shut off" ] || vb_fail "cannot stop $n"
    fi
    vb_virsh undefine "$n" --nvram --snapshots-metadata > /dev/null || vb_fail "cannot undefine $n"
    echo "bs-m6: removed domain $n"
  done <<<"$names"
  names=$(vb_volumes) || exit
  while IFS= read -r n; do
    vb_owned "$n" || continue
    vb_virsh vol-delete --pool "$VB_POOL" "$n" > /dev/null || vb_fail "cannot delete the volume $n"
    echo "bs-m6: removed volume $n"
  done <<<"$names"
  [ -d "$VB_DIR" ] || return 0
  for f in "$VB_DIR"/* "$VB_DIR"/.[!.]*; do
    [ -e "$f" ] || continue
    case ${f##*/} in
      results) continue ;;
      cache) [ $purge = 1 ] || continue ;;
    esac
    rm -rf -- "$f"
    echo "bs-m6: removed $f"
  done
  rmdir "$VB_DIR" 2> /dev/null || true
}

# vb_is_up: both domains running, with their snapshot.
vb_is_up() {
  local vm
  for vm in server client; do
    [ "$(vb_virsh domstate "bs-m6-$vm" 2> /dev/null)" = running ] || return 1
    grep -qx bs-m6-clean <<<"$(vb_virsh snapshot-list "bs-m6-$vm" --name 2> /dev/null)" || return 1
  done
  [ -s "$VB_DIR/ssh_config" ]
}

# vb_reset: both VMs back to bs-m6-clean, their clocks set and their DHCP
# leases renewed (a reverted VM resumes at the snapshot's time, with the
# snapshot's address), client-b/ and work/ emptied.
vb_reset() {
  local vm sip cip
  vb_check_dir
  vb_is_up || vb_refuse "the bench is not up (scripts/vm-bench.sh status; down, then up)"
  for vm in server client; do
    vb_virsh snapshot-revert "bs-m6-$vm" bs-m6-clean --running > /dev/null || vb_fail "cannot revert bs-m6-$vm"
  done
  for vm in server client; do
    vb_wait_ssh "bs-m6-$vm"
    vb_ssh "bs-m6-$vm" "sudo date -u -s @$(date +%s) > /dev/null && for i in /sys/class/net/en*; do sudo networkctl renew \${i##*/}; done" < /dev/null ||
      vb_fail "cannot set the clock or renew the lease of bs-m6-$vm"
  done
  # A renewed lease may carry another address: the config follows it.
  if sip=$(vb_wait_ip server) && cip=$(vb_wait_ip client) &&
    [ "$sip $cip" != "$(awk '$1 == "HostName" { printf "%s%s", sep, $2; sep = " " }' "$VB_DIR/ssh_config")" ]; then
    vb_write_ssh_config "$sip" "$cip"
    vb_say "new addresses: server $sip, client $cip"
    for vm in server client; do vb_wait_ssh "bs-m6-$vm"; done
  fi
  rm -rf "$VB_DIR/client-b" "$VB_DIR/work"
  vb_mkdir "$VB_DIR/client-b" "$VB_DIR/work"
  echo "bs-m6: reset to bs-m6-clean"
}

# vb_status: one line a VM; 0 iff the bench is up.
vb_status() {
  local vm state ip snap
  for vm in server client; do
    if ! state=$(vb_virsh domstate "bs-m6-$vm" 2> /dev/null); then
      printf 'bs-m6-%s: absent\n' "$vm"
      continue
    fi
    ip=$(vb_ip "$vm") || ip=none
    snap=absent
    grep -qx bs-m6-clean <<<"$(vb_virsh snapshot-list "bs-m6-$vm" --name 2> /dev/null)" && snap=present
    printf 'bs-m6-%s: %s, ip %s, snapshot bs-m6-clean %s\n' "$vm" "$state" "$ip" "$snap"
  done
  vb_is_up
}

# ---- run: the M6 proofs (scripts/m6-proofs.sh) on the bench.
#
# The driver: A is bs on bs-m6-client, srv bs on bs-m6-server (tcsh), both
# through the bench's ssh; B is a client on this host in VB_DIR/client-b,
# with its own HOME, BACKSIGHT_HOME and Claude Code (cache/), no
# CLAUDE_CONFIG_DIR, XDG_* or ssh agent, and never server install --ssh
# (OpenSSH would read the user's ~/.ssh whatever HOME says).

# The checks, in the order of decision 0073 block 4's table.
VB_CHECKS=(ssh-unknown-host-key ssh-dropped-mid-install ssh-install ssh-changed-host-key login-shells two-clients timers)

# vb_remote <sh script>: one word any login shell (tcsh, fish…) hands to
# sh unchanged: the script travels base64-encoded.
vb_remote() {
  printf "sh -c 'eval \"\$(printf %%s %s | base64 -d)\"'" "$(printf '%s' "$1" | base64 -w0)"
}

vb_b_home() { echo "$VB_DIR/client-b"; }
vb_b_claude_dir() { echo "$VB_DIR/cache/claude-code-$VB_CLAUDE"; }

# vb_b <cmd…>: cmd as client B, in its home.
vb_b() {
  (cd "$(vb_b_home)" && env -u CLAUDE_CONFIG_DIR -u XDG_CONFIG_HOME -u XDG_DATA_HOME -u XDG_STATE_HOME -u XDG_CACHE_HOME \
    -u SSH_AUTH_SOCK -u BACKSIGHT_STATE_DIR HOME="$(vb_b_home)" BACKSIGHT_HOME="$(vb_b_home)/backsight" DISABLE_AUTOUPDATER=1 \
    PATH="$(vb_b_claude_dir)/bin:/usr/local/bin:/usr/bin:/bin" "$@")
}

# vb_b_claude: B's own Claude Code, CI's pin, installed once under cache/
# (npm's HOME in work/): connect's client steps run claude.
vb_b_claude() {
  local dir
  dir=$(vb_b_claude_dir)
  if [ ! -x "$dir/bin/claude" ]; then
    vb_say "installing Claude Code $VB_CLAUDE for client B in $dir"
    vb_mkdir "$VB_DIR/work/npm-home" "$VB_DIR/logs"
    env -u XDG_CONFIG_HOME -u XDG_DATA_HOME -u XDG_STATE_HOME -u XDG_CACHE_HOME HOME="$VB_DIR/work/npm-home" \
      npm install -g --prefix "$dir" --allow-scripts=@anthropic-ai/claude-code "@anthropic-ai/claude-code@$VB_CLAUDE" >> "$VB_DIR/logs/run.log" 2>&1 ||
      vb_fail "npm install of Claude Code for client B failed (see $VB_DIR/logs/run.log)"
  fi
  case $(vb_b claude --version 2>> "$VB_DIR/logs/run.log") in
    "$VB_CLAUDE "*) ;;
    *) vb_fail "client B's claude is not $VB_CLAUDE ($dir)" ;;
  esac
}

m6_home() {
  case $1 in
    a | srv) echo /home/bs ;;
    b) vb_b_home ;;
    *) fail "no machine $1 (a, b or srv)" ;;
  esac
}

m6_sh() {
  case $1 in
    a) vb_ssh bs-m6-client-bs "$(vb_remote ". ~/.bs-m6-env 2> /dev/null; $2")" ;;
    srv) vb_ssh bs-m6-server-bs "$(vb_remote "$2")" ;;
    b) vb_b sh -c "$2" ;;
    *) fail "no machine $1 (a, b or srv)" ;;
  esac
}

m6_on() {
  local who=$1 a
  shift
  case $who in
    a) m6_sh a "exec \"\$HOME/backsight\" $(m6_q "$@")" ;;
    srv) m6_sh srv "exec \"\$HOME/.local/share/backsight/bin/current\" $(m6_q "$@")" ;;
    b)
      # B never needs a server command: server install would put a server
      # on this host's Docker, next to the user's own; with --ssh, OpenSSH
      # would use the user's ~/.ssh.
      for a in "$@"; do
        case $a in --ssh | --ssh=*) fail "client B never runs server install --ssh: OpenSSH would use the user's ~/.ssh" ;; esac
      done
      for a in "$@"; do
        case $a in
          -*) ;;
          server) fail "client B never runs a server command (backsight server …): this host's Docker runs the user's own server" ;;
          *) break ;;
        esac
      done
      vb_b "$VB_DIR/bin/backsight" "$@"
      ;;
    *) fail "no machine $who (a, b or srv)" ;;
  esac
}

m6_put() { m6_sh "$1" "umask 077 && cat > $(m6_q "$2") && chmod 600 $(m6_q "$2")"; }
m6_get() { m6_sh "$1" "cat $(m6_q "$2")"; }

m6_link() {
  [ "$1" = a ] || fail "m6_link: only A's link is cut"
  vb_virsh domif-setlink bs-m6-client "$VB_CLIENT_MAC" "$2" > /dev/null || fail "cannot set A's link $2"
}

# m6_host_key <new|old>: new gives the server a fresh ed25519 host key,
# which the bench's known_hosts then trusts besides its own; old puts the
# bench's key back and drops that trust.
m6_host_key() {
  local key
  case $1 in
    new)
      key=$VB_DIR/work/hostkey-new/ssh_host_ed25519_key
      rm -rf "${key%/*}"
      vb_mkdir "${key%/*}"
      ssh-keygen -q -t ed25519 -N '' -C bs-m6-server-new -f "$key"
      printf 'bs-m6-server %s\n' "$(cut -d' ' -f1,2 < "$key.pub")" >> "$VB_DIR/known_hosts"
      ;;
    old) key=$VB_DIR/hostkeys/server/ssh_host_ed25519_key ;;
    *) fail "m6_host_key new or old" ;;
  esac
  vb_ssh bs-m6-server 'sudo install -m 600 /dev/stdin /etc/ssh/ssh_host_ed25519_key' < "$key" &&
    vb_ssh bs-m6-server 'sudo install -m 644 /dev/stdin /etc/ssh/ssh_host_ed25519_key.pub && sudo systemctl restart ssh' < "$key.pub" ||
    fail "cannot install the $1 host key on bs-m6-server"
  if [ "$1" = old ]; then vb_untrust_new_key; fi
  vb_wait_ssh bs-m6-server-bs
}

# vb_untrust_new_key: the bench's known_hosts back to the keys it generated.
vb_untrust_new_key() {
  local pub=$VB_DIR/work/hostkey-new/ssh_host_ed25519_key.pub
  [ -s "$pub" ] && [ -s "$VB_DIR/known_hosts" ] || return 0
  grep -vF "$(cut -d' ' -f1,2 < "$pub")" "$VB_DIR/known_hosts" > "$VB_DIR/known_hosts.tmp" || true
  mv "$VB_DIR/known_hosts.tmp" "$VB_DIR/known_hosts"
}

# vb_authorize_a: run's preparation, before any check: A's login key in
# bs's authorized_keys on the server, as the user's ssh-copy-id would put
# it (provisioning, not proof: the host key stays unknown to A).
vb_authorize_a() {
  m6_get a "$(m6_home a)/.ssh/id_ed25519.pub" | m6_sh srv 'mkdir -p -m 700 ~/.ssh && cat >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys' ||
    vb_fail "cannot authorize A's login key for bs on bs-m6-server"
}

# vb_results_table <jsonl>: one markdown row a check.
vb_results_table() {
  printf '| Check | Status | Detail |\n|---|---|---|\n'
  jq -r '"| \(.check) | \(.status) | \(.detail | gsub("\\|"; "\\|") | gsub("\n"; " ")) |"' "$1"
}

# vb_run [check…]: reset, then the install chain and two-clients always,
# then the checks named (all by default) in the table's order, timers
# last; the first FAIL marks the rest NOT RUN. The table goes to stdout
# and results/<stamp>.md, the records to results/<stamp>.jsonl.
vb_run() {
  local want=("$@") c n stamp failed
  local order=(ssh-unknown-host-key ssh-dropped-mid-install ssh-install ssh-changed-host-key two-clients)
  vb_check_dir
  for n in "${want[@]}"; do
    [[ " ${VB_CHECKS[*]} " == *" $n "* ]] || vb_refuse "no check $n (${VB_CHECKS[*]})"
  done
  [ ${#want[@]} -gt 0 ] || want=("${VB_CHECKS[@]}")
  for c in "${VB_CHECKS[@]}"; do
    case $c in ssh-* | two-clients | timers) continue ;; esac
    [[ " ${want[*]} " != *" $c "* ]] || order+=("$c")
  done
  [[ " ${want[*]} " != *" timers "* ]] || order+=(timers)
  vb_is_up || vb_refuse "the bench is not up (scripts/vm-bench.sh status; up first)"
  vb_reset
  vb_mkdir "$VB_DIR/results" "$VB_DIR/work/run" "$VB_DIR/logs"
  vb_b_claude
  # shellcheck source=scripts/m6-proofs.sh
  . "$VB_ROOT/scripts/m6-proofs.sh"
  stamp=$(date -u +%Y%m%dT%H%M%SZ)
  M6_RESULTS=$VB_DIR/results/$stamp.jsonl M6_WORK=$VB_DIR/work/run M6_ROOT=$VB_ROOT M6_VERSION=$VB_VERSION
  M6_COMMIT=$(git -C "$VB_ROOT" describe --always --dirty --abbrev=7 2> /dev/null || echo unknown)
  M6_SERVER_IP=$(awk '$1 == "HostName" { print $2; exit }' "$VB_DIR/ssh_config")
  : > "$M6_RESULTS"
  vb_authorize_a
  # However the run ends, A's link is up and the bench trusts only its keys.
  trap 'vb_virsh domif-setlink bs-m6-client "$VB_CLIENT_MAC" up > /dev/null 2>&1 || true; vb_untrust_new_key' EXIT
  for c in "${order[@]}"; do
    m6_check "$c" "m6_check_${c//-/_}" < /dev/null
  done
  vb_results_table "$M6_RESULTS" | tee "$VB_DIR/results/$stamp.md"
  failed=$(jq -s '[.[] | select(.status == "FAIL")] | length' "$M6_RESULTS")
  [ "$failed" = 0 ]
}

fail() { vb_fail "$@"; }

vb_usage() {
  sed -n '6,13p' "${BASH_SOURCE[0]}" | sed 's/^# //' >&2
  exit 2
}

vb_main() {
  if [ -n "${CI:-}" ] || [ -n "${GITHUB_ACTIONS:-}" ]; then
    vb_refuse "the VM bench never runs on CI (CI or GITHUB_ACTIONS is set)"
  fi
  umask 077
  case ${1:-} in
    up) vb_up ;;
    down)
      shift
      vb_down "$@"
      ;;
    reset) vb_reset ;;
    status) vb_status ;;
    run)
      shift
      vb_run "$@"
      ;;
    ssh)
      case ${2:-} in
        server | client | server-bs | client-bs) ;;
        *) vb_refuse "ssh server, client, server-bs or client-bs" ;;
      esac
      [ -s "$VB_DIR/ssh_config" ] || vb_refuse "no $VB_DIR/ssh_config: run up"
      local host=bs-m6-$2
      shift 2
      exec ssh -F "$VB_DIR/ssh_config" "$host" "$@"
      ;;
    *) vb_usage ;;
  esac
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  set -euo pipefail
  vb_main "$@"
fi
