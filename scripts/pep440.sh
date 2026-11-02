# shellcheck shell=bash
# pep440_cmp, sourced by scripts/native-lock.sh (tested in
# scripts/native_lock_test.go, TestPEP440Cmp).

# pep440_cmp a b: prints -1, 0 or 1 as version a is lower than, equal to or
# higher than b in PEP 440's order: a pre-release (aN, bN, rcN) before its
# release, X.devN before everything else of X, X.postN after X; release
# segments compared as numbers, missing ones as 0 (1.0 == 1.0.0). It reads
# the normalised public forms PyPI's metadata uses,
# N(.N)*[{a|b|rc}N][.postN][.devN], and fails (status 2) on any other (an
# epoch, a local version, an unnormalised spelling): compare those by hand.
# Plain POSIX awk (mawk, gawk, busybox).
pep440_cmp() {
  awk -v a="$1" -v b="$2" '
    # parse: R[1..R[0]] the release segments; K the rest as numbers that
    # compare in order: K[1] the phase (0 a .dev release, 1 a, 2 b, 3 rc,
    # 4 the release itself or its .post), K[2] its number, K[3] post (-1
    # none), K[4] dev (1e18 none, so a .dev sorts first).
    function parse(v, R, K,   m, rel, n) {
      if (v !~ /^[0-9]+(\.[0-9]+)*((a|b|rc)[0-9]+)?(\.post[0-9]+)?(\.dev[0-9]+)?$/) return 0
      match(v, /^[0-9]+(\.[0-9]+)*/)
      rel = substr(v, 1, RLENGTH); v = substr(v, RLENGTH + 1)
      n = split(rel, R, "."); R[0] = n
      K[1] = 4; K[2] = 0; K[3] = -1; K[4] = 1e18
      if (match(v, /^(a|b|rc)[0-9]+/)) {
        m = substr(v, 1, RLENGTH); v = substr(v, RLENGTH + 1)
        K[1] = (m ~ /^rc/) ? 3 : (m ~ /^b/) ? 2 : 1
        sub(/^[a-z]+/, "", m); K[2] = m + 0
      }
      if (match(v, /^\.post[0-9]+/)) { K[3] = substr(v, 6, RLENGTH - 5) + 0; v = substr(v, RLENGTH + 1) }
      if (match(v, /^\.dev[0-9]+/)) K[4] = substr(v, 5, RLENGTH - 4) + 0
      if (K[1] == 4 && K[3] == -1 && K[4] != 1e18) K[1] = 0 # X.devN, before X.a1
      return 1
    }
    BEGIN {
      if (!parse(a, RA, KA) || !parse(b, RB, KB)) exit 2
      n = RA[0] > RB[0] ? RA[0] : RB[0]
      for (i = 1; i <= n; i++) {
        x = (i <= RA[0]) ? RA[i] + 0 : 0; y = (i <= RB[0]) ? RB[i] + 0 : 0
        if (x != y) { print (x < y ? -1 : 1); exit 0 }
      }
      for (i = 1; i <= 4; i++) if (KA[i] != KB[i]) { print (KA[i] < KB[i] ? -1 : 1); exit 0 }
      print 0
    }'
}
