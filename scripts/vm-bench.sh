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
#   scripts/vm-bench.sh run [check…]    reset, this tree built and given to A and B, the
#                                       install chain and two-clients, then the checks
#                                       named (default: all); results/<UTC stamp>.md
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
  vb_push_a
  vb_ssh bs-m6-client-bs 'test -s ~/.ssh/id_ed25519' < /dev/null || vb_fail "client: bs has no ssh key"
  vb_ssh bs-m6-client 'sudo tc qdisc add dev lo root netem delay 1ms && sudo tc qdisc del dev lo root' < /dev/null 2>> "$(vb_log)" ||
    vb_fail "client: netem does not work (see $(vb_log))"
}

# vb_push_a: the bench build to A (~/backsight) with its release
# (~/release), the one ssh install sends the server.
vb_push_a() {
  vb_ssh bs-m6-client-bs 'cat > ~/backsight && chmod 755 ~/backsight' < "$VB_DIR/bin/backsight" || vb_fail "client: cannot copy backsight"
  tar -C "$VB_DIR" -cf - release | vb_ssh bs-m6-client-bs 'rm -rf ~/release && tar -xf - -C ~' || vb_fail "client: cannot copy the release"
  [ "$(vb_ssh bs-m6-client-bs '~/backsight version --json' < /dev/null | jq -r .data.version)" = "$VB_VERSION" ] ||
    vb_fail "client: ~/backsight is not $VB_VERSION"
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
  # A link a stopped run left down stays down through a revert.
  vb_virsh domif-setlink bs-m6-client "$VB_CLIENT_MAC" up > /dev/null || vb_fail "cannot bring bs-m6-client's link up"
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

# The checks, in the order of decision 0073 block 4's table, but restore
# and purge run before the offline and latency checks, which leave
# sessions on A for its timer to send while the restore check counts what
# each tick re-sends.
VB_CHECKS=(ssh-unknown-host-key ssh-dropped-mid-install ssh-install ssh-changed-host-key login-shells two-clients timers
  dedup-copied dedup-concurrent dedup-split key-rotation restore-across-clients purge-across-clients
  offline-laptop offline-hung unsent-cost latency-0ms latency-20ms latency-80ms)

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
      # Any word "server", wherever it stands: a global flag taking a value
      # may come before the command.
      for a in "$@"; do
        case $a in
          server) fail "client B never runs a server command (backsight server …): this host's Docker runs the user's own server" ;;
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

# vb_run_end: however a run ends, A's link is up with no netem or drop
# rule, the bench trusts only its own keys, and no token stays in B's
# home (two-clients' and key-rotation's).
vb_run_end() {
  # First: a later step that fails must not leave a token behind.
  rm -f "$(vb_b_home)/token" "$(vb_b_home)/token2"
  vb_virsh domif-setlink bs-m6-client "$VB_CLIENT_MAC" up > /dev/null 2>&1 || true
  vb_no_netem > /dev/null 2>&1 || true
  vb_no_drop > /dev/null 2>&1 || true
  vb_untrust_new_key
}

# ---- The checks of the client VM's network (offline-laptop, unsent-cost,
# latency-*): they cut A's link or slow it, which only the bench can do.

# The project of the offline session and of the latency sessions.
VB_OFFLINE_PROJECT=bs-m6-offline
VB_LATENCY_PROJECT=bs-m6-latency

# Forty prompts of eight words or more and at least recall_min_chars (60),
# on unrelated topics, so that recall runs on each (a topic shift, C12):
# the offline session takes the first ten.
VB_PROMPTS=(
  "How should the garden irrigation timer be set for the dry summer weeks ahead?"
  "Which sourdough hydration works best for a loaf baked in a cast iron pot?"
  "What is the safest way to replace a cracked bathroom tile without new grout?"
  "Explain how compound interest changes a small monthly savings plan over ten years."
  "Which stretches help a cyclist recover after a long mountain climb in the cold?"
  "How do I teach a young dog to walk calmly on a leash in busy streets?"
  "What makes a Romanesque church different from a Gothic cathedral in its windows?"
  "Suggest a weekly menu for a vegetarian family of four on a tight budget."
  "How does a heat pump keep a house warm when the outside air is freezing?"
  "Which birds can be seen in a wetland reserve during the spring migration season?"
  "What are the rules of the board game Go for a complete beginner player?"
  "How should a beginner tune a classical guitar without an electronic tuner nearby?"
  "Which houseplants survive in a dark north facing apartment with dry winter air?"
  "Describe the main causes of the French Revolution for a school history essay."
  "How long should a family rest between two hikes at high altitude in the Alps?"
  "What is the difference between baking soda and baking powder in a sponge cake?"
  "Which knots should a sailor learn first before taking a small dinghy out alone?"
  "How can a small library organise its children's books so readers find them easily?"
  "What should a first aid kit for a week of camping in the forest contain?"
  "Explain why the sky looks red at sunset and blue in the middle of the day."
  "How do volcanoes form along the edges of tectonic plates under the ocean floor?"
  "Which watercolour paper weight suits quick landscape sketches made outdoors on a trip?"
  "How should a runner plan training for a first half marathon over twelve weeks?"
  "What is the best way to store winter clothes so moths do not damage the wool?"
  "Which chess openings suit a club player who likes quiet positional games with pawns?"
  "How do bees communicate the direction of flowers to the other bees in their hive?"
  "Suggest a calm evening routine that helps a toddler fall asleep before eight o'clock."
  "What is the right way to sharpen a kitchen knife on a whetstone at home?"
  "How did the printing press change the spread of books across Europe after 1450?"
  "Which fruit trees grow well in a small urban garden with clay soil and wind?"
  "How should a beginner photographer use manual exposure for a portrait taken at dusk?"
  "What causes the tides, and why are there two high tides on most coastal days?"
  "Which exercises strengthen the lower back of someone who sits at a desk all day?"
  "How do you make a simple tomato sauce that freezes well for later winter meals?"
  "What should travellers know about the night trains that cross the Alps in winter?"
  "Explain how a bicycle derailleur moves the chain between the gears on the cassette."
  "Which wildflowers attract butterflies to a balcony box during the long summer months?"
  "How can a choir improve its breathing so long phrases stay steady and in tune?"
  "What is the history of the lighthouse on the rocky island near the old harbour?"
  "How should a family prepare an old wooden table before painting it a new colour?"
)

# vb_hook_lib: the A-side helpers of the offline and latency scripts, run
# in a session of their own (setsid) that their watchdog stops whole once
# bound s have passed ($bound must be set).
#   hk <name> <Event> <subject> <json>  runs the command hooks.json wires for
#     that event (its group whose matcher takes the subject, or has none),
#     the json on stdin, as Claude Code would, killed 2 s past its timeout;
#     appends {name, event, ms, timeout_s, rc} to hooks.jsonl, its stdout to
#     out/<name>. A hook killed past its deadline is recorded so (rc 124).
#   ev <session> <Event> [key value…]  Claude Code's JSON for the event.
#   step <name> <bound> <cmd…>  the command killed past bound s, its stdout
#     to <name>.json; {name, ms, rc, bound_s} to steps.jsonl.
#   tk <name>  step <name> 300 jobs tick --json, a skipped one tried again
#     (A's own timer may hold the tick lock) for 90 s.
#   breaker_wait  waits until the hooks' breaker is closed (6 min at most);
#     the seconds waited in waited.
vb_hook_lib() {
  cat << 'EOF'
echo $$ > pgid
trap 'exit 143' TERM
(
  i=0
  while [ "$i" -lt "$bound" ]; do sleep 1; i=$((i + 1)); done
  echo "the script passed its bound of $bound s" >> error
  pkill -TERM -s "$(cat pgid)"
) > /dev/null 2>&1 &
wd=$!
hk() {
  local name=$1 event=$2 subj=$3 json=$4 spec t0 t1 rc=0 to cmd
  spec=$(jq -c --arg e "$event" --arg s "$subj" '[.hooks[$e][]? | (.matcher // "") as $m | select($m == "" or ($s | test("^(" + $m + ")$")))][0].hooks[0] // empty' "$hooks") || spec=
  if [ -z "$spec" ]; then echo "hooks.json ($hooks) wires no $event hook for '$subj'" >> error; exit 1; fi
  mapfile -t cmd < <(jq -r '.command, .args[]' <<< "$spec")
  to=$(jq '.timeout // 0' <<< "$spec")
  t0=$(date +%s%N)
  printf '%s' "$json" | timeout -k 1 $((to + 2)) "${cmd[@]}" > "out/$name" 2> "out/$name.err" || rc=$?
  t1=$(date +%s%N)
  jq -cn --arg n "$name" --arg e "$event" --argjson ms $(((t1 - t0) / 1000000)) --argjson to "$to" --argjson rc "$rc" \
    '{name: $n, event: $e, ms: $ms, timeout_s: $to, rc: $rc}' >> hooks.jsonl
}
ev() {
  local s=$1 e=$2
  shift 2
  jq -cn --arg s "$s" --arg e "$e" --arg t "$tdir/$s.jsonl" --arg c "$cwd" --args \
    '$ARGS.positional as $kv | reduce range(0; $kv | length; 2) as $i ({session_id: $s, transcript_path: $t, cwd: $c, hook_event_name: $e}; .[$kv[$i]] = $kv[$i + 1])' "$@"
}
step() {
  local name=$1 b=$2 t0 rc=0
  shift 2
  t0=$(date +%s%N)
  timeout -k 5 "$b" "$@" > "$name.json" 2> /dev/null || rc=$?
  jq -cn --arg n "$name" --argjson ms $((($(date +%s%N) - t0) / 1000000)) --argjson rc "$rc" --argjson b "$b" '{name: $n, ms: $ms, rc: $rc, bound_s: $b}' >> steps.jsonl
}
tk() {
  local n=0
  while :; do
    step "$1" 300 "$HOME/backsight" jobs tick --json
    [ "$(jq -r '.data.status // ""' "$1.json" 2> /dev/null)" = skipped ] || break
    n=$((n + 1))
    [ "$n" -lt 45 ] || break
    sleep 2
  done
}
breaker_wait() {
  local open left
  echo 0 > waited
  [ -s "$st/breaker.json" ] || return 0
  open=$(jq -r '.open_until // empty' "$st/breaker.json")
  left=$(($(date -d "${open:-1970-01-01T00:00:00Z}" +%s) - $(date +%s)))
  if [ "$left" -gt 360 ]; then echo "the breaker stays open $left s more" >> error; exit 1; fi
  if [ "$left" -ge 0 ]; then sleep $((left + 2)); echo $((left + 2)) > waited; fi
}
paths=$("$HOME/backsight" paths --json) || { echo "backsight paths failed" >> error; exit 1; }
st=$(jq -r .data.state <<< "$paths")
hooks=$(jq -r .data.data <<< "$paths")/marketplace/plugins/backsight/hooks/hooks.json
since=$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)
mkdir -p out
EOF
}

# vb_offline_script: run on A, detached in a session of its own. Args: the
# mode (laptop: the bench cuts A's link, and the bench's ssh dies with
# it; hung: a DROP rule blackholes the server, the link up), the server's
# URL, the script's bound in s, the project, the session, then the
# prompts. It waits until /health fails (down-after tries, curl's exit in
# down-rc), then runs Claude Code's hooks of the session (their wall times
# against hooks.json), two notes, a tick and doctor. laptop then backdates
# reach.json 2 h (the banner and doctor's warning come after an hour of
# silence), runs a second session's startup and doctor, and unsent-cost's
# two ticks around 200 uncursored transcripts of 50 records, which it
# deletes; hung waits first for the breaker to close, so the prompts meet
# the hung server, and keeps the breaker's state. Last, /health must
# still fail (health-end). Everything lands in the script's directory;
# done holds the seconds taken.
vb_offline_script() {
  printf 'set -u\nmode=$1 url=$2 bound=$3 p=$4 sid=$5\nshift 5\nd=$(dirname "$(readlink -f "$0")")\ncd "$d" || exit 1\nt0=$(date +%%s)\n'
  printf 'trap '\''echo $(($(date +%%s) - t0)) > "$d/done"'\'' EXIT\n'
  printf 'tdir=$HOME/.claude/projects/-bs-m6-$p cwd=$HOME/work/$p\n'
  vb_hook_lib
  cat << 'EOF'
echo "$since" > since
n=0
while :; do
  rc=0
  curl -fsS --max-time 2 -o /dev/null "$url/health" 2> /dev/null || rc=$?
  [ "$rc" = 0 ] || break
  n=$((n + 1))
  if [ "$n" -ge 60 ]; then echo "the server still answered $url/health 60 s after the cut" >> error; exit 1; fi
  sleep 1
done
echo "$n" > down-after
echo "$rc" > down-rc
if [ "$mode" = hung ]; then breaker_wait; fi
mkdir -p "$tdir" "$cwd"
hk start1 SessionStart startup "$(ev "$sid" SessionStart source startup)"
# The session's records, 40 min old: settled (30 min) once the server is back.
now=$(date +%s)
for i in 1 2 3; do
  ts=$(date -u -d "@$((now - 2400 + i))" +%Y-%m-%dT%H:%M:%S.000Z)
  jq -cn --arg u "$sid-r$i" --arg s "$sid" --arg ts "$ts" --arg c "$cwd" --arg t "$sid turn $i: the offline laptop kept working on the export job. [rec $sid-r$i]" \
    '{type: "user", uuid: $u, sessionId: $s, cwd: $c, timestamp: $ts, message: {role: "user", content: $t}}' >> "$tdir/$sid.jsonl"
done
touch -d "@$((now - 2397))" "$tdir/$sid.jsonl"
i=0
for prompt in "$@"; do
  i=$((i + 1))
  hk "p$i" UserPromptSubmit "" "$(ev "$sid" UserPromptSubmit prompt "$prompt" prompt_id "$sid-p$i")"
done
for i in 1 2; do
  step "store$i" 60 "$HOME/backsight" store --json --kind decision --project "$p" "bs-m6 offline $mode: note $i of $sid, the export pages by 200, decided with the server out of reach"
done
hk end SessionEnd "" "$(ev "$sid" SessionEnd reason other)"
cp "$st/breaker.json" breaker-end.json 2> /dev/null || echo '{}' > breaker-end.json
tk tick1
step doctor1 120 "$HOME/backsight" doctor --json
if [ "$mode" = laptop ]; then
  cp "$st/reach.json" reach-before.json 2> /dev/null || echo '{}' > reach-before.json
  back=$(date -u -d '-2 hours' +%Y-%m-%dT%H:%M:%SZ)
  echo "$back" > backdated
  jq --arg b "$back" '.failing_since = $b | .last_ok = $b' reach-before.json > "$st/reach.json.bs-m6" && mv "$st/reach.json.bs-m6" "$st/reach.json"
  hk start2 SessionStart startup "$(ev of2 SessionStart source startup)"
  step doctor2 120 "$HOME/backsight" doctor --json
  # unsent-cost: the same tick, then with 200 uncursored transcripts more.
  tk tick-base
  cp "$st/reach.json" reach-base.json
  ud=$HOME/.claude/projects/-bs-m6-bs-m6-unsent uc=$HOME/work/bs-m6-unsent
  mkdir -p "$ud" "$uc"
  now=$(date +%s)
  for i in $(seq 1 50); do tss[i]=$(date -u -d "@$((now - 600 + i))" +%Y-%m-%dT%H:%M:%S.000Z); done
  for s in $(seq -w 1 200); do
    for i in $(seq 1 50); do
      ts=${tss[i]}
      if [ $((i % 2)) = 1 ]; then
        printf '{"type":"user","uuid":"un%s-r%s","sessionId":"un%s","cwd":"%s","timestamp":"%s","message":{"role":"user","content":"un%s turn %s: the export job of the M6 bench failed at page %s. [rec un%s-r%s]"}}\n' "$s" "$i" "$s" "$uc" "$ts" "$s" "$i" "$i" "$s" "$i"
      else
        printf '{"type":"assistant","uuid":"un%s-r%s","sessionId":"un%s","cwd":"%s","timestamp":"%s","message":{"role":"assistant","content":[{"type":"text","text":"Noted for un%s: the export pages by 200. [rec un%s-r%s]"}]}}\n' "$s" "$i" "$s" "$uc" "$ts" "$s" "$s" "$i"
      fi
    done > "$ud/un$s.jsonl"
  done
  find "$ud" -name '*.jsonl' | grep -c . > unsent-files
  tk tick-unsent
  cp "$st/reach.json" reach-unsent.json
  rm -rf "$ud" "$uc"
fi
if curl -fsS --max-time 2 -o /dev/null "$url/health" 2> /dev/null; then echo up > health-end; else echo down > health-end; fi
kill "$wd" 2> /dev/null
EOF
}

# vb_latency_script: run on A in a session of its own (setsid -w). Args:
# the delay, the project, the bound in s, the prompts. Waits until the
# hooks' breaker is closed (an earlier check may have opened it), then a
# startup hook and one UserPromptSubmit hook a prompt of session
# lt<delay>; prints {waited_s, hooks, prompts, failures}: the hooks' wall
# times, the session's prompt telemetry and every hook_failure timeout
# since its start.
vb_latency_script() {
  printf 'set -u\ndelay=$1 p=$2 bound=$3\nshift 3\nd=$(dirname "$(readlink -f "$0")")\ncd "$d" || exit 1\nrm -f hooks.jsonl events.jsonl error\n'
  printf 'tdir=$HOME/.claude/projects/-bs-m6-$p cwd=$HOME/work/$p\n'
  vb_hook_lib
  cat << 'EOF'
mkdir -p "$tdir" "$cwd"
breaker_wait
s=lt$delay
hk start SessionStart startup "$(ev "$s" SessionStart source startup)"
i=0
for prompt in "$@"; do
  i=$((i + 1))
  hk "p$i" UserPromptSubmit "" "$(ev "$s" UserPromptSubmit prompt "$prompt" prompt_id "$s-p$i")"
done
kill "$wd" 2> /dev/null
if [ -s error ]; then cat error >&2; exit 1; fi
cat "$st"/telemetry/events-*.jsonl | jq -cR --arg since "$since" 'fromjson? | select(.ts >= $since)' > events.jsonl
jq -cn --argjson w "$(cat waited)" --arg s "$s" --slurpfile h hooks.jsonl --slurpfile e events.jsonl \
  '{waited_s: $w, hooks: $h, prompts: [$e[] | select(.event == "prompt" and .session == $s) | {n, decision, reason, ms}],
    failures: [$e[] | select(.event == "hook_failure" and .kind == "timeout")]}'
EOF
}

# vb_wait_a: A's ssh answers again (5 min), as a check's fail.
vb_wait_a() {
  local deadline=$((SECONDS + 300))
  until vb_ssh bs-m6-client-bs true < /dev/null 2> /dev/null; do
    [ "$SECONDS" -lt "$deadline" ] || fail "A's ssh did not come back within 5 min of its link"
    sleep 5
  done
}

# vb_drop: a DROP rule on A's OUTPUT to the server, tagged bs-m6-hung:
# connections to it hang (the SYN never leaves), A's link and the bench's
# ssh stay up. vb_no_drop removes every rule so tagged and fails when one
# stays.
vb_drop() {
  vb_ssh bs-m6-client "sudo iptables -I OUTPUT -d $M6_SERVER_IP -m comment --comment bs-m6-hung -j DROP" < /dev/null
}
vb_no_drop() {
  vb_ssh bs-m6-client 'n=0; while sudo iptables -S OUTPUT | grep -q -- "--comment bs-m6-hung" && [ $n -lt 10 ]; do sudo iptables -S OUTPUT | grep -m1 -- "--comment bs-m6-hung" | sed '\''s/^-A /-D /'\'' | xargs sudo iptables; n=$((n + 1)); done; ! sudo iptables -S OUTPUT | grep -q -- "--comment bs-m6-hung"' < /dev/null
}

# vb_offline_cleanup <dir>: however an offline run ends, A's link is up
# and no drop rule stays, the offline script's session is stopped when it
# did not end, and what the offline checks write on A is gone: their
# directory, their transcripts, a backdated reach.json's temp file.
vb_offline_cleanup() {
  local dir=${1:-m6-offline}
  vb_virsh domif-setlink bs-m6-client "$VB_CLIENT_MAC" up > /dev/null 2>&1 || true
  vb_no_drop > /dev/null 2>&1 || true
  (vb_wait_a) > /dev/null 2>&1 || true
  m6_sh a "d=\$HOME/$dir; if [ -s \"\$d/pgid\" ] && [ ! -e \"\$d/done\" ]; then pkill -TERM -s \"\$(cat \"\$d/pgid\")\"; fi; st=\$(\"\$HOME/backsight\" paths --json 2> /dev/null | jq -r '.data.state // empty'); rm -rf \"\$d\" \"\$HOME/.claude/projects/-bs-m6-bs-m6-offline\" \"\$HOME/work/bs-m6-offline\" \"\$HOME/.claude/projects/-bs-m6-bs-m6-unsent\" \"\$HOME/work/bs-m6-unsent\"; if [ -n \"\$st\" ]; then rm -f \"\$st/reach.json.bs-m6\"; fi" > /dev/null 2>&1 || true
}

# vb_offline_run <laptop|hung>: the offline script of that mode, once a
# run (offline-laptop and unsent-cost share laptop's), collected into
# M6_WORK/offline (laptop) or M6_WORK/offline-hung. laptop: the bench
# cuts A's link right after starting it, waits its bound (M6_OFFLINE_S,
# 360 s), brings the link back and collects. hung: the bench sets the
# drop rule, waits for the script (M6_HUNG_S, 1200 s, and 1 min), lifts
# the rule and collects. Positive controls: A reads /health before, the
# script saw it fail (hung: by a timeout, not a refusal), and it fails
# again at the script's end, else the run is void.
vb_offline_run() {
  local mode=${1:-laptop} w dir sid bound h url rule _
  case $mode in
    laptop) w=$M6_WORK/offline dir=m6-offline sid=of1 bound=${M6_OFFLINE_S:-360} ;;
    hung) w=$M6_WORK/offline-hung dir=m6-offline-hung sid=oh1 bound=${M6_HUNG_S:-1200} ;;
    *) fail "vb_offline_run laptop or hung, not $mode" ;;
  esac
  [ ! -e "$w/collected" ] || return 0
  rm -rf "$w"
  mkdir -p "$w"
  h=$(m6_home a)
  url=$(m6_get a "$(m6_paths a | jq -r .config_file)" | m6_toml server url)
  url=${url%/}
  [ -n "$url" ] || fail "A's config names no server url"
  [ "$(m6_outbox a)" = 0 ] || fail "A's outbox holds notes before the cut: its 2 queued would prove nothing"
  m6_live_since a "$M6_LIVE_MIN"
  m6_sh a "curl -fsS --max-time 5 -o /dev/null $(m6_q "$url/health")" ||
    fail "A cannot read $url/health with its link up: that it fails offline would prove nothing"
  m6_sh a "rm -rf ~/$dir && mkdir -p ~/$dir" || fail "cannot make ~/$dir on A"
  vb_offline_script | m6_put a "$h/$dir/run.sh" || fail "cannot write the offline script on A"
  # shellcheck disable=SC2064 # the directory is this run's, fixed now
  trap "vb_offline_cleanup $dir" EXIT
  m6_sh a "setsid nohup bash $(m6_q "$h/$dir/run.sh" "$mode" "$url" "$bound" "$VB_OFFLINE_PROJECT" "$sid" "${VB_PROMPTS[@]:0:10}") > /dev/null 2>&1 < /dev/null &" ||
    fail "cannot start the offline script on A"
  if [ "$mode" = laptop ]; then
    m6_link a down
    sleep "$bound"
    m6_link a up
    vb_wait_a
    for _ in $(seq 1 60); do
      if m6_sh a "test -e ~/$dir/done"; then break; fi
      sleep 2
    done
    m6_sh a "test -e ~/$dir/done" || fail "the offline script on A did not end within 2 min of the link coming back"
  else
    vb_drop || fail "cannot set the drop rule on A"
    rule=$(vb_ssh bs-m6-client 'sudo iptables -S OUTPUT' < /dev/null) || fail "cannot read A's OUTPUT rules"
    case $rule in *"-d $M6_SERVER_IP/32 -m comment --comment bs-m6-hung -j DROP"*) ;; *) fail "the drop rule is not on A: $rule" ;; esac
    for _ in $(seq 1 $(((bound + 60) / 5))); do
      if m6_sh a "test -e ~/$dir/done"; then break; fi
      sleep 5
    done
    m6_sh a "test -e ~/$dir/done" || fail "the offline script on A did not end within $((bound + 60)) s"
    vb_no_drop > /dev/null || fail "the drop rule stayed on A"
  fi
  m6_sh a "tar -C ~/$dir -cf - ." | tar -C "$w" -xf - || fail "cannot collect ~/$dir from A"
  [ ! -s "$w/error" ] || fail "the offline script on A: $(paste -sd ' ' "$w/error")"
  [ -s "$w/down-after" ] || fail "the offline script never saw /health fail"
  if [ "$mode" = hung ] && [ "$(cat "$w/down-rc")" != 28 ]; then
    fail "A's /health failed with curl exit $(cat "$w/down-rc"), not a timeout: the server refused, it did not hang"
  fi
  if [ "$(cat "$w/health-end" 2> /dev/null)" != down ]; then
    if [ "$mode" = laptop ]; then
      fail "the link came back too early: /health answered A at the offline script's end ($(cat "$w/done" 2> /dev/null || echo ?) s, bound $bound s)"
    fi
    fail "the drop let /health through at the offline script's end"
  fi
  touch "$w/collected"
}

# vb_hooks_in_time <hooks.jsonl>: every hook's wall time under its
# hooks.json timeout, which must be known, and every hook exited 0 (the
# supervisor always does); prints the slowest per event.
vb_hooks_in_time() {
  local late bad
  jq -e -s 'length > 0 and all(.timeout_s > 0)' "$1" > /dev/null || fail "a hook ran without a hooks.json timeout, or none ran"
  late=$(jq -r -s '[.[] | select(.ms >= .timeout_s * 1000) | "\(.name) (\(.event)) \(.ms) ms, deadline \(.timeout_s) s"] | join("; ")' "$1")
  [ -z "$late" ] || fail "past its deadline: $late"
  bad=$(jq -r -s '[.[] | select(.rc != 0) | "\(.name) (\(.event)) rc \(.rc)"] | join("; ")' "$1")
  [ -z "$bad" ] || fail "a hook did not exit 0: $bad"
  jq -c -s 'group_by(.event) | map({(.[0].event): (map(.ms) | max)}) | add' "$1"
}

# vb_slowest <slowest json>: "Event ms, …".
vb_slowest() { jq -r 'to_entries | map("\(.key) \(.value)") | join(", ")' <<< "$1"; }

# vb_telemetry_since <since>: A's telemetry events at or after since (A's clock).
vb_telemetry_since() {
  m6_sh a "st=\$(\"\$HOME/backsight\" paths --json | jq -r .data.state) && cat \"\$st\"/telemetry/events-*.jsonl | jq -cR --arg s $(m6_q "$1") 'fromjson? | select(.ts >= \$s)'" ||
    fail "cannot read A's telemetry"
}

# vb_offline_notes <dir>: the two notes the offline script stored, which
# must be queued.
vb_offline_notes() {
  local i
  for i in 1 2; do
    jq -e '.data.status == "queued"' "$1/store$i.json" > /dev/null 2>&1 || fail "store $i with the server $2: $(cat "$1/store$i.json"), want queued"
    jq -r .data.document "$1/store$i.json"
  done
}

# vb_offline_sent <sid> <note…>: once the server is back, the session and
# the notes on the server once, A's outbox empty, doctor's server-id ok
# and server-reach no longer silent.
vb_offline_sent() {
  local sid=$1 counts note doc reach
  shift
  m6_queue_wait
  m6_wait_doc "$sid" > /dev/null
  counts=$(m6_uuid_counts "$sid") || exit 1
  [ "$counts" = "$(printf '%s-r%s 1\n' "$sid" 1 "$sid" 2 "$sid" 3)" ] || fail "conversation:$sid holds the records $(paste -sd ' ' <<< "$counts"), want r1 r2 r3 once each"
  for note in "$@"; do
    [ -n "$(m6_doc "$note")" ] || fail "the note $note queued offline is not on the server"
  done
  [ "$(m6_outbox a)" = 0 ] || fail "A's outbox still holds $(m6_outbox a) note(s)"
  doc=$(m6_doctor a) || exit 1
  [ "$(jq -r '[.data.checks[] | select(.id == "server-id") | .status][0]' <<< "$doc")" = ok ] ||
    fail "doctor's server-id on A after the link came back: $(jq -c '[.data.checks[] | select(.id == "server-id")]' <<< "$doc")"
  reach=$(jq -c '[.data.checks[]? | select(.id == "server-reach")][0] // {}' <<< "$doc")
  jq -e '(.status == "info" or .status == "ok") and (.summary | test("silent|skipped") | not) and (.summary | contains("0 note(s) queued"))' > /dev/null <<< "$reach" ||
    fail "doctor's server-reach on A after the link came back: $reach"
}

# offline-laptop: A's link cut, its hooks stay within their deadlines,
# doctor and the banner say the server is silent, and once the link is
# back everything is sent once.
vb_check_offline() {
  local w=$M6_WORK/offline slow ev n back want msg reach reach1 notes=() tick1
  vb_offline_run laptop
  slow=$(vb_hooks_in_time "$w/hooks.jsonl") || exit 1
  [ "$(grep -c . "$w/hooks.jsonl")" = 13 ] || fail "$(grep -c . "$w/hooks.jsonl") hooks ran, want 13 (2 startups, 10 prompts, the end)"
  ev=$(vb_telemetry_since "$(cat "$w/since")") || exit 1
  # Positive control: the telemetry read is the hooks' own (each prompt is one event).
  n=$(jq -s '[.[] | select(.event == "prompt" and .session == "of1")] | length' <<< "$ev")
  [ "$n" = 10 ] || fail "A's telemetry holds $n prompt events of of1 since the start, want 10: the timeout look would prove nothing"
  n=$(jq -s '[.[] | select(.event == "hook_failure" and .kind == "timeout")] | length' <<< "$ev")
  [ "$n" = 0 ] || fail "A's telemetry holds $n hook_failure timeout(s) since the start"
  back=$(cat "$w/backdated")
  jq -e '(.failing_since // "") != ""' "$w/reach-before.json" > /dev/null || fail "reach.json recorded no failure while the server was silent: $(cat "$w/reach-before.json")"
  want="memory server silent since $(date -u -d "$back" '+%Y-%m-%d %H:%M') UTC"
  msg=$(jq -r '.systemMessage // ""' "$w/out/start2" 2> /dev/null || true)
  case $msg in *"$want"*) ;; *) fail "the second startup's banner lacks '$want': $msg" ;; esac
  reach=$(jq -c '[.data.checks[]? | select(.id == "server-reach")][0] // {}' "$w/doctor2.json")
  jq -e '.status == "warn" and (.summary | contains("2 note(s) queued"))' > /dev/null <<< "$reach" || fail "doctor's server-reach with reach.json 2 h silent: $reach, want warn naming 2 queued notes"
  mapfile -t notes < <(vb_offline_notes "$w" silent)
  [ "${#notes[@]}" = 2 ] || exit 1
  tick1=$(jq -c '.data | {status, ms, sessions}' "$w/tick1.json") || fail "the offline tick answered no JSON"
  reach1=$(jq -c '[.data.checks[]? | select(.id == "server-reach")][0] // {}' "$w/doctor1.json") || fail "the first offline doctor answered no JSON"
  # The link is back: one tick sends the session and both notes, once.
  m6_tick a > /dev/null
  vb_offline_sent of1 "${notes[@]}"
  m6_result offline-laptop PASS "link down ${M6_OFFLINE_S:-360} s, the offline script done in $(cat "$w/done") s; 13 hooks within their deadlines (slowest ms: $(vb_slowest "$slow")), no hook timeout; banner and doctor: silent since $back, 2 notes queued; back up: of1 and both notes on the server once, outbox empty, server-reach ok" \
    "$(jq -cn --argjson slow "$slow" --argjson t "$tick1" --arg back "$back" --argjson down "$(cat "$w/down-after")" --argjson r1 "$reach1" '{slowest_ms: $slow, offline_tick: $t, backdated: $back, health_failed_after_s: $down, doctor_reach_offline: $r1}')"
}

# offline-hung: A's link up, but the server never answers (a DROP rule):
# every hook still within its deadline, no hook timeout; at least one
# prompt really tried the server, and failed, before the breaker opened;
# once the rule is lifted, everything is sent once.
vb_check_hung() {
  local w=$M6_WORK/offline-hung slow ev n tried first open notes=() how steps
  vb_offline_run hung
  slow=$(vb_hooks_in_time "$w/hooks.jsonl") || exit 1
  [ "$(grep -c . "$w/hooks.jsonl")" = 12 ] || fail "$(grep -c . "$w/hooks.jsonl") hooks ran, want 12 (the startup, 10 prompts, the end)"
  ev=$(vb_telemetry_since "$(cat "$w/since")") || exit 1
  n=$(jq -s '[.[] | select(.event == "prompt" and .session == "oh1")] | length' <<< "$ev")
  [ "$n" = 10 ] || fail "A's telemetry holds $n prompt events of oh1 since the start, want 10: the timeout look would prove nothing"
  n=$(jq -s '[.[] | select(.event == "hook_failure" and .kind == "timeout")] | length' <<< "$ev")
  [ "$n" = 0 ] || fail "A's telemetry holds $n hook_failure timeout(s) since the start"
  # Positive control: the deadlines were held against the hung server, not
  # only behind an open breaker.
  tried=$(jq -c -s '[.[] | select(.event == "prompt" and .session == "oh1" and .decision == "recall")]' <<< "$ev")
  [ "$(jq length <<< "$tried")" -ge 1 ] || fail "no prompt of oh1 tried the server (reasons $(jq -c -s '[.[] | select(.event == "prompt" and .session == "oh1") | .reason]' <<< "$ev")): the deadlines were never met against a hung server"
  jq -e 'all(.[]; (.scopes // {}) | length > 0 and all(.[]; .ok == false))' > /dev/null <<< "$tried" ||
    fail "a recall of oh1 reached the server: the drop did not hold ($tried)"
  first=$(jq -r -s '[.[] | select(.event == "prompt" and .session == "oh1" and .reason == "breaker") | .n] | min // "none"' <<< "$ev")
  open=$(jq -r '.open_until // ""' "$w/breaker-end.json")
  mapfile -t notes < <(vb_offline_notes "$w" hung)
  [ "${#notes[@]}" = 2 ] || exit 1
  steps=$(jq -c -s 'map({(.name): {ms, rc}}) | add // {}' "$w/steps.jsonl")
  # The rule is lifted: a tick sends the session (a flush waits for the
  # SessionEnd flush still holding it, if it does) and both notes, once.
  m6_tick a > /dev/null
  how=$(m6_flush_sent a oh1 oh1-r3) || exit 1
  vb_offline_sent oh1 "${notes[@]}"
  m6_result offline-hung PASS "drop to $M6_SERVER_IP (A's /health timed out), link up; 12 hooks within their deadlines (slowest ms: $(vb_slowest "$slow")), no hook timeout; $(jq length <<< "$tried") prompts tried the server and failed before the breaker opened (from prompt $first, open until ${open:-never}); 2 notes queued; drop lifted: oh1 (by $how) and both notes on the server once, outbox empty, server-reach ok" \
    "$(jq -cn --argjson slow "$slow" --argjson t "$(jq length <<< "$tried")" --arg first "$first" --arg open "$open" --argjson steps "$steps" --arg how "$how" --argjson w "$(cat "$w/waited")" \
      '{slowest_ms: $slow, recall_attempts: $t, breaker_from_prompt: ($first | tonumber? // null), breaker_open_until: $open, steps: $steps, sent_by: $how, breaker_wait_s: $w}')"
}

# unsent-cost (recorded): what the offline tick pays for 200 uncursored
# transcripts of 50 records (Unsent, reach.json's sessions_waiting), and
# BenchmarkUnsent on this host. Above 2 000 ms more, a block 5 follow-up.
vb_check_unsent() {
  local w=$M6_WORK/offline base unsent wb wu ns diff note=''
  vb_offline_run laptop
  base=$(jq -er '.data | select(.status != "skipped") | .ms' "$w/tick-base.json") || fail "the offline tick without the transcripts measured nothing: $(cat "$w/tick-base.json")"
  unsent=$(jq -er '.data | select(.status != "skipped") | .ms' "$w/tick-unsent.json") || fail "the offline tick with the transcripts measured nothing: $(cat "$w/tick-unsent.json")"
  wb=$(jq -r '.sessions_waiting // 0' "$w/reach-base.json") wu=$(jq -r '.sessions_waiting // 0' "$w/reach-unsent.json")
  # Positive control: the tick counted the 200 transcripts.
  [ "$(cat "$w/unsent-files")" = 200 ] && [ "$wu" -ge $((wb + 200)) ] ||
    fail "the tick with $(cat "$w/unsent-files") transcripts counted $wu sessions waiting, $wb before: it did not survey them"
  ns=$(cd "$M6_ROOT" && go test ./internal/retain/ -run '^$' -bench '^BenchmarkUnsent$' -benchtime 3x 2>&1 | awk '$1 ~ /^BenchmarkUnsent/ { print $3 }' || true)
  [ -n "$ns" ] || fail "BenchmarkUnsent reported nothing"
  diff=$((unsent - base))
  if [ "$diff" -gt 2000 ]; then note="; above 2 000 ms: block 5 follow-up (cache Unsent by path, size and mtime)"; fi
  m6_result unsent-cost RECORDED "offline tick $base ms, $unsent ms with 200 uncursored transcripts of 50 records (+$diff ms, waiting $wb -> $wu); BenchmarkUnsent on the host $ns ns/op$note" \
    "$(jq -cn --argjson b "$base" --argjson u "$unsent" --argjson d "$diff" --argjson ns "$ns" --argjson wb "$wb" --argjson wu "$wu" \
      '{tick_ms: $b, tick_unsent_ms: $u, diff_ms: $d, benchmark_ns_op: $ns, sessions_waiting: {before: $wb, with_unsent: $wu}, follow_up: ($d > 2000)}')"
}

# vb_a_iface: A's interface towards the server.
vb_a_iface() {
  local i
  i=$(m6_sh a "ip -o route get $(m6_q "$M6_SERVER_IP")" | sed -n 's/.* dev \([^ ]*\).*/\1/p')
  [[ $i =~ ^[a-z0-9]+$ ]] || fail "cannot tell A's interface to $M6_SERVER_IP ('$i')"
  echo "$i"
}

# vb_no_netem: no netem qdisc left on A (its root qdisc back to the
# default); fails when one stays.
vb_no_netem() {
  vb_ssh bs-m6-client 'tc qdisc show | awk '\''$2 == "netem" { print $5 }'\'' | while read -r i; do sudo tc qdisc del dev "$i" root; done; ! tc qdisc show | grep -q netem' < /dev/null
}

# vb_check_latency <ms>: netem delays A's packets by ms (0: none, removed
# whatever way the check ends, with the latency files on A); a startup and
# 40 prompt hooks of session lt<ms>. Recorded: p50 and p95 of the recall
# prompts' wall time and telemetry prompt.ms; FAIL only for a hook past its
# deadline, a hook timeout, or too few recalls to measure (20).
vb_check_latency() {
  local d=$1 name=latency-$1ms iface qd rtt out slow r h stats
  h=$(m6_home a)
  iface=$(vb_a_iface) || exit 1
  trap 'vb_no_netem > /dev/null 2>&1 || true; m6_sh a "rm -rf ~/m6-latency ~/m6-latency.sh" > /dev/null 2>&1 || true' EXIT
  vb_no_netem > /dev/null || fail "cannot clear A's netem"
  if [ "$d" -gt 0 ]; then
    vb_ssh bs-m6-client "sudo tc qdisc replace dev $iface root netem delay ${d}ms" < /dev/null || fail "cannot set netem delay ${d}ms on A's $iface"
  fi
  # Positive control: the delay is there, and the packets feel it.
  qd=$(vb_ssh bs-m6-client "tc qdisc show dev $iface" < /dev/null) || fail "cannot read A's qdisc"
  if [ "$d" -gt 0 ]; then
    case $qd in *"netem"*"delay ${d}ms"*) ;; *) fail "netem delay ${d}ms is not on A's $iface: $qd" ;; esac
  else
    case $qd in *netem*) fail "a netem qdisc is on A's $iface with no delay asked: $qd" ;; esac
  fi
  rtt=$(m6_sh a "ping -n -c 11 -i 0.2 $(m6_q "$M6_SERVER_IP") || true" | sed -n 's/.*time=\([0-9.]*\) ms.*/\1/p' | sort -n | awk '{ v[NR] = $1 } END { if (NR) print v[int((NR + 1) / 2)] }')
  [ -n "$rtt" ] || fail "no ping reply from the server: the delay could not be measured"
  awk -v r="$rtt" -v d="$d" 'BEGIN { exit !(r >= d) }' || fail "median RTT to the server $rtt ms, below the ${d} ms netem delay: netem did not apply"
  m6_sh a "rm -rf ~/m6-latency && mkdir -p ~/m6-latency" || fail "cannot make ~/m6-latency on A"
  vb_latency_script | m6_put a "$h/m6-latency/run.sh" || fail "cannot write the latency script on A"
  out=$(m6_sh a "setsid -w bash $(m6_q "$h/m6-latency/run.sh" "$d" "$VB_LATENCY_PROJECT" "${M6_LATENCY_S:-1500}" "${VB_PROMPTS[@]}")") ||
    fail "the latency hooks on A failed: $(printf '%s' "$out" | tail -n 2)"
  out=$(m6_json "the latency hooks on A" "$out") || exit 1
  jq -c '.hooks[]' <<< "$out" > "$M6_WORK/$name.hooks"
  slow=$(vb_hooks_in_time "$M6_WORK/$name.hooks") || exit 1
  [ "$(grep -c . "$M6_WORK/$name.hooks")" = 41 ] || fail "$(grep -c . "$M6_WORK/$name.hooks") hooks ran, want 41"
  [ "$(jq '.failures | length' <<< "$out")" = 0 ] || fail "hook timeouts in A's telemetry: $(jq -c .failures <<< "$out")"
  r=$(jq '[.prompts[] | select(.decision == "recall")] | length' <<< "$out")
  [ "$r" -ge 20 ] || fail "$r of 40 prompts recalled, want 20 or more: nothing measured ($(jq -c '[.prompts[] | .reason] | group_by(.) | map({(.[0]): length}) | add' <<< "$out"))"
  vb_no_netem > /dev/null || fail "netem stayed on A"
  stats=$(jq -c '
    def pct($p): sort | .[((length * $p | ceil) - 1) | if . < 0 then 0 else . end];
    ([.prompts[] | select(.decision == "recall") | .n]) as $ns
    | ([.hooks[] | select(.name | test("^p[0-9]+$")) | select((.name[1:] | tonumber) as $i | $ns | index($i) != null) | .ms]) as $wall
    | ([.prompts[] | select(.decision == "recall") | .ms]) as $tms
    | {recall: ($ns | length), wall_p50: ($wall | pct(0.5)), wall_p95: ($wall | pct(0.95)), prompt_ms_p50: ($tms | pct(0.5)), prompt_ms_p95: ($tms | pct(0.95)),
       startup_ms: ([.hooks[] | select(.name == "start") | .ms][0]), breaker_wait_s: .waited_s}' <<< "$out")
  m6_result "$name" PASS "netem ${d} ms (median RTT $rtt ms); 41 hooks within their deadlines, no hook timeout; $(jq -r .recall <<< "$stats") recall prompts of 40: wall p50 $(jq -r .wall_p50 <<< "$stats") p95 $(jq -r .wall_p95 <<< "$stats") ms, prompt.ms p50 $(jq -r .prompt_ms_p50 <<< "$stats") p95 $(jq -r .prompt_ms_p95 <<< "$stats") ms; startup $(jq -r .startup_ms <<< "$stats") ms" \
    "$(jq -cn --argjson s "$stats" --argjson d "$d" --arg rtt "$rtt" --argjson slow "$slow" '$s + {delay_ms: $d, rtt_median_ms: ($rtt | tonumber), slowest_ms: $slow}')"
}

m6_check_offline_laptop() { vb_check_offline; }
m6_check_offline_hung() { vb_check_hung; }
m6_check_unsent_cost() { vb_check_unsent; }
m6_check_latency_0ms() { vb_check_latency 0; }
m6_check_latency_20ms() { vb_check_latency 20; }
m6_check_latency_80ms() { vb_check_latency 80; }

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
  # The snapshot holds the build of up's day: every run tests this tree,
  # built again (B's binary is VB_DIR/bin's) and given to A with its
  # release.
  vb_say "building backsight $VB_VERSION (testkey) and its release from this tree"
  vb_build
  vb_push_a
  vb_b_claude
  # shellcheck source=scripts/m6-proofs.sh
  . "$VB_ROOT/scripts/m6-proofs.sh"
  stamp=$(date -u +%Y%m%dT%H%M%SZ)
  M6_RESULTS=$VB_DIR/results/$stamp.jsonl M6_WORK=$VB_DIR/work/run M6_ROOT=$VB_ROOT M6_VERSION=$VB_VERSION
  M6_COMMIT=$(git -C "$VB_ROOT" describe --always --dirty --abbrev=7 2> /dev/null || echo unknown)
  M6_SERVER_IP=$(awk '$1 == "HostName" { print $2; exit }' "$VB_DIR/ssh_config")
  : > "$M6_RESULTS"
  vb_authorize_a
  trap vb_run_end EXIT
  for c in "${order[@]}"; do
    m6_check "$c" "m6_check_${c//-/_}" < /dev/null
  done
  vb_results_table "$M6_RESULTS" | tee "$VB_DIR/results/$stamp.md"
  failed=$(jq -s '[.[] | select(.status == "FAIL")] | length' "$M6_RESULTS")
  [ "$failed" = 0 ]
}

fail() { vb_fail "$@"; }

vb_usage() {
  sed -n '6,14p' "${BASH_SOURCE[0]}" | sed 's/^# //' >&2
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
